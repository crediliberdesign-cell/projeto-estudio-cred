package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed downloads.json
var downloadManifestBytes []byte

type InstallEvent struct {
	Stage    string  `json:"stage"`
	Progress float64 `json:"progress"`
	Message  string  `json:"message"`
}
type RuntimeConfig struct {
	Root             string `json:"root"`
	Python           string `json:"python"`
	Node             string `json:"node"`
	FFmpeg           string `json:"ffmpeg"`
	FFprobe          string `json:"ffprobe"`
	OllamaExecutable string `json:"ollama_executable"`
	OllamaURL        string `json:"ollama_url"`
	RemotionDir      string `json:"remotion_dir"`
	WhisperModelPath string `json:"whisper_model_path"`
	QuickModel       string `json:"quick_model"`
	AgentModel       string `json:"agent_model"`
	EmbeddingModel   string `json:"embedding_model"`
}
type DownloadFile struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type ArchiveSpec struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Executable string `json:"executable"`
}
type ModelSpec struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type DownloadManifest struct {
	SchemaVersion    int           `json:"schema_version"`
	MinimumFreeBytes uint64        `json:"minimum_free_bytes"`
	PythonVersion    string        `json:"python_version"`
	Archives         []ArchiveSpec `json:"archives"`
	Prerequisites    []ArchiveSpec `json:"prerequisites"`
	Models           []ModelSpec   `json:"models"`
	Whisper          struct {
		Repository string         `json:"repository"`
		Revision   string         `json:"revision"`
		Files      []DownloadFile `json:"files"`
	} `json:"whisper"`
}

func GetDownloadManifest() (DownloadManifest, error) {
	var m DownloadManifest
	err := json.Unmarshal(downloadManifestBytes, &m)
	return m, err
}
func emitInstall(emit func(InstallEvent), stage string, p float64, msg string) {
	if emit != nil {
		emit(InstallEvent{stage, p, msg})
	}
}

// InspectInstallTarget checks the selected destination volume, not total PC capacity.
func InspectInstallTarget(ctx context.Context, root string) (map[string]any, error) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return nil, errors.New("Este instalador requer Windows 11 x64. Nenhuma dependência foi instalada.")
	}
	if !filepath.IsAbs(root) || strings.HasPrefix(root, `\\`) {
		return nil, errors.New("Escolha uma pasta em uma unidade local do Windows, como C: ou D:.")
	}
	script := `$ErrorActionPreference='Stop'; $drive=[System.IO.DriveInfo]::new([System.IO.Path]::GetPathRoot($env:AGENT_INSTALL_ROOT)); $os=Get-CimInstance Win32_OperatingSystem; $gpu=@(Get-CimInstance Win32_VideoController | Select-Object Name,DriverVersion); @{free_bytes=$drive.AvailableFreeSpace; os=$os.Caption; build=$os.BuildNumber; ram_bytes=([uint64]$os.TotalVisibleMemorySize*1024); gpu=$gpu} | ConvertTo-Json -Depth 4 -Compress`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	HideManagedProcessWindow(cmd)
	cmd.Env = append(os.Environ(), "AGENT_INSTALL_ROOT="+root)
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Não foi possível verificar a unidade de destino: %w", err)
	}
	var out map[string]any
	if err = json.Unmarshal(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), &out); err != nil {
		return nil, err
	}
	if build, err := strconv.Atoi(fmt.Sprint(out["build"])); err == nil && build < 22000 {
		return out, errors.New("Esta distribuição é destinada ao Windows 11; esta versão do Windows não foi validada.")
	}
	return out, nil
}

