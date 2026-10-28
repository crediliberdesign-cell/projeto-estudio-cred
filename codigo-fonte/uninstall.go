package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunUninstall is intentionally separate from ordinary startup. It removes only
// managed application components after consent and preserves all user data/models.
func RunUninstall(root string) error {
	if runtime.GOOS != "windows" {
		return errors.New("A desinstalação visual está disponível no Windows.")
	}
	realRoot, err := uninstallValidateRoot(root)
	if err != nil {
		return err
	}
	choice, err := powershell(`Add-Type -AssemblyName PresentationFramework; $text='Remover os componentes do Estúdio Local e o atalho?'+[Environment]::NewLine+[Environment]::NewLine+'Os trabalhos em andamento serão encerrados. Empresas, documentos, projetos, conversas, memórias, backups e modelos serão preservados em:'+[Environment]::NewLine+$env:ATELIE_UNINSTALL_ROOT; [Console]::Write([System.Windows.MessageBox]::Show($text,'Desinstalar Estúdio Local',[System.Windows.MessageBoxButton]::YesNo,[System.Windows.MessageBoxImage]::Question,[System.Windows.MessageBoxResult]::No))`, "ATELIE_UNINSTALL_ROOT="+realRoot)
	if err != nil {
		return fmt.Errorf("Não foi possível abrir a confirmação: %w", err)
	}
	if strings.TrimSpace(string(choice)) != "Yes" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = uninstallStopSession(ctx, realRoot); err != nil {
		return err
	}
	warnings, err := uninstallManagedComponents(realRoot)
	if err != nil {
		return err
	}
	// Resolve only the application's exact shortcut; never remove a similarly
	// named shortcut whose target/arguments belong to another installation.
	_, shortcutErr := powershell(`$ErrorActionPreference='Stop'; $p=Join-Path ([Environment]::GetFolderPath('Desktop')) 'Estúdio Local.lnk'; if(Test-Path -LiteralPath $p){$w=New-Object -ComObject WScript.Shell; $l=$w.CreateShortcut($p); $expected=Join-Path $env:ATELIE_UNINSTALL_ROOT 'EstudioLocal.exe'; $args='--data-dir "'+$env:ATELIE_UNINSTALL_ROOT+'"'; if([String]::Equals([IO.Path]::GetFullPath($l.TargetPath),[IO.Path]::GetFullPath($expected),[StringComparison]::OrdinalIgnoreCase) -and $l.Arguments -eq $args){Remove-Item -LiteralPath $p -Force}}`, "ATELIE_UNINSTALL_ROOT="+realRoot)
	if shortcutErr != nil {
		warnings = append(warnings, "O atalho não pôde ser removido; apague apenas o atalho Estúdio Local desta instalação.")
	}
	message := "Componentes removidos. Empresas, projetos, conversas, memórias e modelos foram preservados em:\n" + realRoot + "\n\nPara reinstalar, execute o instalador e escolha esta mesma pasta."
	if len(warnings) > 0 {
		message = "Desinstalação concluída parcialmente. Seus dados e modelos foram preservados em:\n" + realRoot + "\n\n" + strings.Join(warnings, "\n") + "\n\nFeche os programas indicados e execute o desinstalador novamente. Nenhuma exclusão foi agendada."
	}
	_, err = powershell(`Add-Type -AssemblyName PresentationFramework; [System.Windows.MessageBox]::Show($env:ATELIE_UNINSTALL_MESSAGE,'Estúdio Local')`, "ATELIE_UNINSTALL_MESSAGE="+message)
	return err
}

func uninstallValidateRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("Pasta da instalação não encontrada: %w", err)
	}
	if filepath.Dir(real) == real {
		return "", errors.New("A raiz de uma unidade não pode ser desinstalada.")
	}
	if home, _ := os.UserHomeDir(); strings.EqualFold(filepath.Clean(real), filepath.Clean(home)) {
		return "", errors.New("A pasta pessoal não pode ser usada como raiz de desinstalação.")
	}
	dbPath := filepath.Join(real, "database.json")
	if info, e := os.Lstat(dbPath); e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("O banco deve ser um arquivo local regular da instalação; nada foi removido.")
	}
	if err = uninstallCheckContained(real, dbPath); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return "", errors.New("Não foi encontrado o banco de dados do Estúdio Local nesta pasta. Nada foi removido.")
	}
	var db map[string]json.RawMessage
	if err = json.Unmarshal(raw, &db); err != nil {
		return "", errors.New("O banco de dados está inválido. Preserve a pasta e solicite reparo antes de desinstalar.")
	}
	var schema int
	if json.Unmarshal(db["schema"], &schema) != nil || schema < 1 {
		return "", errors.New("A pasta não possui uma instalação reconhecida do Estúdio Local.")
	}
	for _, key := range []string{"companies", "projects", "assets", "memories", "jobs", "settings", "installed"} {
		if _, ok := db[key]; !ok {
			return "", errors.New("A pasta não possui os dados esperados do Estúdio Local. Nada foi removido.")
		}
	}
	return real, nil
}

func uninstallSessionURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("Endereço de sessão inválido; nada foi encerrado.")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("Porta de sessão inválida; nada foi encerrado.")
	}
	return u.String(), nil
}

func uninstallStopSession(ctx context.Context, root string) error {
	realRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	root = realRoot
	file := filepath.Join(root, "session.json")
	if err := uninstallCheckContained(root, file); err != nil && !os.IsNotExist(err) {
		return err
	}
	if info, e := os.Lstat(file); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("A sessão não pode apontar para outra pasta; nada foi encerrado.")
	}
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var session struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &session) != nil {
		return errors.New("Sessão local inválida. Feche o Estúdio Local antes de desinstalar.")
	}
	origin, err := uninstallSessionURL(session.URL)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(session.Token) {
		return errors.New("Credencial da sessão inválida; nenhum processo foi encerrado.")
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, route string) (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, method, origin+route, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", "Bearer "+session.Token)
		req.Header.Set("Origin", origin)
		return client.Do(req)
	}
	resp, err := request("GET", "/api/ping")
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		return fmt.Errorf("Não foi possível confirmar se o aplicativo está fechado. Feche-o e tente novamente: %w", err)
	}
	var ping struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ping)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || !ping.OK || ping.Version != appVersion {
		return errors.New("A sessão não corresponde a esta versão do Estúdio Local. Feche o aplicativo manualmente e tente novamente; nenhum processo foi encerrado.")
	}
	resp, err = request("GET", "/api/state")
	if err != nil {
		return errors.New("Não foi possível confirmar a pasta desta sessão; nada foi encerrado.")
	}
	var state struct {
		DataDir string `json:"data_dir"`
	}
	stateErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&state)
	resp.Body.Close()
	stateRoot, resolveErr := filepath.EvalSymlinks(state.DataDir)
	if resp.StatusCode != http.StatusOK || stateErr != nil || resolveErr != nil || !strings.EqualFold(filepath.Clean(stateRoot), filepath.Clean(root)) {
		return errors.New("A sessão pertence a outra pasta ou instalação; nenhum processo foi encerrado.")
	}
	resp, err = request("POST", "/api/shutdown")
	if err != nil {
		return fmt.Errorf("Não foi possível encerrar o aplicativo com segurança: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("O aplicativo recusou o encerramento; nada foi removido.")
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("O aplicativo ainda está em execução. Feche-o e tente desinstalar novamente.")
		case <-tick.C:
			if _, e := os.Stat(file); os.IsNotExist(e) {
				return nil
			}
			response, e := request("GET", "/api/ping")
			if errors.Is(e, syscall.ECONNREFUSED) {
				return nil
			}
			if response != nil {
				response.Body.Close()
			}
		}
	}
}

