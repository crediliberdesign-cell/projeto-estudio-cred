package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newIntegrationApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &App{root: root, store: s, ctx: ctx, stop: cancel, token: "test-session-token", origin: "http://127.0.0.1:43210", cancels: map[string]context.CancelFunc{}, setup: SetupStatus{Status: "idle"}}
	mux := http.NewServeMux()
	a.routes(mux)
	return a, a.security(mux)
}
func integrationRequest(t *testing.T, a *App, h http.Handler, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if v != nil {
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, a.origin+path, &b)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func integrationCompany(t *testing.T, a *App, h http.Handler, name string) Company {
	t.Helper()
	w := integrationRequest(t, a, h, "POST", "/api/companies", map[string]any{"name": name, "colors": []Color{{Hex: "#abc", Role: "primary"}}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var c Company
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func integrationProject(t *testing.T, a *App, h http.Handler, cid string, extra map[string]any) Project {
	t.Helper()
	v := map[string]any{"company_id": cid, "title": "Vídeo individual"}
	for k, x := range extra {
		v[k] = x
	}
	w := integrationRequest(t, a, h, "POST", "/api/projects", v)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p Project
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func integrationUpload(t *testing.T, a *App, h http.Handler, cid, pid, name string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("company_id", cid)
	mw.WriteField("project_id", pid)
	mw.WriteField("kind", "file")
	f, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("fixture-content"))
	mw.Close()
	req := httptest.NewRequest("POST", a.origin+"/api/upload", &b)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func makeIntegrationFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIntegrationVideoChoicesStayPerProject(t *testing.T) {
	a, h := newIntegrationApp(t)
	w := integrationRequest(t, a, h, "POST", "/api/companies", map[string]any{"name": "Marca A", "duration": 99, "audio": "music", "aspect": "9:16", "briefing": "old video", "colors": []Color{{Hex: "abc", Role: "primary"}}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var c Company
	json.Unmarshal(w.Body.Bytes(), &c)
	for _, field := range []string{"duration", "audio", "aspect", "briefing"} {
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		if _, ok := m[field]; ok {
			t.Fatalf("company persisted video field %s", field)
		}
	}
	p1 := integrationProject(t, a, h, c.ID, map[string]any{"duration": 12, "aspect": "9:16", "audio": map[string]any{"mode": "none"}, "briefing": "A primeira campanha"})
	p2 := integrationProject(t, a, h, c.ID, nil)
	if p2.Duration != nil || p2.Audio != nil || p2.Aspect != "" || p2.Briefing != "" {
		t.Fatalf("new project inherited video choices: %+v", p2)
	}
	if p2.Scope != "company" {
		t.Fatal("unsafe default scope", p2.Scope)
	}
	if number(p1.Duration) != 12 {
		t.Fatal("individual duration lost")
	}
	if len(c.Colors) != 1 || c.Colors[0].Hex != "#AABBCC" {
		t.Fatal("color normalization failed")
	}
	c.Colors = []Color{{Hex: "#FF0000", Role: "primary"}}
	w = integrationRequest(t, a, h, "PUT", "/api/companies", c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	persisted, _ := a.store.project(p1.ID)
	raw, _ := json.Marshal(persisted.Brand)
	if !bytes.Contains(raw, []byte("#AABBCC")) || bytes.Contains(raw, []byte("#FF0000")) {
		t.Fatal("existing brand snapshot changed", string(raw))
	}
}

func TestIntegrationPersistenceAndMemoryIsolation(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	pa := integrationProject(t, a, h, ca.ID, nil)
	pb := integrationProject(t, a, h, cb.ID, nil)
	wa := integrationRequest(t, a, h, "POST", "/api/memories", Memory{CompanyID: ca.ID, ProjectID: pa.ID, Text: "Somente projeto A"})
	if wa.Code != 200 {
		t.Fatal(wa.Body.String())
	}
	wb := integrationRequest(t, a, h, "POST", "/api/memories", Memory{CompanyID: cb.ID, ProjectID: pb.ID, Text: "Somente projeto B"})
	if wb.Code != 200 {
		t.Fatal(wb.Body.String())
	}
	bad := integrationRequest(t, a, h, "POST", "/api/memories", Memory{CompanyID: ca.ID, ProjectID: pb.ID, Text: "contaminação"})
	if bad.Code != 403 {
		t.Fatal("cross-company memory accepted", bad.Code)
	}
	list := integrationRequest(t, a, h, "GET", "/api/memories?company_id="+ca.ID, nil)
	if strings.Contains(list.Body.String(), "Somente projeto B") || !strings.Contains(list.Body.String(), "Somente projeto A") {
		t.Fatal("memory leaked", list.Body.String())
	}
	s, err := openStore(a.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.db.Companies) != 2 || len(s.db.Projects) != 2 || len(s.db.Memories) != 2 {
		t.Fatal("restart lost persisted data")
	}
	var saved Memory
	json.Unmarshal(wa.Body.Bytes(), &saved)
	del := integrationRequest(t, a, h, "DELETE", "/api/memories/"+saved.ID, nil)
	if del.Code != 200 {
		t.Fatal(del.Body.String())
	}
	s, err = openStore(a.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.db.Memories[saved.ID]; ok {
		t.Fatal("forget did not persist")
	}
}

func TestIntegrationUploadsStayInCompany(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	pa := integrationProject(t, a, h, ca.ID, nil)
	pb := integrationProject(t, a, h, cb.ID, nil)
	w := integrationUpload(t, a, h, ca.ID, pa.ID, `..\..\video.bin`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var asset Asset
	json.Unmarshal(w.Body.Bytes(), &asset)
	if !within(asset.Path, filepath.Join(a.root, "companies", ca.ID, "assets")) {
		t.Fatal("upload escaped company")
	}
	if asset.CompanyID != ca.ID || asset.ProjectID != pa.ID {
		t.Fatal("wrong upload ownership")
	}
	if _, err := a.authorizePath(pa, asset.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.authorizePath(pb, asset.Path); err == nil {
		t.Fatal("other company upload readable")
	}
	bad := integrationUpload(t, a, h, ca.ID, pb.ID, "bad.bin")
	if bad.Code != 403 {
		t.Fatal("cross-company project upload accepted", bad.Code)
	}
	bad = integrationRequest(t, a, h, "POST", "/api/projects", map[string]any{"company_id": cb.ID, "scope": "computer", "source_asset_id": asset.ID})
	if bad.Code != 403 {
		t.Fatal("cross-company source asset accepted", bad.Code)
	}
}

func TestIntegrationFileScopeAndSymlinkEscape(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Empresa")
	p := integrationProject(t, a, h, c.ID, nil)
	outside := makeIntegrationFile(t, filepath.Join(t.TempDir(), "private.bin"))
	if _, err := a.authorizePath(p, outside); err == nil {
		t.Fatal("default scope reads unrelated file")
	}
	folder := t.TempDir()
	inside := makeIntegrationFile(t, filepath.Join(folder, "ok.bin"))
	p.Scope = "folder"
	p.Folder = folder
	if _, err := a.authorizePath(p, inside); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(folder, "escape.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if _, err := a.authorizePath(p, link); err == nil {
		t.Fatal("symlink escaped selected folder")
	}
	w := integrationRequest(t, a, h, "POST", "/api/projects", map[string]any{"company_id": c.ID, "source": outside})
	if w.Code != 403 {
		t.Fatal("default project source accepted unrelated file", w.Code)
	}
}

func TestIntegrationComputerScopeStillExcludesOtherCompanies(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	pa := integrationProject(t, a, h, ca.ID, map[string]any{"scope": "computer"})
	bfile := makeIntegrationFile(t, filepath.Join(a.root, "companies", cb.ID, "assets", "secret.bin"))
	if _, err := a.authorizePath(pa, bfile); err == nil {
		t.Fatal("computer scope crossed private company")
	}
	pb := integrationProject(t, a, h, cb.ID, nil)
	bproject := makeIntegrationFile(t, filepath.Join(a.root, "projects", pb.ID, "outputs", "secret.bin"))
	if _, err := a.authorizePath(pa, bproject); err == nil {
		t.Fatal("computer scope crossed other project")
	}
}

func TestIntegrationCanonicalCompanyBoundary(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	pa := integrationProject(t, a, h, ca.ID, map[string]any{"scope": "computer"})
	other := makeIntegrationFile(t, filepath.Join(a.root, "companies", cb.ID, "assets", "secret.bin"))
	alias := filepath.Join(t.TempDir(), "installation-link")
	if err := os.Symlink(a.root, alias); err != nil {
		t.Skip("symlink unavailable", err)
	}
	a.root = alias
	if _, err := a.authorizePath(pa, other); err == nil {
		t.Fatal("canonical path bypasses a symlinked installation company boundary")
	}
}

func TestIntegrationExternalCompanyFoldersExcluded(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	folder := t.TempDir()
	secret := makeIntegrationFile(t, filepath.Join(folder, "client-secret.bin"))
	cb.Folders = []string{folder}
	w := integrationRequest(t, a, h, "PUT", "/api/companies", cb)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	pa := integrationProject(t, a, h, ca.ID, map[string]any{"scope": "computer"})
	if _, err := a.authorizePath(pa, secret); err == nil {
		t.Fatal("computer scope reads external folder registered to another private company")
	}
}

func TestIntegrationRAGCompanyIsolation(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	a.store.mu.Lock()
	a.store.db.Assets["asset-a"] = Asset{ID: "asset-a", CompanyID: ca.ID, Name: "campanha A", Kind: "briefing"}
	a.store.db.Assets["asset-b"] = Asset{ID: "asset-b", CompanyID: cb.ID, Name: "campanha B", Kind: "briefing"}
	a.store.db.Chunks["a"] = Chunk{ID: "a", AssetID: "asset-a", CompanyID: ca.ID, Text: "campanha exclusiva A"}
	a.store.db.Chunks["b"] = Chunk{ID: "b", AssetID: "asset-b", CompanyID: cb.ID, Text: "campanha exclusiva B"}
	a.store.mu.Unlock()
	hits := a.retrieve(context.Background(), ca.ID, "campanha", 8)
	if len(hits) != 1 || hits[0].AssetID != "asset-a" {
		t.Fatal("RAG leaked other company", hits)
	}
	cb.Shared = true
	w := integrationRequest(t, a, h, "PUT", "/api/companies", cb)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	hits = a.retrieve(context.Background(), ca.ID, "campanha", 8)
	if len(hits) != 2 {
		t.Fatal("explicit shared reference not available", hits)
	}
}

func TestIntegrationSessionHostAndOriginProtection(t *testing.T) {
	a, h := newIntegrationApp(t)
	for _, tc := range []struct {
		name, token, host, origin string
		status                    int
	}{{"missing token", "", "", "", 401}, {"valid", a.token, "", "", 200}, {"foreign host", a.token, "attacker.invalid", "", 403}, {"foreign origin", a.token, "", "https://attacker.invalid", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", a.origin+"/api/state", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	req := httptest.NewRequest("GET", a.origin+"/launch?token="+a.token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 303 {
		t.Fatal("launch failed", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie missing protections")
	}
	req = httptest.NewRequest("GET", a.origin+"/api/ping", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("cookie auth failed")
	}
}