func RunInstall(ctx context.Context, root string, emit func(InstallEvent)) (RuntimeConfig, error) {
	cfg := RuntimeConfig{Root: root, QuickModel: "qwen3.5:4b", AgentModel: "qwen3.5:9b", EmbeddingModel: "embeddinggemma:latest", RemotionDir: filepath.Join(root, "assets", "remotion"), WhisperModelPath: filepath.Join(root, "models", "whisper-turbo")}
	manifest, err := GetDownloadManifest()
	if err != nil {
		return cfg, err
	}
	emitInstall(emit, "preflight", 0, "Verificando Windows, unidade de destino e espaço disponível…")
	info, err := InspectInstallTarget(ctx, root)
	if err != nil {
		return cfg, err
	}
	free, _ := info["free_bytes"].(float64)
	needed := remainingInstallSpace(root, manifest.MinimumFreeBytes)
	if uint64(free) < needed {
		return cfg, fmt.Errorf("São necessários aproximadamente %.1f GiB livres nesta unidade para concluir a instalação; disponíveis %.1f GiB. Seus vídeos precisam de espaço adicional.", float64(needed)/(1<<30), free/(1<<30))
	}
	for _, p := range []string{"cache/downloads", "runtime", "models", "logs"} {
		if err = os.MkdirAll(filepath.Join(root, filepath.FromSlash(p)), 0700); err != nil {
			return cfg, err
		}
	}
	writeInstallJSON(filepath.Join(root, "logs", "hardware.json"), info)
	if err = os.WriteFile(filepath.Join(root, "downloads.lock.json"), downloadManifestBytes, 0600); err != nil {
		return cfg, err
	}
	if err = ensureVisualCRuntime(ctx, root, emit); err != nil {
		return cfg, err
	}
	bins := map[string]string{}
	for i, a := range manifest.Archives {
		if ctx.Err() != nil {
			return cfg, ctx.Err()
		}
		base := 0.02 + float64(i)*0.08
		emitInstall(emit, "runtime", base, "Preparando "+a.ID+" "+a.Version+"…")
		dir := filepath.Join(root, "runtime", a.ID+"-"+a.Version)
		marker := filepath.Join(dir, ".verified-sha256")
		ready, _ := os.ReadFile(marker)
		if strings.TrimSpace(string(ready)) != a.SHA256 {
			archive := filepath.Join(root, "cache", "downloads", a.ID+"-"+a.Version+".zip")
			if err = downloadVerified(ctx, a.URL, archive, a.SHA256, a.Bytes, func(done, total int64) {
				p := base
				if total > 0 {
					p += 0.07 * float64(done) / float64(total)
				}
				emitInstall(emit, "download", p, fmt.Sprintf("Baixando %s: %.0f / %.0f MB", a.ID, float64(done)/1e6, float64(total)/1e6))
			}); err != nil {
				return cfg, fmt.Errorf("%s: %w", a.ID, err)
			}
			// Never overwrite a directory that was not created by this installer.
			if st, e := os.Stat(dir); e == nil && st.IsDir() {
				if _, e = os.Stat(marker); e != nil {
					return cfg, fmt.Errorf("A pasta %s já existe sem identificação do instalador. Escolha outra pasta de instalação.", dir)
				}
				return cfg, fmt.Errorf("A versão instalada de %s tem um manifesto diferente. Use outra pasta para esta versão.", a.ID)
			}
			stage := dir + ".extracting"
			if err = os.MkdirAll(stage, 0700); err != nil {
				return cfg, err
			}
			// Staging is strictly app-namespaced and contains downloaded files only.
			if err = extractZipSafe(archive, stage); err != nil {
				return cfg, err
			}
			if err = os.WriteFile(filepath.Join(stage, ".verified-sha256"), []byte(a.SHA256+"\n"), 0600); err != nil {
				return cfg, err
			}
			if err = os.Rename(stage, dir); err != nil {
				return cfg, err
			}
		}
		bins[a.ID], err = findManagedExecutable(dir, a.Executable)
		if err != nil {
			return cfg, err
		}
	}
	cfg.Node = bins["node"]
	cfg.OllamaExecutable = bins["ollama"]
	cfg.FFmpeg = bins["ffmpeg"]
	cfg.FFprobe = filepath.Join(filepath.Dir(cfg.FFmpeg), "ffprobe.exe")
	cfg.Python = filepath.Join(root, "runtime", "venv", "Scripts", "python.exe")
	env := runtimeEnv(cfg)
	emitInstall(emit, "python", 0.36, "Preparando Python isolado e bibliotecas de vídeo…")
	if err = runInstallerCommand(ctx, root, env, bins["uv"], []string{"python", "install", manifest.PythonVersion}, emit, "python"); err != nil {
		return cfg, err
	}
	if _, err = os.Stat(cfg.Python); os.IsNotExist(err) {
		if err = runInstallerCommand(ctx, root, env, bins["uv"], []string{"venv", "--python", manifest.PythonVersion, filepath.Join(root, "runtime", "venv")}, emit, "python"); err != nil {
			return cfg, err
		}
	}
	req := filepath.Join(root, "assets", "media", "requirements.txt")
	if _, err = os.Stat(req); err != nil {
		return cfg, fmt.Errorf("O pacote está incompleto: requirements.txt ausente: %w", err)
	}
	lock := filepath.Join(root, "assets", "media", "requirements.lock")
	if _, err = os.Stat(lock); err != nil {
		return cfg, fmt.Errorf("O pacote está incompleto: requirements.lock ausente: %w", err)
	}
	installArgs := []string{"pip", "install", "--python", cfg.Python, "--require-hashes", "--only-binary", ":all:", "-r", lock}
	if err = runInstallerCommand(ctx, root, env, bins["uv"], installArgs, emit, "python"); err != nil {
		return cfg, err
	}
	if err = runInstallerCommand(ctx, root, env, bins["uv"], []string{"pip", "freeze", "--python", cfg.Python}, emit, "python-lock"); err != nil {
		return cfg, err
	}
	emitInstall(emit, "remotion", 0.43, "Instalando o renderizador de animações e navegador local…")
	if _, err = os.Stat(filepath.Join(cfg.RemotionDir, "package-lock.json")); err != nil {
		return cfg, fmt.Errorf("O pacote está incompleto: package-lock.json ausente: %w", err)
	}
	npm := filepath.Join(filepath.Dir(cfg.Node), "node_modules", "npm", "bin", "npm-cli.js")
	if err = runInstallerCommand(ctx, cfg.RemotionDir, env, cfg.Node, []string{npm, "ci", "--no-audit", "--no-fund"}, emit, "remotion"); err != nil {
		return cfg, err
	}
	if err = runInstallerCommand(ctx, cfg.RemotionDir, env, cfg.Node, []string{filepath.Join(cfg.RemotionDir, "build-player.mjs")}, emit, "player"); err != nil {
		return cfg, err
	}
	cli := filepath.Join(cfg.RemotionDir, "node_modules", "@remotion", "cli", "remotion-cli.js")
	if err = runInstallerCommand(ctx, cfg.RemotionDir, env, cfg.Node, []string{cli, "browser", "ensure"}, emit, "browser"); err != nil {
		return cfg, err
	}
	emitInstall(emit, "transcription", 0.50, "Baixando reconhecimento de fala multilíngue, incluindo português…")
	if err = os.MkdirAll(cfg.WhisperModelPath, 0700); err != nil {
		return cfg, err
	}
	for _, f := range manifest.Whisper.Files {
		if err = downloadVerified(ctx, f.URL, filepath.Join(cfg.WhisperModelPath, f.Name), f.SHA256, f.Bytes, func(done, total int64) {
			p := .50
			if total > 0 {
				p += .08 * float64(done) / float64(total)
			}
			emitInstall(emit, "transcription", p, fmt.Sprintf("Reconhecimento de fala: %s %.0f / %.0f MB", f.Name, float64(done)/1e6, float64(total)/1e6))
		}); err != nil {
			return cfg, err
		}
	}
	emitInstall(emit, "models", 0.60, "Iniciando o motor local para baixar os modelos…")
	proc, err := StartOllama(ctx, &cfg)
	if err != nil {
		return cfg, err
	}
	defer StopOllama(proc)
	for i, m := range manifest.Models {
		if err = pullVerifiedModel(ctx, cfg.OllamaURL, m, func(done, total int64, msg string) {
			p := .60 + float64(i)*.10
			if total > 0 {
				p += .09 * float64(done) / float64(total)
			}
			emitInstall(emit, "models", p, m.Name+": "+msg)
		}); err != nil {
			return cfg, err
		}
	}
	emitInstall(emit, "readiness", 0.92, "Conferindo ferramentas instaladas e suporte a legendas…")
	if err = checkRuntimeReadiness(ctx, cfg, emit); err != nil {
		return cfg, err
	}
	if err = writeInstallJSON(filepath.Join(root, "runtime.json"), cfg); err != nil {
		return cfg, err
	}
	emitInstall(emit, "ready", 1, "Instalação concluída. O aplicativo está pronto para iniciar o assistente.")
	return cfg, nil
}

