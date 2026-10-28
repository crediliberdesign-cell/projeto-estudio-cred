package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func transferFixtureAsset(t *testing.T, a *App, cid, kind, name, pid string) Asset {
	t.Helper()
	id := newID()
	p := filepath.Join(a.root, "companies", cid, "assets", id+filepath.Ext(name))
	makeIntegrationFile(t, p)
	content := []byte("asset fixture " + kind + " para teste de identidade")
	if kind == "logo" {
		content = []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20"><rect width="20" height="20" fill="#123456"/></svg>`)
	}
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	x := Asset{ID: id, CompanyID: cid, ProjectID: pid, Name: name, Kind: kind, Path: p, SHA256: shaTest(content), Status: "ready", Family: "Fonte Teste", Role: "heading"}
	a.store.mu.Lock()
	a.store.db.Assets[id] = x
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return x
}
func transferExport(t *testing.T, a *App, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", a.origin+"/api/company-bundle?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	w := httptest.NewRecorder()
	a.security(http.HandlerFunc(a.companyBundle)).ServeHTTP(w, req)
	return w
}
func transferImport(t *testing.T, a *App, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	f, err := mw.CreateFormFile("file", "empresa.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", a.origin+"/api/company-bundle/import", &b)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.security(http.HandlerFunc(a.importCompanyBundle)).ServeHTTP(w, req)
	return w
}
func transferUnzip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range r.File {
		rd, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name], err = io.ReadAll(rd)
		rd.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func transferZip(t *testing.T, m companyTransferManifest, files map[string][]byte, extraName string, symlink bool) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("company.json")
	json.NewEncoder(f).Encode(m)
	for name, data := range files {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
	}
	if extraName != "" {
		header := &zip.FileHeader{Name: extraName, Method: zip.Deflate}
		if symlink {
			header.SetMode(os.ModeSymlink | 0777)
		}
		f, err := z.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("outside"))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func minimalTransfer() companyTransferManifest {
	return companyTransferManifest{Format: "estudio-local-company", Version: 1, Company: companyTransferIdentity{Name: "Importada", Colors: []Color{{Hex: "#123456", Role: "primary"}}, FontRoles: map[string]string{}}, Assets: []companyTransferAsset{}}
}
func waitTransferIndex(t *testing.T, a *App, cid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending := false
		a.store.mu.RLock()
		for _, asset := range a.store.db.Assets {
			if asset.CompanyID == cid && (asset.Status == "pending" || asset.Status == "indexing") {
				pending = true
			}
		}
		a.store.mu.RUnlock()
		if !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("imported documents were not indexed")
}

