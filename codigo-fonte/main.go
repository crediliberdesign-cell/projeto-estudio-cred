package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

//go:embed web media remotion skills
var bundled embed.FS

type SetupStatus struct {
	Stage    string         `json:"stage"`
	Progress float64        `json:"progress"`
	Message  string         `json:"message"`
	Status   string         `json:"status"`
	Error    string         `json:"error,omitempty"`
	Events   []InstallEvent `json:"events"`
}
type App struct {
	store         *Store
	root          string
	token         string
	origin        string
	ctx           context.Context
	runtime       RuntimeConfig
	embeddingURL  string
	runtimeMu     sync.RWMutex
	installMu     sync.Mutex
	setup         SetupStatus
	installCancel context.CancelFunc
	runtimeCancel context.CancelFunc
	mediaMu       sync.Mutex
	chatMu        sync.Mutex
	jobMu         sync.Mutex
	cancels       map[string]context.CancelFunc
	docMu         sync.Mutex
	srv           *http.Server
	stop          context.CancelFunc
}

func defaultRoot() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "AtelieLocal")
		}
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "AtelieLocal")
}
func main() {
	rootFlag := flag.String("data-dir", "", "Pasta local de dados")
	noBrowser := flag.Bool("no-browser", false, "Não abrir navegador")
	uninstall := flag.Bool("uninstall", false, "Remover componentes preservando os dados")
	port := flag.Int("port", 8765, "Porta local (padrão 8765, 0 = automática)")
	flag.Parse()
	root := *rootFlag
	if root == "" {
		root = defaultRoot()
		if b, e := os.ReadFile(filepath.Join(root, "location.json")); e == nil {
			var x struct{ Root string }
			if json.Unmarshal(b, &x) == nil && x.Root != "" {
				root = x.Root
			}
		}
	}
	abs, e := filepath.Abs(root)
	if e != nil {
		startupFatal(e)
	}
	root = abs
	if *uninstall {
		if e := RunUninstall(root); e != nil {
			startupFatal(e)
		}
		return
	}
	if b, e := os.ReadFile(filepath.Join(root, "session.json")); e == nil {
		var ses map[string]string
		_ = json.Unmarshal(b, &ses)
		if u, e := url.Parse(ses["url"]); e == nil && u.Hostname() == "127.0.0.1" && ses["token"] != "" {
			client := http.Client{Timeout: 3 * time.Second}
			req, _ := http.NewRequest("GET", ses["url"]+"/api/ping", nil)
			req.Header.Set("Authorization", "Bearer "+ses["token"])
			if resp, e := client.Do(req); e == nil {
				defer resp.Body.Close()
				if resp.StatusCode == 200 {
					fmt.Printf("\n[Estúdio Local] Aplicativo já está em execução em %s\nAbrindo navegador...\n\n", ses["url"])
					if !*noBrowser {
						_ = openExternal(ses["url"] + "/launch?token=" + ses["token"])
					}
					return
				}
			}
		}
	}
	s, e := openStore(root)
	if e != nil {
		startupFatal(e)
	}
	if cr, ce := canonical(root); ce == nil {
		root = cr
		s.root = cr
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	app := &App{store: s, root: root, ctx: ctx, stop: cancel, token: newID() + newID(), cancels: map[string]context.CancelFunc{}, setup: SetupStatus{Status: "idle", Stage: "welcome", Events: []InstallEvent{}}}
	if e = app.extractAssets(); e != nil {
		startupFatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(root, "runtime.json")); e == nil {
		_ = json.Unmarshal(b, &app.runtime)
	}
	s.mu.Lock()
	for id, j := range s.db.Jobs {
		if j.Status == "running" || j.Status == "queued" {
			j.Status = "interrupted"
			j.Stage = "Interrompido ao fechar o aplicativo. Use Tentar novamente."
			s.db.Jobs[id] = j
		}
	}
	_ = s.saveLocked()
	installed := s.db.Installed
	s.mu.Unlock()
	if installed {
		go func() {
			if e := app.startRuntime(); e != nil {
				log.Print("Runtime: ", e)
			}
		}()
	}
	var ln net.Listener
	if *port != 0 {
		ln, e = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
		if e != nil {
			log.Printf("Porta %d ocupada (%v); buscando porta automática...", *port, e)
			ln, e = net.Listen("tcp4", "127.0.0.1:0")
		}
	} else {
		ln, e = net.Listen("tcp4", "127.0.0.1:0")
	}
	if e != nil {
		startupFatal(e)
	}
	app.origin = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	app.routes(mux)
	app.srv = &http.Server{Handler: app.security(mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Minute}
	_ = atomicJSON(filepath.Join(root, "session.json"), map[string]string{"url": app.origin, "token": app.token})
	defer os.Remove(filepath.Join(root, "session.json"))

	fmt.Println()
	fmt.Println("==================================================================")
	fmt.Println("             ESTÚDIO LOCAL — ASSISTENTE DE VÍDEO                  ")
	fmt.Println("==================================================================")
	fmt.Printf(" [STATUS] Servidor ativo em: %s\n", app.origin)
	fmt.Printf(" [ACESSO] %s/launch?token=%s\n", app.origin, app.token)
	fmt.Printf(" [DADOS]  %s\n", root)
	fmt.Println("------------------------------------------------------------------")
	fmt.Println(" * Mantenha este terminal aberto para manter o estúdio funcionando.")
	fmt.Println(" * Para encerrar, pressione Ctrl+C ou feche esta janela.")
	fmt.Println("==================================================================")
	fmt.Println()

	if !*noBrowser {
		_ = openExternal(app.origin + "/launch?token=" + app.token)
	}
	go func() {
		<-ctx.Done()
		app.installMu.Lock()
		if app.installCancel != nil {
			app.installCancel()
		}
		app.installMu.Unlock()
		app.runtimeMu.Lock()
		if app.runtimeCancel != nil {
			app.runtimeCancel()
		}
		app.runtimeMu.Unlock()
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = app.srv.Shutdown(c)
	}()
	if e = app.srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
		startupFatal(e)
	}
}
func (a *App) extractAssets() error {
	// Keep application temporary/download staging on the volume checked by setup.
	// These variables affect this process and its children, never Windows settings.
	tmp := filepath.Join(a.root, "cache", "temp")
	if e := os.MkdirAll(tmp, 0700); e != nil {
		return e
	}
	for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
		_ = os.Setenv(key, tmp)
	}
	return fs.WalkDir(bundled, ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || strings.HasPrefix(p, "web/") {
			return nil
		}
		b, e := bundled.ReadFile(p)
		if e != nil {
			return e
		}
		dest := filepath.Join(a.root, "assets", filepath.FromSlash(p))
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		return os.WriteFile(dest, b, 0600)
	})
}
func (a *App) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; font-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		if r.Host != strings.TrimPrefix(a.origin, "http://") {
			http.Error(w, "Host inválido", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != a.origin {
			http.Error(w, "Origem não autorizada", 403)
			return
		}
		if r.URL.Path == "/launch" {
			tok := r.URL.Query().Get("token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
				http.Error(w, "Sessão inválida", 403)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "atelie_session", Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, e := r.Cookie("atelie_session"); e == nil && tok == "" {
			tok = c.Value
		}
		if tok == "" && r.URL.Query().Get("token") != "" {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
			http.Error(w, "Abra o Ateliê pelo atalho do aplicativo para iniciar uma sessão.", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) routes(m *http.ServeMux) {
	m.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"ok": true, "version": appVersion})
	})
	m.HandleFunc("/api/state", a.state)
	m.HandleFunc("/api/companies", a.companies)
	m.HandleFunc("/api/companies/", a.companyAction)
	m.HandleFunc("/api/company-bundle", a.companyBundle)
	m.HandleFunc("/api/company-bundle/import", a.importCompanyBundle)
	m.HandleFunc("/api/projects", a.projects)
	m.HandleFunc("/api/projects/", a.projectAction)
	m.HandleFunc("/api/upload", a.upload)
	m.HandleFunc("/api/assets/", a.assetAction)
	m.HandleFunc("/api/memories", a.memories)
	m.HandleFunc("/api/memories/", a.memoryAction)
	m.HandleFunc("/api/settings", a.settings)
	m.HandleFunc("/api/pick", a.pick)
	m.HandleFunc("/api/setup", a.setupStart)
	m.HandleFunc("/api/setup/status", a.setupGet)
	m.HandleFunc("/api/setup/cancel", a.setupCancel)
	m.HandleFunc("/api/jobs", a.jobs)
	m.HandleFunc("/api/jobs/", a.jobAction)
	m.HandleFunc("/api/chat", a.chat)
	m.HandleFunc("/api/search", a.search)
	m.HandleFunc("/api/open", a.openPath)
	m.HandleFunc("/api/file", a.outputFile)
	m.HandleFunc("/api/diagnostics", a.diagnostics)
	m.HandleFunc("/api/player/", a.player)
	m.HandleFunc("/api/job-assets/", a.jobAsset)
	m.HandleFunc("/api/backup", a.backup)
	m.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			fail(w, 405, "Use POST")
			return
		}
		reply(w, map[string]bool{"ok": true})
		go func() { time.Sleep(100 * time.Millisecond); a.stop() }()
	})
	web, _ := fs.Sub(bundled, "web")
	m.Handle("/", http.FileServer(http.FS(web)))
}
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func body(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20)).Decode(v)
}
func powershell(script string, env ...string) ([]byte, error) {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-STA", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), env...)
	HideManagedProcessWindow(cmd)
	return cmd.Output()
}
func powershellDialog(script string, env ...string) ([]byte, error) {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-STA", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), env...)
	return cmd.Output()
}
func openExternal(target string) error {
	switch runtime.GOOS {
	case "windows":
		_, e := powershell(`Start-Process -FilePath $env:ATELIE_TARGET`, "ATELIE_TARGET="+target)
		return e
	case "darwin":
		return exec.Command("open", target).Run()
	default:
		return exec.Command("xdg-open", target).Run()
	}
}
func (a *App) startRuntime() error {
	a.runtimeMu.Lock()
	if a.runtimeCancel != nil {
		a.runtimeCancel()
	}
	cfg := a.runtime
	cfg.OllamaURL = ""
	a.runtime.OllamaURL = ""
	a.embeddingURL = ""
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeCancel = cancel
	a.runtimeMu.Unlock()
	cmd, e := StartOllama(ctx, &cfg)
	if e != nil {
		cancel()
		return e
	}
	go cmd.Wait()
	a.runtimeMu.Lock()
	a.runtime = cfg
	a.runtimeMu.Unlock()
	if e = atomicJSON(filepath.Join(a.root, "runtime.json"), cfg); e != nil {
		return e
	}
	go func() {
		embedCfg := cfg
		cmd, e := StartOllama(ctx, &embedCfg)
		if e != nil {
			log.Print("Busca semântica indisponível; busca textual ativa: ", e)
			return
		}
		a.runtimeMu.Lock()
		a.embeddingURL = embedCfg.OllamaURL
		a.runtimeMu.Unlock()
		go cmd.Wait()
		a.indexPending()
	}()
	return nil
}
func (a *App) runtimeConfig() RuntimeConfig {
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	return a.runtime
}

func startupFatal(err error) {
	log.Print(err)
	if runtime.GOOS == "windows" {
		_, _ = powershell(`Add-Type -AssemblyName PresentationFramework; [System.Windows.MessageBox]::Show($env:ATELIE_ERROR,'Estúdio Local')`, "ATELIE_ERROR="+err.Error())
	}
	os.Exit(1)
}
