package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func canonical(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("Caminho vazio")
	}
	abs, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(abs)
}
func within(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}
func (a *App) allowedRoots(p Project) []string {
	roots := []string{filepath.Join(a.root, "companies", p.CompanyID), filepath.Join(a.root, "projects", p.ID)}
	if p.Scope == "computer" {
		if runtime.GOOS == "windows" {
			for d := 'C'; d <= 'Z'; d++ {
				v := fmt.Sprintf("%c:\\", d)
				if _, e := os.Stat(v); e == nil {
					roots = append(roots, v)
				}
			}
		} else {
			roots = append(roots, "/")
		}
	} else if p.Scope == "folder" {
		if p.Folder != "" {
			roots = append(roots, p.Folder)
		}
	} else if p.Scope == "company" || p.Scope == "" {
		if c, ok := a.store.company(p.CompanyID); ok {
			roots = append(roots, c.Folders...)
		}
	}
	existing := []string{}
	for _, root := range roots {
		if cr, e := canonical(root); e == nil {
			existing = append(existing, cr)
		}
	}
	return existing
}
func (a *App) authorizePath(p Project, path string) (string, error) {
	v, e := canonical(path)
	if e != nil {
		return "", fmt.Errorf("Arquivo/pasta inacessível: %w", e)
	}
	a.store.mu.RLock()
	for id, c := range a.store.db.Companies {
		if id == p.CompanyID || c.Shared {
			continue
		}
		for _, forbidden := range append([]string{filepath.Join(a.root, "companies", id)}, c.Folders...) {
			if cr, e := canonical(forbidden); e == nil && within(v, cr) {
				a.store.mu.RUnlock()
				return "", errors.New("Arquivo pertence a outra empresa")
			}
		}
	}
	for id, pr := range a.store.db.Projects {
		if pr.CompanyID != p.CompanyID {
			if cr, e := canonical(filepath.Join(a.root, "projects", id)); e == nil && within(v, cr) {
				a.store.mu.RUnlock()
				return "", errors.New("Projeto pertence a outra empresa")
			}
		}
	}
	a.store.mu.RUnlock()
	for _, root := range a.allowedRoots(p) {
		if cr, e := canonical(root); e == nil && within(v, cr) {
			return v, nil
		}
	}
	return "", errors.New("Arquivo fora do escopo deste projeto. Selecione a pasta ou importe o arquivo.")
}
func (a *App) jobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.mu.RLock()
		defer a.store.mu.RUnlock()
		out := []Job{}
		for _, j := range a.store.db.Jobs {
			if pid := r.URL.Query().Get("project_id"); pid != "" && pid != j.ProjectID {
				continue
			}
			out = append(out, j)
		}
		reply(w, out)
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	var v struct {
		ProjectID string         `json:"project_id"`
		Action    string         `json:"action"`
		Options   map[string]any `json:"options"`
	}
	if body(r, &v) != nil {
		fail(w, 400, "Trabalho inválido")
		return
	}
	job, e := a.submitJob(v.ProjectID, v.Action, v.Options)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	reply(w, job)
}
func (a *App) submitJob(pid, action string, opts map[string]any) (Job, error) {
	p, ok := a.store.project(pid)
	if !ok {
		return Job{}, errors.New("Selecione um projeto")
	}
	if !contains([]string{"probe", "frames", "transcribe", "plan_silence", "captions", "edit", "motion", "carousel", "diagnose"}, action) {
		return Job{}, errors.New("Operação não disponível")
	}
	cfg := a.runtimeConfig()
	if cfg.Python == "" {
		return Job{}, errors.New("Conclua a instalação das ferramentas antes de processar vídeos")
	}
	if !contains([]string{"motion", "carousel", "diagnose"}, action) && p.Source == "" {
		return Job{}, errors.New("Adicione um vídeo ou áudio ao projeto")
	}
	if p.Source != "" {
		if _, e := a.authorizePath(p, p.Source); e != nil {
			return Job{}, e
		}
	}
	if opts == nil {
		opts = map[string]any{}
	}
	if e := a.validatePaths(p, opts); e != nil {
		return Job{}, e
	}
	if contains([]string{"edit", "motion", "carousel"}, action) {
		if action == "motion" || action == "carousel" {
			if (p.Audio == nil || p.Audio == "") && opts["audio"] != nil {
				p.Audio = opts["audio"]
			} else if p.Audio == nil || p.Audio == "" {
				p.Audio = map[string]any{"mode": "none"}
			}
			if p.Aspect == "" && opts["aspect"] != nil && str(opts, "aspect") != "" {
				p.Aspect = str(opts, "aspect")
			} else if p.Aspect == "" {
				p.Aspect = "9:16"
			}
			if number(p.Duration) <= 0 && !(p.Duration == "source" && p.Source != "") {
				if opts["duration"] != nil && number(opts["duration"]) > 0 {
					p.Duration = opts["duration"]
				} else if scenes, ok := opts["scenes"].([]any); ok && len(scenes) > 0 {
					var totalDur float64
					for _, sc := range scenes {
						if sm, ok := sc.(map[string]any); ok {
							totalDur += number(sm["duration"])
						}
					}
					if totalDur > 0 {
						p.Duration = totalDur
					} else {
						p.Duration = 10
					}
				} else {
					p.Duration = 10
				}
			}
			a.store.mu.Lock()
			if curP, exists := a.store.db.Projects[pid]; exists {
				curP.Audio = p.Audio
				curP.Aspect = p.Aspect
				curP.Duration = p.Duration
				a.store.db.Projects[pid] = curP
			}
			a.store.mu.Unlock()
		}
		if p.Audio == nil || p.Audio == "" {
			return Job{}, errors.New("Escolha o áudio deste vídeo: original, sem áudio ou uma combinação")
		}
		if m, ok := p.Audio.(map[string]any); ok && str(m, "mode") == "" {
			return Job{}, errors.New("Escolha o áudio deste vídeo")
		}
		if p.Aspect == "" {
			return Job{}, errors.New("Escolha a proporção deste vídeo")
		}
		if action == "motion" && number(p.Duration) <= 0 && !(p.Duration == "source" && p.Source != "") {
			return Job{}, errors.New("Informe a duração deste motion em segundos")
		}
		if action == "motion" || action == "carousel" {
			a.store.mu.RLock()
			ack := a.store.db.Settings.RemotionLicenseAck
			a.store.mu.RUnlock()
			if !ack {
				return Job{}, errors.New("Revise os termos do Remotion na instalação")
			}
		}
	}
	id := newID()
	j := Job{ID: id, ProjectID: pid, CompanyID: p.CompanyID, Action: action, Status: "queued", Stage: "Na fila de processamento", Options: opts, OutputDir: filepath.Join(a.root, "projects", pid, "outputs", id), Created: now(), Updated: now()}
	a.store.mu.Lock()
	a.store.db.Jobs[id] = j
	e := a.store.saveLocked()
	a.store.mu.Unlock()
	if e != nil {
		return Job{}, e
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.jobMu.Lock()
	a.cancels[id] = cancel
	a.jobMu.Unlock()
	go a.runJob(ctx, j, p)
	return j, nil
}
func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		var f float64
		fmt.Sscan(x, &f)
		return f
	}
	return 0
}
func (a *App) validatePaths(p Project, m map[string]any) error {
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if e := a.validatePaths(p, sub); e != nil {
				return e
			}
		}
		if s, ok := v.(string); ok && s != "" && (strings.HasSuffix(k, "_path") || k == "input" || k == "source") {
			if _, e := a.authorizePath(p, s); e != nil {
				return e
			}
		}
	}
	return nil
}
func (a *App) updateJob(id string, fn func(*Job)) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	j, ok := a.store.db.Jobs[id]
	if !ok {
		return
	}
	fn(&j)
	j.Updated = now()
	a.store.db.Jobs[id] = j
	_ = a.store.saveLocked()
}
func (a *App) runJob(ctx context.Context, j Job, p Project) {
	defer func() { a.jobMu.Lock(); delete(a.cancels, j.ID); a.jobMu.Unlock() }()
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if ctx.Err() != nil {
		a.updateJob(j.ID, func(x *Job) { x.Status = "cancelled"; x.Stage = "Cancelado" })
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	_ = os.MkdirAll(j.OutputDir, 0700)
	a.updateJob(j.ID, func(x *Job) { x.Status = "running"; x.Stage = "Preparando processamento" })
	cfg := a.runtimeConfig()
	if j.Action == "transcribe" || j.Action == "edit" || j.Action == "captions" {
		a.updateJob(j.ID, func(x *Job) { x.Stage = "Aguardando o modelo liberar recursos" })
		if !lockWithContext(ctx, &a.chatMu) {
			a.jobError(j.ID, ctx.Err(), ctx)
			return
		}
		defer a.chatMu.Unlock()
	}
	if j.Action == "transcribe" || j.Action == "edit" || j.Action == "captions" {
		a.unloadModels(ctx)
	}
	request := map[string]any{"action": j.Action, "input": p.Source, "output_dir": j.OutputDir, "ffmpeg": cfg.FFmpeg, "ffprobe": cfg.FFprobe, "node": cfg.Node, "remotion_dir": cfg.RemotionDir, "whisper_model_path": cfg.WhisperModelPath, "project": p, "options": j.Options, "allowed_roots": a.allowedRoots(p), "cancel_file": filepath.Join(j.OutputDir, "cancel")}
	if aud, ok := p.Audio.(map[string]any); ok {
		if e := a.validatePaths(p, aud); e != nil {
			a.updateJob(j.ID, func(x *Job) { x.Status = "failed"; x.Error = e.Error() })
			return
		}
	}
	req := filepath.Join(j.OutputDir, "request.json")
	if e := atomicJSON(req, request); e != nil {
		a.updateJob(j.ID, func(x *Job) { x.Status = "failed"; x.Error = e.Error() })
		return
	}
	cmd := exec.CommandContext(ctx, cfg.Python, filepath.Join(a.root, "assets", "media", "worker.py"), "--request", req)
	HideManagedProcessWindow(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "PATH="+filepath.Dir(cfg.Node)+string(os.PathListSeparator)+filepath.Dir(cfg.FFmpeg)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, e := cmd.StdoutPipe()
	if e != nil {
		a.jobError(j.ID, e, ctx)
		return
	}
	errfile, _ := os.Create(filepath.Join(j.OutputDir, "worker.log"))
	if errfile != nil {
		defer errfile.Close()
		cmd.Stderr = errfile
	}
	if e = cmd.Start(); e != nil {
		a.jobError(j.ID, e, ctx)
		return
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = os.WriteFile(filepath.Join(j.OutputDir, "cancel"), []byte("cancel"), 0600)
			if runtime.GOOS == "windows" && cmd.Process != nil {
				_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
			}
		case <-done:
		}
	}()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var result map[string]any
	var workerErr string
	for scanner.Scan() {
		var ev map[string]any
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch str(ev, "type") {
		case "progress":
			a.updateJob(j.ID, func(x *Job) { x.Stage = str(ev, "stage"); x.Progress = number(ev["progress"]) })
		case "result":
			if v, ok := ev["result"].(map[string]any); ok {
				result = v
			} else {
				result = ev
			}
		case "error":
			workerErr = str(ev, "message")
		}
	}
	waitErr := cmd.Wait()
	close(done)
	if ctx.Err() != nil {
		a.jobError(j.ID, ctx.Err(), ctx)
		return
	}
	if workerErr != "" {
		a.jobError(j.ID, errors.New(workerErr), ctx)
		return
	}
	if waitErr != nil {
		a.jobError(j.ID, fmt.Errorf("Processamento falhou. Consulte o diagnóstico deste trabalho: %w", waitErr), ctx)
		return
	}
	if result == nil {
		a.jobError(j.ID, errors.New("A ferramenta terminou sem um resultado verificável"), ctx)
		return
	}
	result["output_dir"] = j.OutputDir
	a.updateJob(j.ID, func(x *Job) { x.Status = "completed"; x.Stage = "Concluído"; x.Progress = 1; x.Result = result })
	a.store.mu.Lock()
	pr := a.store.db.Projects[p.ID]
	pr.Revisions = append(pr.Revisions, map[string]any{"job_id": j.ID, "action": j.Action, "created": now(), "result": result, "brand": p.Brand})
	pr.Updated = now()
	a.store.db.Projects[p.ID] = pr
	_ = a.store.saveLocked()
	a.store.mu.Unlock()
	a.rememberProject(p.ID, "Processamento concluído: "+j.Action+". Saída: "+j.OutputDir, "job:"+j.ID)
}
func (a *App) jobError(id string, e error, ctx context.Context) {
	a.updateJob(id, func(j *Job) {
		j.Status = "failed"
		j.Stage = "Falha"
		j.Error = e.Error()
		if ctx.Err() != nil {
			j.Status = "cancelled"
			j.Stage = "Cancelado"
		}
	})
}
func (a *App) jobAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
	id := parts[0]
	a.store.mu.RLock()
	j, ok := a.store.db.Jobs[id]
	a.store.mu.RUnlock()
	if !ok {
		fail(w, 404, "Trabalho não encontrado")
		return
	}
	if len(parts) > 1 && parts[1] == "cancel" && r.Method == "POST" {
		a.jobMu.Lock()
		if c := a.cancels[id]; c != nil {
			c()
		}
		a.jobMu.Unlock()
		reply(w, map[string]bool{"ok": true})
		return
	}
	reply(w, j)
}
func (a *App) rememberProject(pid, text, source string) {
	if !a.store.settings().MemoryEnabled {
		return
	}
	p, ok := a.store.project(pid)
	if !ok {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, m := range a.store.db.Memories {
		if m.ProjectID == pid && m.Text == text {
			return
		}
	}
	m := Memory{ID: newID(), CompanyID: p.CompanyID, ProjectID: pid, Scope: "project", Text: text, Source: source, Created: now(), Updated: now()}
	a.store.db.Memories[m.ID] = m
	_ = a.store.saveLocked()
}
func (a *App) unloadModels(ctx context.Context) {
	s := a.store.settings()
	for _, model := range []string{s.ModelQuick, s.ModelAgent} {
		if model != "" {
			var out any
			_ = a.ollamaJSON(ctx, "/api/generate", map[string]any{"model": model, "keep_alive": 0}, &out)
		}
	}
}

var _ io.Reader