// Reject a linked ancestor before touching a managed entry. Leaf symlinks are
// unlinked by uninstallRemoveTree, never followed into user/company folders.
func uninstallCheckContained(root, path string) error {
	realRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	root = realRoot
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !within(parent, root) {
		return errors.New("Uma pasta gerenciada aponta para fora da instalação. Nada nessa pasta foi removido.")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !within(real, root) || strings.EqualFold(filepath.Clean(real), filepath.Clean(root)) {
		return errors.New("Caminho de remoção fora dos componentes gerenciados.")
	}
	return nil
}

func uninstallRemoveTree(root, path string) error {
	err := uninstallCheckContained(root, path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = uninstallRemoveTree(root, filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func uninstallManagedComponents(root string) ([]string, error) {
	real, err := uninstallValidateRoot(root)
	if err != nil {
		return nil, err
	}
	root = real
	// Preserve all unknown/future JSON fields; only installed is changed.
	dbPath := filepath.Join(root, "database.json")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, err
	}
	backupDir := filepath.Join(root, "backups")
	if _, err = os.Lstat(backupDir); os.IsNotExist(err) {
		err = os.Mkdir(backupDir, 0700)
	}
	if err != nil {
		return nil, err
	}
	if err = uninstallCheckContained(root, backupDir); err != nil {
		return nil, err
	}
	// A backup symlink is not a writable backup destination.
	if bi, e := os.Lstat(backupDir); e != nil || bi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Pasta de backups não pode ser um atalho; nada foi removido.")
	}
	if realBackup, e := filepath.EvalSymlinks(backupDir); e != nil || !within(realBackup, root) {
		return nil, errors.New("Pasta de backups aponta para fora da instalação; desinstalação cancelada.")
	}
	backup := filepath.Join(backupDir, "uninstall-"+time.Now().UTC().Format("20060102T150405.000000000")+".json")
	if err = os.WriteFile(backup, data, 0600); err != nil {
		return nil, fmt.Errorf("Não foi possível preservar o banco antes da desinstalação: %w", err)
	}
	var db map[string]json.RawMessage
	if err = json.Unmarshal(data, &db); err != nil {
		return nil, err
	}
	db["installed"] = json.RawMessage("false")
	if err = atomicJSON(dbPath, db); err != nil {
		return nil, err
	}
	// Only these namespaces were created by this application. Unknown siblings,
	// company assets, projects, models, logs and backups are deliberately kept.
	paths := []string{"runtime/venv", "runtime/python", "cache/downloads", "cache/uv", "cache/npm", "cache/temp", "assets/media", "assets/remotion", "assets/skills", "runtime.json", "downloads.lock.json", "session.json", "EstudioLocal.exe"}
	runtimeDir := filepath.Join(root, "runtime")
	if realRuntime, re := filepath.EvalSymlinks(runtimeDir); re == nil && within(realRuntime, root) {
		if entries, e := os.ReadDir(runtimeDir); e == nil {
			known := regexp.MustCompile(`^(ollama|uv|node|ffmpeg)-[0-9][A-Za-z0-9._-]*$`)
			hash := regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
			for _, entry := range entries {
				if !known.MatchString(entry.Name()) {
					continue
				}
				candidate := filepath.Join(runtimeDir, entry.Name())
				if e = uninstallCheckContained(root, candidate); e != nil {
					continue
				}
				// Inspect the marker only after canonical containment. Unknown or
				// incomplete directories remain for explicit later user cleanup.
				canonicalCandidate, ce := filepath.EvalSymlinks(candidate)
				if ce != nil || !within(canonicalCandidate, root) {
					continue
				}
				marker, e := os.ReadFile(filepath.Join(candidate, ".verified-sha256"))
				if e == nil && hash.MatchString(strings.TrimSpace(string(marker))) {
					paths = append(paths, filepath.Join("runtime", entry.Name()))
				}
			}
		}
	}
	warnings := []string{}
	for _, rel := range paths {
		if err = uninstallRemoveTree(root, filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			warnings = append(warnings, fmt.Sprintf("Não removido (em uso ou protegido): %s. %v", rel, err))
		}
	}
	// Remove empty application containers only; never recursively remove root.
	for _, rel := range []string{"runtime", "cache", "assets"} {
		_ = os.Remove(filepath.Join(root, rel))
	}
	return warnings, nil
}