// Verified against the bytes served by download.visualstudio.microsoft.com on 2026-10-28.
// Microsoft's redistributable is the sole conditional machine-level prerequisite.
const visualCRuntimeVersion = "14.51.36247.0"
const visualCRuntimeURL = "https://download.visualstudio.microsoft.com/download/pr/ebdab8e5-1d7b-4d9f-a11b-cbb1720c3b12/843068991DAAA1F73AD9F6239BCE4D0F6A07A51F18C37EA2A867E9BECA71295C/VC_redist.x64.exe"
const visualCRuntimeSHA = "843068991daaa1f73ad9f6239bce4d0f6a07a51f18c37ea2a867e9beca71295c"

func ensureVisualCRuntime(ctx context.Context, root string, emit func(InstallEvent)) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	check := `$ErrorActionPreference='Stop'; $p=Join-Path $env:SystemRoot 'System32\msvcp140.dll'; if(!(Test-Path -LiteralPath $p)){exit 1}; $v=(Get-Item -LiteralPath $p).VersionInfo; if(([version]($v.FileMajorPart.ToString()+'.'+$v.FileMinorPart+'.'+$v.FileBuildPart+'.'+$v.FilePrivatePart)) -lt [version]'14.51.36247.0'){exit 1}`
	probe := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", check)
	HideManagedProcessWindow(probe)
	if probe.Run() == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	emitInstall(emit, "visual-cpp", .01, "Preparando componente Microsoft Visual C++. Se o Windows pedir, confirme a instalação da Microsoft ou solicite ao TI.")
	target := filepath.Join(root, "cache", "downloads", "VC_redist-"+visualCRuntimeVersion+"-x64.exe")
	if err := downloadVerified(ctx, visualCRuntimeURL, target, visualCRuntimeSHA, 18731856, nil); err != nil {
		return fmt.Errorf("Microsoft Visual C++: %w", err)
	}
	// SHA256 has already been verified. Also require an intact Microsoft Authenticode signature.
	script := `$ErrorActionPreference='Stop'; $sig=Get-AuthenticodeSignature -LiteralPath $env:ATELIE_VCREDIST; if($sig.Status -ne 'Valid' -or $sig.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation'){throw 'Assinatura Microsoft inválida'}; try {$p=Start-Process -FilePath $env:ATELIE_VCREDIST -ArgumentList '/install','/passive','/norestart' -Verb RunAs -Wait -PassThru; if($p.ExitCode -notin 0,3010,1638){throw ('Instalador Microsoft retornou '+$p.ExitCode)}} catch {Write-Error $_;exit 1}`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", script)
	HideManagedProcessWindow(cmd)
	cmd.Env = append(os.Environ(), "ATELIE_VCREDIST="+target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("Instalação cancelada. Se a janela da Microsoft já estiver aberta, deixe-a concluir; o componente de sistema não será interrompido à força.")
		}
		return fmt.Errorf("O componente Microsoft Visual C++ precisa de autorização do Windows/TI. Os dados do aplicativo foram preservados. Tente instalar novamente após autorizar o instalador Microsoft. Detalhe: %s", string(out))
	}
	probe = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", check)
	HideManagedProcessWindow(probe)
	if err = probe.Run(); err != nil {
		return errors.New("O Microsoft Visual C++ solicitou conclusão/reinício ou não pôde ser confirmado. Reinicie o Windows se solicitado pela Microsoft e execute Instalar novamente.")
	}
	return nil
}