func TestCompanyTransferRoundTripWithAssets(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Marca transferência")
	other := integrationCompany(t, a, h, "Outra empresa")
	font := transferFixtureAsset(t, a, c.ID, "font", "Marca.ttf", "")
	logo := transferFixtureAsset(t, a, c.ID, "logo", "Logo.svg", "")
	document := transferFixtureAsset(t, a, c.ID, "manual", "Manual.txt", "")
	foreign := transferFixtureAsset(t, a, other.ID, "manual", "Privado.txt", "")
	p := integrationProject(t, a, h, c.ID, map[string]any{"duration": 99, "aspect": "9:16", "audio": map[string]any{"mode": "music"}, "briefing": "DO NOT TRANSFER VIDEO BRIEFING"})
	media := transferFixtureAsset(t, a, c.ID, "video", "Projeto.mp4", p.ID)
	c.FontRoles = map[string]string{"heading": font.ID, "body": font.ID, "caption": foreign.ID}
	c.Folders = []string{t.TempDir()}
	c.Shared = true
	if w := integrationRequest(t, a, h, "PUT", "/api/companies", c); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	exported := transferExport(t, a, "company_id="+c.ID+"&include_assets=1")
	if exported.Code != 200 {
		t.Fatal(exported.Code, exported.Body.String())
	}
	files := transferUnzip(t, exported.Body.Bytes())
	if len(files) != 4 {
		t.Fatal("wrong asset selection", len(files))
	}
	metadata := string(files["company.json"])
	for _, excluded := range []string{foreign.ID, media.ID, a.root, "DO NOT TRANSFER VIDEO BRIEFING", `"duration"`, `"aspect"`, `"audio"`, `"folders"`, `"shared"`, `"project_id"`} {
		if strings.Contains(metadata, excluded) {
			t.Fatal("private/project field exported", excluded)
		}
	}
	imported := transferImport(t, a, exported.Body.Bytes())
	if imported.Code != 200 {
		t.Fatal(imported.Code, imported.Body.String())
	}
	var response struct {
		Company Company `json:"company"`
		Count   int     `json:"imported_assets"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	nc := response.Company
	if nc.ID == c.ID || response.Count != 3 || nc.Shared || len(nc.Folders) != 0 {
		t.Fatal("import identity/grants invalid", nc)
	}
	oldIDs := map[string]bool{font.ID: true, logo.ID: true, document.ID: true}
	var newFont string
	for _, asset := range nc.Assets {
		if oldIDs[asset.ID] || asset.CompanyID != nc.ID || asset.ProjectID != "" {
			t.Fatal("asset ownership/remapping failed", asset)
		}
		if !within(asset.Path, filepath.Join(a.root, "companies", nc.ID)) {
			t.Fatal("asset outside new company")
		}
		if valid, err := fileHasSHA(asset.Path, asset.SHA256); err != nil || !valid {
			t.Fatal("import hash mismatch", err)
		}
		if asset.Kind == "font" {
			newFont = asset.ID
		}
	}
	if nc.FontRoles["heading"] != newFont || nc.FontRoles["body"] != newFont || nc.FontRoles["caption"] != "" {
		t.Fatal("font roles did not remap", nc.FontRoles)
	}
	waitTransferIndex(t, a, nc.ID)
	a.store.mu.RLock()
	chunks := 0
	for _, ch := range a.store.db.Chunks {
		if ch.CompanyID == nc.ID {
			chunks++
		}
	}
	a.store.mu.RUnlock()
	if chunks == 0 {
		t.Fatal("imported reference not indexed")
	}
	reopened, err := openStore(a.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.db.Companies[c.ID]; !ok {
		t.Fatal("original company overwritten")
	}
	if _, ok := reopened.db.Companies[nc.ID]; !ok {
		t.Fatal("new company not persisted")
	}
}

func TestCompanyTransferConfigOnlyAndExplicitMedia(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Configuração")
	font := transferFixtureAsset(t, a, c.ID, "font", "Marca.otf", "")
	p := integrationProject(t, a, h, c.ID, nil)
	transferFixtureAsset(t, a, c.ID, "video", "Filmagem.mp4", p.ID)
	c.FontRoles = map[string]string{"heading": font.ID}
	integrationRequest(t, a, h, "PUT", "/api/companies", c)
	without := transferExport(t, a, "company_id="+c.ID)
	if without.Code != 200 {
		t.Fatal(without.Body.String())
	}
	files := transferUnzip(t, without.Body.Bytes())
	if len(files) != 1 {
		t.Fatal("assets exported without opt-in")
	}
	w := transferImport(t, a, without.Body.Bytes())
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var resp struct {
		Company Company `json:"company"`
		Count   int     `json:"imported_assets"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 0 || len(resp.Company.FontRoles) != 0 {
		t.Fatal("config-only created phantom assets")
	}
	withMedia := transferExport(t, a, "company_id="+c.ID+"&include_assets=1&include_project_media=1")
	if withMedia.Code != 200 {
		t.Fatal(withMedia.Body.String())
	}
	if len(transferUnzip(t, withMedia.Body.Bytes())) != 3 {
		t.Fatal("explicit same-company media missing")
	}
}

func TestCompanyTransferRejectsForeignFilePath(t *testing.T) {
	a, h := newIntegrationApp(t)
	ca := integrationCompany(t, a, h, "Empresa A")
	cb := integrationCompany(t, a, h, "Empresa B")
	good := transferFixtureAsset(t, a, ca.ID, "manual", "Guia.txt", "")
	foreign := transferFixtureAsset(t, a, cb.ID, "manual", "Privado.txt", "")
	a.store.mu.Lock()
	good.Path = foreign.Path
	a.store.db.Assets[good.ID] = good
	a.store.mu.Unlock()
	w := transferExport(t, a, "company_id="+ca.ID+"&include_assets=1")
	if w.Code != 400 {
		t.Fatal("foreign company path was exported", w.Code)
	}
	if strings.Contains(w.Body.String(), "asset fixture") {
		t.Fatal("foreign content leaked in error")
	}
}

func TestCompanyTransferRejectsMaliciousArchives(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Preservada")
	keep := transferFixtureAsset(t, a, c.ID, "logo", "Original.svg", "")
	base := minimalTransfer()
	for _, name := range []string{"../escape.txt", "/absolute.txt", `C:\\escape.txt`, "assets/../../bad", "assets/CON", "undeclared.txt"} {
		t.Run(name, func(t *testing.T) {
			w := transferImport(t, a, transferZip(t, base, nil, name, false))
			if w.Code != 400 {
				t.Fatal("malicious ZIP accepted", w.Code)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		w := transferImport(t, a, transferZip(t, base, nil, "assets/link", true))
		if w.Code != 400 {
			t.Fatal("link accepted")
		}
	})
	t.Run("checksum", func(t *testing.T) {
		m := base
		m.Assets = []companyTransferAsset{{ID: "source", Name: "Bad.txt", Kind: "reference", File: "assets/a.txt", Bytes: 3, SHA256: strings.Repeat("0", 64)}}
		w := transferImport(t, a, transferZip(t, m, map[string][]byte{"assets/a.txt": []byte("bad")}, "", false))
		if w.Code != 400 {
			t.Fatal("bad hash accepted")
		}
	})
	t.Run("expanded budget", func(t *testing.T) {
		b := transferZip(t, base, nil, "", false)
		at := bytes.Index(b, []byte{'P', 'K', 1, 2})
		if at < 0 {
			t.Fatal("fixture header absent")
		}
		binary.LittleEndian.PutUint32(b[at+24:at+28], uint32(companyBundleExpandedLimit+1))
		w := transferImport(t, a, b)
		if w.Code != 400 {
			t.Fatal("oversized ZIP header accepted")
		}
	})
	if _, err := os.Stat(keep.Path); err != nil {
		t.Fatal("original asset removed", err)
	}
	a.store.mu.RLock()
	count := len(a.store.db.Companies)
	a.store.mu.RUnlock()
	if count != 1 {
		t.Fatal("failed import left a company")
	}
	dirs, _ := os.ReadDir(filepath.Join(a.root, "companies"))
	for _, dir := range dirs {
		if strings.HasPrefix(dir.Name(), ".import-") {
			t.Fatal("failed staging was not cleaned")
		}
	}
}

func TestCompanyTransferDoesNotAcceptVideoSettings(t *testing.T) {
	a, _ := newIntegrationApp(t)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("company.json")
	f.Write([]byte(`{"format":"estudio-local-company","version":1,"company":{"name":"Bad","duration":90,"audio":"music","aspect":"9:16"},"assets":[]}`))
	z.Close()
	w := transferImport(t, a, b.Bytes())
	if w.Code != 400 {
		t.Fatal("company package accepted per-video fields", w.Code)
	}
}
func TestCompanyTransferCompressedWriterLimit(t *testing.T) {
	var b bytes.Buffer
	w := bundleLimitWriter{writer: &b, left: 3}
	if _, err := w.Write([]byte("four")); err == nil {
		t.Fatal("compressed limit not enforced")
	}
	if b.Len() != 0 {
		t.Fatal("over-budget bytes written")
	}
}
