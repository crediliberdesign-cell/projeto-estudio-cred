package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shaTest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func TestInstallerManifest(t *testing.T) {
	m, err := GetDownloadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Archives) != 4 || len(m.Models) != 3 || len(m.Whisper.Files) != 5 {
		t.Fatal("incomplete manifest")
	}
	for _, a := range m.Archives {
		if _, err := hex.DecodeString(a.SHA256); err != nil || len(a.SHA256) != 64 {
			t.Fatal(a.ID)
		}
		if !strings.HasPrefix(a.URL, "https://") {
			t.Fatal("HTTPS required")
		}
	}
}
func TestDownloadResumes(t *testing.T) {
	body := []byte("conteudo publico verificado")
	offset := int64(7)
	dest := filepath.Join(t.TempDir(), "arquivo.zip")
	os.WriteFile(dest+".part", body[:offset], 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=7-" {
			t.Errorf("missing range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
		w.WriteHeader(206)
		w.Write(body[offset:])
	}))
	defer srv.Close()
	if err := downloadAttempt(context.Background(), srv.URL, dest, shaTest(body), int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if string(b) != string(body) {
		t.Fatal("wrong resumed content")
	}
}
func TestDownloadServerIgnoresRange(t *testing.T) {
	body := []byte("novo conteudo")
	dest := filepath.Join(t.TempDir(), "arquivo")
	os.WriteFile(dest+".part", []byte("velho"), 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	if err := downloadAttempt(context.Background(), srv.URL, dest, shaTest(body), int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if string(b) != string(body) {
		t.Fatal("range fallback appended old bytes")
	}
}
func TestDownloadRejectsCorruption(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "arquivo")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("tampered")) }))
	defer srv.Close()
	if err := downloadAttempt(context.Background(), srv.URL, dest, shaTest([]byte("expected")), 8, nil); err == nil {
		t.Fatal("accepted bad hash")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("corrupt final file exists")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("corrupt partial retained")
	}
}
func TestDownloadRejectsWrongRange(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "arquivo")
	os.WriteFile(dest+".part", []byte("x"), 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(206)
		w.Write([]byte("abcd"))
	}))
	defer srv.Close()
	if err := downloadAttempt(context.Background(), srv.URL, dest, shaTest([]byte("abcd")), 4, nil); err == nil {
		t.Fatal("wrong range accepted")
	}
}
func createTestZip(t *testing.T, name string, symlink bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	if symlink {
		h.SetMode(os.ModeSymlink | 0777)
	}
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("sample"))
	z.Close()
	f.Close()
	return path
}
func TestExtractRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape.exe", "/absolute.exe", `..\escape.exe`, "C:/evil.exe"} {
		t.Run(name, func(t *testing.T) {
			if err := extractZipSafe(createTestZip(t, name, false), t.TempDir()); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}
func TestExtractRejectsSymlink(t *testing.T) {
	if err := extractZipSafe(createTestZip(t, "link", true), t.TempDir()); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestExtractValid(t *testing.T) {
	dest := t.TempDir()
	if err := extractZipSafe(createTestZip(t, "app/bin/tool.exe", false), dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "app", "bin", "tool.exe"))
	if err != nil || string(b) != "sample" {
		t.Fatal("missing content", err)
	}
}
func TestDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := downloadVerified(ctx, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "x"), shaTest([]byte("x")), 1, nil); err != context.Canceled {
		t.Fatal(err)
	}
}
func TestPinnedModelDigestMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" {
			fmt.Fprintln(w, `{"status":"success"}`)
		} else {
			fmt.Fprintln(w, `{"models":[{"name":"test:1","digest":"bad"}]}`)
		}
	}))
	defer srv.Close()
	err := pullVerifiedModel(context.Background(), srv.URL, ModelSpec{Name: "test:1", SHA256: strings.Repeat("a", 64)}, nil)
	if err == nil || !strings.Contains(err.Error(), "mudou no registro") {
		t.Fatal(err)
	}
}

func TestVisualCRuntimeManifestMatchesBootstrap(t *testing.T) {
	m, err := GetDownloadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Prerequisites) != 1 {
		t.Fatal("missing Windows runtime prerequisite")
	}
	p := m.Prerequisites[0]
	if p.Version != visualCRuntimeVersion || p.SHA256 != visualCRuntimeSHA || p.URL != visualCRuntimeURL {
		t.Fatal("Microsoft prerequisite diverges from pinned manifest")
	}
}
func TestInstallerWorkerResultParsing(t *testing.T) {
	b := []byte("runtime diagnostic\n{\"type\":\"progress\",\"progress\":0.5}\n{\"type\":\"result\",\"segments\":[{\"text\":\"Parabéns\"}],\"device\":\"cpu\"}\n")
	result, err := parseInstallerWorkerResult(b)
	if err != nil || result["device"] != "cpu" {
		t.Fatal(result, err)
	}
	if _, err = parseInstallerWorkerResult([]byte("{\"type\":\"progress\"}")); err == nil {
		t.Fatal("missing result accepted")
	}
	if _, err = parseInstallerWorkerResult([]byte("{\"type\":\"error\",\"message\":\"GPU failed\"}")); err == nil {
		t.Fatal("worker failure ignored")
	}
}

func TestInstallerQuickChatBenchmarkEvidence(t *testing.T) {
	chats, unloads := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			unloads++
			fmt.Fprintln(w, `{"done":true}`)
		case "/api/ps":
			fmt.Fprintln(w, `{"models":[{"name":"quick:4b","size":1000,"size_vram":1000}]}`)
		case "/api/chat":
			chats++
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			if payload["think"] != false || payload["stream"] != true {
				t.Error("uncontrolled thinking/stream")
			}
			opts := payload["options"].(map[string]any)
			if opts["num_ctx"] != float64(2048) || opts["num_predict"] != float64(48) {
				t.Error("wrong benchmark limits")
			}
			fmt.Fprintln(w, `{"message":{"content":"Olá!"},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":" Tudo bem."},"done":true,"load_duration":120,"prompt_eval_count":7,"prompt_eval_duration":21,"eval_count":5,"eval_duration":32,"total_duration":173}`)
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	result, err := benchmarkInstallerQuick(context.Background(), RuntimeConfig{OllamaURL: srv.URL, QuickModel: "quick:4b"})
	if err != nil {
		t.Fatal(err)
	}
	if chats != 2 || unloads != 2 {
		t.Fatal("wrong cold/warm lifecycle", chats, unloads)
	}
	runs := result["runs"].([]map[string]any)
	if runs[0]["kind"] != "cold" || runs[1]["kind"] != "warm" || runs[0]["eval_count"] != int64(5) || runs[1]["ttft_ms"].(float64) <= 0 {
		t.Fatal("metrics were not recorded", runs)
	}
	if _, ok := result["residency"]; !ok {
		t.Fatal("missing residency evidence")
	}
}
func TestInstallerQuickChatRequiresCompletedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"content":"incomplete"},"done":false}`)
	}))
	defer srv.Close()
	if _, err := measureInstallerChat(context.Background(), RuntimeConfig{OllamaURL: srv.URL, QuickModel: "quick"}); err == nil {
		t.Fatal("incomplete stream accepted")
	}
}