// Subtract resumable app-managed downloads from the initial budget; keep a 12 GiB work reserve.
func remainingInstallSpace(root string, initial uint64) uint64 {
	var present uint64
	for _, sub := range []string{"runtime", "models", "cache/downloads"} {
		filepath.WalkDir(filepath.Join(root, filepath.FromSlash(sub)), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !d.IsDir() {
				if info, e := d.Info(); e == nil && info.Size() > 0 {
					present += uint64(info.Size())
				}
			}
			return nil
		})
	}
	const reserve = uint64(12 << 30)
	if present >= initial-reserve {
		return reserve
	}
	return initial - present
}

func runtimeEnv(cfg RuntimeConfig) []string {
	return append(os.Environ(), "PATH="+filepath.Dir(cfg.Node)+string(os.PathListSeparator)+filepath.Dir(cfg.FFmpeg)+string(os.PathListSeparator)+os.Getenv("PATH"), "UV_PYTHON_INSTALL_DIR="+filepath.Join(cfg.Root, "runtime", "python"), "UV_CACHE_DIR="+filepath.Join(cfg.Root, "cache", "uv"), "UV_PYTHON_PREFERENCE=only-managed", "PYTHONNOUSERSITE=1", "HF_HUB_DISABLE_TELEMETRY=1", "HF_HUB_OFFLINE=1", "HF_DATASETS_OFFLINE=1", "DO_NOT_TRACK=1", "npm_config_cache="+filepath.Join(cfg.Root, "cache", "npm"))
}

// StartOllama starts only the app's portable executable. Existing Ollama installations are untouched.
// Caller must call StopOllama when closing the application. It also must save cfg.OllamaURL.
func StartOllama(ctx context.Context, cfg *RuntimeConfig) (*exec.Cmd, error) {
	if cfg.OllamaExecutable == "" {
		return nil, errors.New("Motor local ainda não instalado.")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := listener.Addr().String()
	listener.Close()
	cfg.OllamaURL = "http://" + addr
	cmd := exec.CommandContext(ctx, cfg.OllamaExecutable, "serve")
	HideManagedProcessWindow(cmd)
	cmd.Dir = filepath.Dir(cfg.OllamaExecutable)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST="+addr, "OLLAMA_MODELS="+filepath.Join(cfg.Root, "models", "ollama"), "OLLAMA_NO_CLOUD=1", "OLLAMA_NUM_PARALLEL=1", "OLLAMA_MAX_LOADED_MODELS=1", "OLLAMA_CONTEXT_LENGTH=8192", "OLLAMA_FLASH_ATTENTION=1", "OLLAMA_KV_CACHE_TYPE=q8_0", "OLLAMA_KEEP_ALIVE=10m")
	os.MkdirAll(filepath.Join(cfg.Root, "logs"), 0700)
	log, err := os.OpenFile(filepath.Join(cfg.Root, "logs", "ollama.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = log
	cmd.Stderr = log
	setOwnedCancellation(cmd)
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	log.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 120; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", cfg.OllamaURL+"/api/version", nil)
		r, e := client.Do(req)
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return cmd, nil
			}
		}
		select {
		case <-ctx.Done():
			StopOllama(cmd)
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	StopOllama(cmd)
	return nil, errors.New("O motor local não iniciou em 60 segundos. Consulte logs/ollama.log e a política de execução da empresa.")
}

// Reflection keeps one source file cross-compilable while setting Windows-only process flags.
func HideManagedProcessWindow(cmd *exec.Cmd) {
	if runtime.GOOS != "windows" {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	attr := reflect.ValueOf(cmd.SysProcAttr).Elem()
	if f := attr.FieldByName("HideWindow"); f.IsValid() && f.CanSet() {
		f.SetBool(true)
	}
	if f := attr.FieldByName("CreationFlags"); f.IsValid() && f.CanSet() {
		f.SetUint(f.Uint() | 0x08000000)
	}
}
func setOwnedCancellation(cmd *exec.Cmd) {
	var once sync.Once
	var result error
	cmd.Cancel = func() error { once.Do(func() { result = killOwnedCommand(cmd) }); return result }
}
func StopOllama(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		} else {
			_ = killOwnedCommand(cmd)
		}
		_ = cmd.Wait()
	}
}
func killOwnedCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		HideManagedProcessWindow(killer)
		return killer.Run()
	}
	return cmd.Process.Kill()
}

