package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func uninstallFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data := map[string]any{"schema": 1, "installed": true, "companies": map[string]any{"A": map[string]string{"name": "A"}}, "projects": map[string]any{}, "assets": map[string]any{}, "memories": map[string]any{}, "jobs": map[string]any{}, "settings": map[string]any{}, "future_field": "preserve-me"}
	if err := atomicJSON(filepath.Join(root, "database.json"), data); err != nil {
		t.Fatal(err)
	}
	return root
}
func uninstallPut(t *testing.T, root, rel, value string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestUninstallPreservesUserData(t *testing.T) {
	root := uninstallFixture(t)
	keep := []string{"companies/A/logo.png", "projects/P/video.mp4", "models/whisper/model.bin", "models/ollama/model", "logs/report.json", "custom/file.txt", "runtime/unrelated/my-file.txt"}
	remove := []string{"runtime/venv/python.exe", "runtime/python/python.exe", "cache/downloads/archive.zip", "cache/uv/cache.dat", "cache/npm/cache.dat", "assets/media/worker.py", "assets/remotion/package.json", "assets/skills/SKILL.md", "runtime.json", "downloads.lock.json", "EstudioLocal.exe"}
	for _, p := range append(keep, remove...) {
		uninstallPut(t, root, p, "fixture")
	}
	uninstallPut(t, root, "runtime/ffmpeg-9.0.2/.verified-sha256", strings.Repeat("a", 64))
	uninstallPut(t, root, "runtime/ffmpeg-9.0.2/ffmpeg.exe", "binary")
	warnings, e := uninstallManagedComponents(root)
	if e != nil || len(warnings) != 0 {
		t.Fatalf("warnings=%v err=%v", warnings, e)
	}
	for _, p := range keep {
		if _, e := os.Stat(filepath.Join(root, p)); e != nil {
			t.Fatalf("data removed: %s", p)
		}
	}
	for _, p := range remove {
		if _, e := os.Stat(filepath.Join(root, p)); !os.IsNotExist(e) {
			t.Fatalf("managed file remained: %s", p)
		}
	}
	if _, e := os.Stat(filepath.Join(root, "runtime/ffmpeg-9.0.2")); !os.IsNotExist(e) {
		t.Fatal("verified runtime remained")
	}
	var db map[string]any
	b, _ := os.ReadFile(filepath.Join(root, "database.json"))
	json.Unmarshal(b, &db)
	if db["installed"] != false || db["future_field"] != "preserve-me" {
		t.Fatal(db)
	}
	backups, _ := filepath.Glob(filepath.Join(root, "backups", "uninstall-*.json"))
	if len(backups) != 1 {
		t.Fatal("missing database backup")
	}
	if _, e = uninstallManagedComponents(root); e != nil {
		t.Fatal("rerun", e)
	}
}
func TestUninstallRejectsUnrecognizedDirectory(t *testing.T) {
	root := t.TempDir()
	uninstallPut(t, root, "database.json", `{"installed":true}`)
	uninstallPut(t, root, "runtime/venv/keep.txt", "keep")
	if _, e := uninstallManagedComponents(root); e == nil {
		t.Fatal("accepted unrelated root")
	}
	if _, e := os.Stat(filepath.Join(root, "runtime/venv/keep.txt")); e != nil {
		t.Fatal("deleted unrelated data")
	}
}
func TestUninstallSymlinkEscape(t *testing.T) {
	root := uninstallFixture(t)
	outside := t.TempDir()
	uninstallPut(t, outside, "media/keep.txt", "keep")
	if e := os.Symlink(outside, filepath.Join(root, "assets")); e != nil {
		t.Skip(e)
	}
	warnings, e := uninstallManagedComponents(root)
	if e != nil {
		t.Fatal(e)
	}
	if len(warnings) == 0 {
		t.Fatal("expected refusal notice")
	}
	if _, e := os.Stat(filepath.Join(outside, "media/keep.txt")); e != nil {
		t.Fatal("external files removed")
	}
}
func TestUninstallLeafSymlinkDoesNotFollow(t *testing.T) {
	root := uninstallFixture(t)
	outside := t.TempDir()
	uninstallPut(t, outside, "keep.txt", "keep")
	os.MkdirAll(filepath.Join(root, "runtime/venv"), 0700)
	if e := os.Symlink(outside, filepath.Join(root, "runtime/venv/link")); e != nil {
		t.Skip(e)
	}
	if _, e := uninstallManagedComponents(root); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(outside, "keep.txt")); e != nil {
		t.Fatal("link target removed")
	}
}
func TestUninstallSessionRejectsExternalAndRedirectTargets(t *testing.T) {
	for _, s := range []string{"http://evil.test:9000", "http://127.0.0.1.evil.test:9000", "https://127.0.0.1:9000", "http://127.0.0.1:9000/path", "http://u:p@127.0.0.1:9000", "http://127.0.0.1:9000?next=x", "http://127.0.0.1:0"} {
		if _, e := uninstallSessionURL(s); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestUninstallStopsOnlyAuthenticatedMatchingSession(t *testing.T) {
	root := uninstallFixture(t)
	token := strings.Repeat("a", 48)
	stopped := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing credential")
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path == "/api/state" {
			json.NewEncoder(w).Encode(map[string]any{"data_dir": root})
			return
		}
		if r.URL.Path == "/api/ping" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": appVersion})
			return
		}
		if r.URL.Path == "/api/shutdown" && r.Method == "POST" {
			stopped = true
			os.Remove(filepath.Join(root, "session.json"))
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	atomicJSON(filepath.Join(root, "session.json"), map[string]string{"url": server.URL, "token": token})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if e := uninstallStopSession(ctx, root); e != nil {
		t.Fatal(e)
	}
	if !stopped {
		t.Fatal("shutdown not requested")
	}
}
func TestUninstallRefusesOtherApplication(t *testing.T) {
	root := uninstallFixture(t)
	stopped := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			stopped = true
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": "unrelated-app"})
	}))
	defer server.Close()
	atomicJSON(filepath.Join(root, "session.json"), map[string]string{"url": server.URL, "token": strings.Repeat("b", 48)})
	if e := uninstallStopSession(context.Background(), root); e == nil {
		t.Fatal("accepted foreign session")
	}
	if stopped {
		t.Fatal("stopped unrelated service")
	}
}

func TestUninstallRejectsLinkedDatabase(t *testing.T) {
	root := uninstallFixture(t)
	outside := uninstallFixture(t)
	os.Remove(filepath.Join(root, "database.json"))
	if e := os.Symlink(filepath.Join(outside, "database.json"), filepath.Join(root, "database.json")); e != nil {
		t.Skip(e)
	}
	if _, e := uninstallManagedComponents(root); e == nil {
		t.Fatal("linked database accepted")
	}
}
func TestUninstallRefusesCopiedSessionFromOtherRoot(t *testing.T) {
	root := uninstallFixture(t)
	other := uninstallFixture(t)
	stopped := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			stopped = true
		}
		if r.URL.Path == "/api/state" {
			json.NewEncoder(w).Encode(map[string]any{"data_dir": other})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": appVersion})
	}))
	defer server.Close()
	atomicJSON(filepath.Join(root, "session.json"), map[string]string{"url": server.URL, "token": strings.Repeat("c", 48)})
	if e := uninstallStopSession(context.Background(), root); e == nil {
		t.Fatal("copied session accepted")
	}
	if stopped {
		t.Fatal("other installation stopped")
	}
}