func runInstallerCommand(ctx context.Context, dir string, env []string, executable string, args []string, emit func(InstallEvent), stage string) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	HideManagedProcessWindow(cmd)
	cmd.Dir = dir
	cmd.Env = env
	setOwnedCancellation(cmd)
	logDir := os.Getenv("TEMP")
	for _, s := range env {
		if strings.HasPrefix(s, "UV_CACHE_DIR=") {
			logDir = filepath.Join(filepath.Dir(filepath.Dir(strings.TrimPrefix(s, "UV_CACHE_DIR="))), "logs")
		}
	}
	os.MkdirAll(logDir, 0700)
	file, err := os.OpenFile(filepath.Join(logDir, "install-"+stage+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	fmt.Fprintf(file, "\n[%s] %s %s\n", time.Now().UTC().Format(time.RFC3339), filepath.Base(executable), strings.Join(args, " "))
	var tail limitedInstallBuffer
	cmd.Stdout = io.MultiWriter(file, &tail)
	cmd.Stderr = cmd.Stdout
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Falha na etapa %s: %w\n%s", stage, err, tail.String())
	}
	return nil
}

type limitedInstallBuffer struct{ b []byte }

func (b *limitedInstallBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	if len(b.b) > 4096 {
		b.b = b.b[len(b.b)-4096:]
	}
	return len(p), nil
}
func (b *limitedInstallBuffer) String() string  { return string(b.b) }
func writeInstallJSON(path string, v any) error { return atomicJSON(path, v) }

func checkRuntimeReadiness(ctx context.Context, cfg RuntimeConfig, emit func(InstallEvent)) error {
	checks := []struct {
		name, exe string
		args      []string
	}{{"ffmpeg", cfg.FFmpeg, []string{"-version"}}, {"ffprobe", cfg.FFprobe, []string{"-version"}}, {"node", cfg.Node, []string{"--version"}}, {"python", cfg.Python, []string{"-c", `import faster_whisper, ctranslate2, PIL; print('imports OK')`}}}
	report := map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "platform": runtime.GOOS + "/" + runtime.GOARCH, "windows_gpu_render_verified": false}
	for _, c := range checks {
		cmd := exec.CommandContext(ctx, c.exe, c.args...)
		HideManagedProcessWindow(cmd)
		cmd.Env = runtimeEnv(cfg)
		out, err := cmd.CombinedOutput()
		report[c.name] = string(out)
		if err != nil {
			writeInstallJSON(filepath.Join(cfg.Root, "logs", "readiness.json"), report)
			return fmt.Errorf("Verificação de %s falhou: %w — %s", c.name, err, string(out))
		}
	}
	out, err := exec.CommandContext(ctx, cfg.FFmpeg, "-hide_banner", "-filters").CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("subtitles")) {
		return errors.New("FFmpeg instalado não confirmou o filtro subtitles/libass.")
	}
	report["libass"] = true
	emitInstall(emit, "readiness", .96, "Testando transcrição de uma pequena fala em português no computador…")
	sampleResult, err := transcribeInstallerFixture(ctx, cfg)
	report["asr_sample"] = sampleResult
	if err != nil {
		writeInstallJSON(filepath.Join(cfg.Root, "logs", "readiness.json"), report)
		return err
	}
	report["asr_gpu_verified"] = sampleResult["device"] == "cuda"
	emitInstall(emit, "readiness", .97, "Medindo a resposta do modelo rápido com primeira carga e com modelo carregado…")
	quick, quickErr := benchmarkInstallerQuick(ctx, cfg)
	report["quick_chat"] = quick
	if quickErr != nil {
		writeInstallJSON(filepath.Join(cfg.Root, "logs", "readiness.json"), report)
		return quickErr
	}
	emitInstall(emit, "readiness", .98, "Renderizando um motion curto e verificando o arquivo gerado…")
	motion, motionErr := renderInstallerMotion(ctx, cfg)
	report["motion_sample"] = motion
	if motionErr != nil {
		writeInstallJSON(filepath.Join(cfg.Root, "logs", "readiness.json"), report)
		return motionErr
	}
	emitInstall(emit, "readiness", .99, "Verificando recursos de vídeo e aceleração disponíveis…")
	diagnostic, diagErr := runInstallerWorker(ctx, cfg, "diagnose", map[string]any{}, 2*time.Minute)
	report["media_diagnostics"] = diagnostic
	if diagErr != nil {
		report["diagnostics_warning"] = diagErr.Error()
	}
	report["result"] = "quick-model cold/warm replies, Portuguese speech and two-second motion passed; hardware and timing results describe only these samples"
	return writeInstallJSON(filepath.Join(cfg.Root, "logs", "readiness.json"), report)
}

func transcribeInstallerFixture(ctx context.Context, cfg RuntimeConfig) (map[string]any, error) {
	sample := filepath.Join(cfg.Root, "assets", "media", "sample-pt.wav")
	if _, err := os.Stat(sample); err != nil {
		return nil, errors.New("Pacote incompleto: exemplo de fala de verificação ausente.")
	}
	dir := filepath.Join(cfg.Root, "logs", "readiness-asr")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	req := map[string]any{"action": "transcribe", "input": sample, "output_dir": dir, "ffmpeg": cfg.FFmpeg, "ffprobe": cfg.FFprobe, "whisper_model_path": cfg.WhisperModelPath, "allowed_roots": []string{filepath.Dir(sample)}, "options": map[string]any{"language": "pt"}}
	requestPath := filepath.Join(dir, "request.json")
	if err := atomicJSON(requestPath, req); err != nil {
		return nil, err
	}
	timeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(timeout, cfg.Python, filepath.Join(cfg.Root, "assets", "media", "worker.py"), "--request", requestPath)
	cmd.Env = append(runtimeEnv(cfg), "PYTHONUTF8=1")
	HideManagedProcessWindow(cmd)
	setOwnedCancellation(cmd)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(dir, "worker.log"), out, 0600)
	if err != nil {
		return nil, fmt.Errorf("O teste de fala local falhou: %w. Consulte logs/readiness-asr/worker.log.", err)
	}
	result, err := parseInstallerWorkerResult(out)
	if err != nil {
		return nil, err
	}
	segments, ok := result["segments"].([]any)
	hasText := false
	for _, seg := range segments {
		if m, yes := seg.(map[string]any); yes {
			if text, yes := m["text"].(string); yes && strings.TrimSpace(text) != "" {
				hasText = true
			}
		}
	}
	if !ok || len(segments) == 0 || !hasText {
		return result, errors.New("O teste de transcrição terminou sem reconhecer a amostra de fala. Consulte o diagnóstico antes de usar legendas.")
	}
	result["wall_seconds"] = time.Since(start).Seconds()
	result["sample_purpose"] = "one short Portuguese utterance; runtime check only, not an accuracy benchmark"
	return result, nil
}
func parseInstallerWorkerResult(data []byte) (map[string]any, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var result map[string]any
	for scanner.Scan() {
		var ev map[string]any
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if ev["type"] == "error" {
			return nil, fmt.Errorf("Teste da ferramenta: %v", ev["message"])
		}
		if ev["type"] == "result" {
			result = ev
			if nested, ok := ev["result"].(map[string]any); ok {
				result = nested
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("A ferramenta não retornou resultado de verificação.")
	}
	return result, nil
}

func runInstallerWorker(ctx context.Context, cfg RuntimeConfig, action string, extra map[string]any, timeout time.Duration) (map[string]any, error) {
	dir := filepath.Join(cfg.Root, "logs", "readiness-"+action)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	req := map[string]any{"action": action, "output_dir": dir, "ffmpeg": cfg.FFmpeg, "ffprobe": cfg.FFprobe, "node": cfg.Node, "remotion_dir": cfg.RemotionDir, "whisper_model_path": cfg.WhisperModelPath, "allowed_roots": []string{dir}}
	for k, v := range extra {
		req[k] = v
	}
	requestPath := filepath.Join(dir, "request.json")
	if err := atomicJSON(requestPath, req); err != nil {
		return nil, err
	}
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(child, cfg.Python, filepath.Join(cfg.Root, "assets", "media", "worker.py"), "--request", requestPath)
	cmd.Env = append(runtimeEnv(cfg), "PYTHONUTF8=1")
	HideManagedProcessWindow(cmd)
	setOwnedCancellation(cmd)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(dir, "worker.log"), out, 0600)
	if err != nil {
		return nil, fmt.Errorf("A verificação %s falhou: %w. Consulte logs/readiness-%s/worker.log.", action, err, action)
	}
	result, err := parseInstallerWorkerResult(out)
	if result != nil {
		result["wall_seconds"] = time.Since(start).Seconds()
	}
	return result, err
}
func renderInstallerMotion(ctx context.Context, cfg RuntimeConfig) (map[string]any, error) {
	project := map[string]any{"title": "Verificação do Estúdio", "duration": 2, "aspect": "16:9", "resolution": 320, "audio": map[string]any{"mode": "none"}, "brand_snapshot": map[string]any{"name": "Demonstração", "colors": []map[string]string{{"role": "accent", "hex": "#A9EE7E"}}, "fonts": []any{}, "logos": []any{}}}
	result, err := runInstallerWorker(ctx, cfg, "motion", map[string]any{"project": project, "options": map[string]any{"scenes": []map[string]any{{"type": "title", "title": "Pronto para criar", "body": "Verificação local de animação", "duration": 2}}}}, 10*time.Minute)
	if err != nil {
		return result, err
	}
	output, _ := result["output"].(string)
	path, err := canonical(output)
	expectedDir, dirErr := canonical(filepath.Join(cfg.Root, "logs", "readiness-motion"))
	if err != nil || dirErr != nil || !within(path, expectedDir) {
		return result, errors.New("A verificação de motion não retornou um arquivo na pasta esperada.")
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() < 128 {
		return result, errors.New("O motion de verificação não gerou um MP4 válido.")
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, cfg.FFprobe, "-v", "error", "-show_format", "-show_streams", "-of", "json", path)
	HideManagedProcessWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return result, fmt.Errorf("FFprobe rejeitou o motion de verificação: %w", err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err = json.Unmarshal(out, &probe); err != nil {
		return result, err
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || duration < 1.9 || duration > 2.2 {
		return result, errors.New("A duração do motion de verificação divergiu dos dois segundos solicitados.")
	}
	video, audio := false, false
	for _, s := range probe.Streams {
		if s.CodecType == "video" && s.Width > 0 && s.Height > 0 {
			video = true
		}
		if s.CodecType == "audio" {
			audio = true
		}
	}
	if !video || audio {
		return result, errors.New("As faixas do motion de verificação não correspondem ao vídeo sem áudio solicitado.")
	}
	cmd = exec.CommandContext(probeCtx, cfg.FFmpeg, "-v", "error", "-nostdin", "-i", path, "-f", "null", "-")
	HideManagedProcessWindow(cmd)
	setOwnedCancellation(cmd)
	decoded, err := cmd.CombinedOutput()
	if err != nil {
		return result, fmt.Errorf("A decodificação do motion de verificação falhou: %w — %s", err, string(decoded))
	}
	result["probe_duration"] = duration
	result["decoded_successfully"] = true
	result["sample_purpose"] = "two-second 16:9 motion at 320px; export readiness only, not complex-render performance"
	return result, nil
}
func installerOllamaJSON(ctx context.Context, base, path string, input any) (map[string]any, error) {
	method := "GET"
	var body io.Reader
	if input != nil {
		method = "POST"
		b, _ := json.Marshal(input)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Ollama retornou HTTP%d durante a verificação", resp.StatusCode)
	}
	var result map[string]any
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result)
	if err == nil {
		if message, ok := result["error"].(string); ok && message != "" {
			err = errors.New(message)
		}
	}
	return result, err
}
func benchmarkInstallerQuick(ctx context.Context, cfg RuntimeConfig) (map[string]any, error) {
	result := map[string]any{"model": cfg.QuickModel, "purpose": "two tiny local chat requests; timings are measured, not a performance guarantee"}
	child, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	if _, err := installerOllamaJSON(child, cfg.OllamaURL, "/api/generate", map[string]any{"model": cfg.QuickModel, "keep_alive": 0}); err != nil {
		return result, fmt.Errorf("Não foi possível preparar a medição do modelo rápido: %w", err)
	}
	defer func() {
		cleanup, close := context.WithTimeout(context.Background(), 5*time.Second)
		defer close()
		_, _ = installerOllamaJSON(cleanup, cfg.OllamaURL, "/api/generate", map[string]any{"model": cfg.QuickModel, "keep_alive": 0})
	}()
	runs := []map[string]any{}
	for _, kind := range []string{"cold", "warm"} {
		run, err := measureInstallerChat(child, cfg)
		if err != nil {
			return result, fmt.Errorf("O modelo rápido não concluiu a resposta de verificação (%s): %w", kind, err)
		}
		run["kind"] = kind
		runs = append(runs, run)
		result["runs"] = runs
	}
	ps, err := installerOllamaJSON(child, cfg.OllamaURL, "/api/ps", nil)
	if err != nil {
		result["residency_warning"] = err.Error()
	} else {
		result["residency"] = ps
	}
	return result, nil
}
func measureInstallerChat(ctx context.Context, cfg RuntimeConfig) (map[string]any, error) {
	payload := map[string]any{"model": cfg.QuickModel, "messages": []map[string]string{{"role": "user", "content": "Oi! Responda brevemente em português: tudo bem?"}}, "stream": true, "think": false, "keep_alive": "2m", "options": map[string]any{"num_ctx": 2048, "num_predict": 48, "temperature": 0}}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.OllamaURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("HTTP%d: %s", resp.StatusCode, string(b))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	result := map[string]any{}
	text := ""
	first := time.Duration(0)
	done := false
	for scanner.Scan() {
		var ev struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done           bool   `json:"done"`
			Error          string `json:"error"`
			Total          int64  `json:"total_duration"`
			Load           int64  `json:"load_duration"`
			PromptCount    int64  `json:"prompt_eval_count"`
			PromptDuration int64  `json:"prompt_eval_duration"`
			EvalCount      int64  `json:"eval_count"`
			EvalDuration   int64  `json:"eval_duration"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, err
		}
		if ev.Error != "" {
			return nil, errors.New(ev.Error)
		}
		if ev.Message.Content != "" {
			if first == 0 {
				first = time.Since(started)
			}
			text += ev.Message.Content
		}
		if ev.Done {
			done = true
			result["total_duration_ns"] = ev.Total
			result["load_duration_ns"] = ev.Load
			result["prompt_eval_count"] = ev.PromptCount
			result["prompt_eval_duration_ns"] = ev.PromptDuration
			result["eval_count"] = ev.EvalCount
			result["eval_duration_ns"] = ev.EvalDuration
			break
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	if !done || strings.TrimSpace(text) == "" {
		return nil, errors.New("Resposta vazia ou stream sem confirmação de conclusão.")
	}
	result["ttft_ms"] = float64(first) / float64(time.Millisecond)
	result["wall_ms"] = float64(time.Since(started)) / float64(time.Millisecond)
	result["response"] = text
	return result, nil
}

func downloadVerified(ctx context.Context, url, dest, wantSHA string, expected int64, progress func(int64, int64)) error {
	if len(wantSHA) != 64 {
		return errors.New("Checksum SHA256 inválido no manifesto.")
	}
	if _, err := hex.DecodeString(wantSHA); err != nil {
		return err
	}
	if ok, _ := fileHasSHA(dest, wantSHA); ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = downloadAttempt(ctx, url, dest, wantSHA, expected, progress)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return fmt.Errorf("Download não concluído após 3 tentativas: %w. Ao tentar novamente, os arquivos parciais serão reaproveitados.", last)
}
func downloadAttempt(ctx context.Context, url, dest, wantSHA string, expected int64, progress func(int64, int64)) error {
	part := dest + ".part"
	offset := int64(0)
	if st, e := os.Stat(part); e == nil {
		offset = st.Size()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "VideoLocal-Installer/1.0")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, TLSHandshakeTimeout: 20 * time.Second}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if ok, _ := fileHasSHA(part, wantSHA); ok {
			return replaceDownload(part, dest)
		}
		os.Remove(part)
		return errors.New("Download parcial reiniciado.")
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("Servidor retornou HTTP %d", resp.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if resp.StatusCode == 206 {
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
			return errors.New("Resposta de retomada inconsistente.")
		}
		flags |= os.O_APPEND
	} else {
		offset = 0
		flags |= os.O_TRUNC
	}
	total := expected
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	f, err := os.OpenFile(part, flags, 0600)
	if err != nil {
		return err
	}
	counter := &downloadCounter{done: offset, total: total, emit: progress}
	_, err = io.Copy(io.MultiWriter(f, counter), resp.Body)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if ok, e := fileHasSHA(part, wantSHA); e != nil {
		return e
	} else if !ok {
		os.Remove(part)
		return errors.New("SHA256 divergiu do manifesto. Arquivo rejeitado antes de executar.")
	}
	return replaceDownload(part, dest)
}
func replaceDownload(from, to string) error {
	if _, err := os.Stat(to); err == nil {
		if err = os.Remove(to); err != nil {
			return err
		}
	}
	return os.Rename(from, to)
}

type downloadCounter struct {
	done, total int64
	last        time.Time
	emit        func(int64, int64)
}

func (c *downloadCounter) Write(p []byte) (int, error) {
	c.done += int64(len(p))
	if c.emit != nil && (time.Since(c.last) > 500*time.Millisecond || c.done == c.total) {
		c.last = time.Now()
		c.emit(c.done, c.total)
	}
	return len(p), nil
}
func fileHasSHA(path, want string) (bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(want), nil
}
func extractZipSafe(archive, dest string) error {
	if st, e := os.Lstat(dest); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("A pasta de extração não pode ser um link.")
	}
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	total := uint64(0)
	for _, f := range r.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if strings.Contains(name, ":") || strings.HasPrefix(name, "/") || f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Caminho inseguro no ZIP: %s", f.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		rel, err := filepath.Rel(dest, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("ZIP tentou gravar fora da pasta gerenciada.")
		}
		total += f.UncompressedSize64
		if total > 16<<30 {
			return errors.New("ZIP excedeu o limite de extração de 16 GiB.")
		}
		// Reject existing symlink/junction components when resuming extraction.
		for cur := target; cur != dest && cur != filepath.Dir(cur); cur = filepath.Dir(cur) {
			if st, e := os.Lstat(cur); e == nil && st.Mode()&os.ModeSymlink != 0 {
				return errors.New("Link encontrado na pasta de extração.")
			}
		}
		if f.FileInfo().IsDir() {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		_, cpErr := io.Copy(out, in)
		in.Close()
		e := out.Close()
		if cpErr != nil {
			return cpErr
		}
		if e != nil {
			return e
		}
	}
	return nil
}
func findManagedExecutable(dir, name string) (string, error) {
	found := ""
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), name) {
			if found != "" {
				return fmt.Errorf("Executável ambíguo no pacote: %s", name)
			}
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("Executável ausente no pacote: %s", name)
	}
	return found, nil
}
func pullVerifiedModel(ctx context.Context, base string, m ModelSpec, progress func(int64, int64, string)) error {
	b, _ := json.Marshal(map[string]any{"model": m.Name, "stream": true})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/pull", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Download do modelo %s falhou: HTTP%d", m.Name, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	success := false
	for sc.Scan() {
		var e struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
		}
		if err = json.Unmarshal(sc.Bytes(), &e); err != nil {
			return err
		}
		if e.Error != "" {
			return errors.New(e.Error)
		}
		if progress != nil {
			progress(e.Completed, e.Total, e.Status)
		}
		if e.Status == "success" {
			success = true
		}
	}
	if err = sc.Err(); err != nil {
		return err
	}
	if !success {
		return errors.New("Download do modelo terminou sem confirmação.")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", base+"/api/tags", nil)
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&tags); err != nil {
		return err
	}
	for _, t := range tags.Models {
		if t.Name == m.Name {
			if strings.TrimPrefix(t.Digest, "sha256:") != m.SHA256 {
				return fmt.Errorf("O modelo %s mudou no registro desde a versão deste instalador. Atualize o manifesto; o pacote não usará pesos diferentes sem revisão.", m.Name)
			}
			return nil
		}
	}
	return fmt.Errorf("Modelo %s não apareceu na lista após download.", m.Name)
}
