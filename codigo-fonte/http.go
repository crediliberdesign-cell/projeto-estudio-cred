package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	a.store.mu.RLock()
	cs := []Company{}
	for _, c := range a.store.db.Companies {
		c.Assets = []Asset{}
		for _, x := range a.store.db.Assets {
			if x.CompanyID == c.ID {
				c.Assets = append(c.Assets, x)
				if x.Kind == "logo" {
					c.LogoURI = "/api/assets/" + x.ID + "/content"
				}
			}
		}
		cs = append(cs, c)
	}
	ps := []Project{}
	for _, p := range a.store.db.Projects {
		ps = append(ps, p)
	}
	installed := a.store.db.Installed
	settings := a.store.db.Settings
	a.store.mu.RUnlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	sort.Slice(ps, func(i, j int) bool { return ps[i].Updated > ps[j].Updated })
	a.installMu.Lock()
	setup := a.setup
	a.installMu.Unlock()
	cfg := a.runtimeConfig()
	reply(w, map[string]any{"installed": installed, "setup": setup, "companies": cs, "projects": ps, "settings": settings, "data_dir": a.root, "version": appVersion, "runtime_ready": cfg.OllamaURL != "", "status": "local", "download_estimate": "Cerca de 15–20 GB; reserve ao menos 35 GB livres, além dos seus vídeos."})
}
func (a *App) companies(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.mu.RLock()
		ids := []string{}
		for id := range a.store.db.Companies {
			ids = append(ids, id)
		}
		a.store.mu.RUnlock()
		cs := []Company{}
		for _, id := range ids {
			c, _ := a.store.company(id)
			cs = append(cs, c)
		}
		reply(w, cs)
		return
	}
	if r.Method != "POST" && r.Method != "PUT" {
		fail(w, 405, "Método não permitido")
		return
	}
	var c Company
	if body(r, &c) != nil || strings.TrimSpace(c.Name) == "" {
		fail(w, 400, "Informe o nome da empresa.")
		return
	}
	for i, col := range c.Colors {
		v := normalizedHex(col.Hex)
		if v == "" {
			fail(w, 400, "Cor inválida: use o seletor ou um HEX válido.")
			return
		}
		c.Colors[i].Hex = v
	}
	for _, folder := range c.Folders {
		if _, e := canonical(folder); e != nil {
			fail(w, 400, "Pasta inacessível: "+folder)
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if c.ID == "" {
		c.ID = newID()
	} else if _, ok := a.store.db.Companies[c.ID]; !ok {
		fail(w, 404, "Empresa não encontrada")
		return
	}
	old := a.store.db.Companies[c.ID]
	c.Version = old.Version + 1
	c.Updated = now()
	c.Assets = nil
	c.LogoURI = ""
	a.store.db.Companies[c.ID] = c
	if e := a.store.saveLocked(); e != nil {
		fail(w, 500, e.Error())
		return
	}
	reply(w, c)
}
func (a *App) companyAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/companies/"), "/")
	id := parts[0]
	c, ok := a.store.company(id)
	if !ok {
		fail(w, 404, "Empresa não encontrada")
		return
	}
	if len(parts) > 1 && parts[1] == "export" {
		w.Header().Set("Content-Disposition", `attachment; filename="empresa.json"`)
		c.Assets = nil
		reply(w, c)
		return
	}
	if r.Method == "GET" {
		reply(w, c)
		return
	}
	if r.Method == "DELETE" {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		for _, p := range a.store.db.Projects {
			if p.CompanyID == id {
				fail(w, 409, "Esta empresa possui projetos. Exporte seus dados antes de remover a empresa.")
				return
			}
		}
		delete(a.store.db.Companies, id)
		for k, v := range a.store.db.Chunks {
			if v.CompanyID == id {
				delete(a.store.db.Chunks, k)
			}
		}
		for k, v := range a.store.db.Memories {
			if v.CompanyID == id {
				delete(a.store.db.Memories, k)
			}
		}
		for k, v := range a.store.db.Assets {
			if v.CompanyID == id {
				delete(a.store.db.Assets, k)
			}
		}
		_ = a.store.saveLocked()
		reply(w, map[string]bool{"ok": true})
		return
	}
	fail(w, 405, "Método não permitido")
}
func (a *App) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.mu.RLock()
		defer a.store.mu.RUnlock()
		ps := []Project{}
		for _, p := range a.store.db.Projects {
			if r.URL.Query().Get("company_id") == "" || p.CompanyID == r.URL.Query().Get("company_id") {
				ps = append(ps, p)
			}
		}
		reply(w, ps)
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	var p Project
	if body(r, &p) != nil {
		fail(w, 400, "Projeto inválido")
		return
	}
	c, ok := a.store.company(p.CompanyID)
	if !ok {
		fail(w, 400, "Selecione uma empresa.")
		return
	}
	p.ID = newID()
	p.Created = now()
	p.Updated = p.Created
	if p.Title == "" {
		p.Title = "Novo vídeo"
	}
	if p.Scope == "" {
		p.Scope = "company"
	}
	if p.Scope != "company" && p.Scope != "folder" && p.Scope != "computer" && p.Scope != "files" {
		fail(w, 400, "Escopo de arquivos inválido")
		return
	}
	p.Brand = buildBrand(c)
	p.Messages = []ChatMessage{}
	p.Revisions = []map[string]any{}
	if p.SourceAssetID != "" {
		a.store.mu.RLock()
		asset, ok := a.store.db.Assets[p.SourceAssetID]
		a.store.mu.RUnlock()
		if !ok || asset.CompanyID != p.CompanyID {
			fail(w, 403, "Arquivo pertence a outra empresa")
			return
		}
		p.Source = asset.Path
	}
	if p.Source != "" {
		if _, e := a.authorizePath(p, p.Source); e != nil {
			fail(w, 403, e.Error())
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	a.store.db.Projects[p.ID] = p
	if e := a.store.saveLocked(); e != nil {
		fail(w, 500, e.Error())
		return
	}
	reply(w, p)
}
func (a *App) projectAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	if strings.HasSuffix(id, "/transcript") {
		a.saveTranscript(w, r, strings.TrimSuffix(id, "/transcript"))
		return
	}
	p, ok := a.store.project(id)
	if !ok {
		fail(w, 404, "Projeto não encontrado")
		return
	}
	if r.Method == "GET" {
		reply(w, p)
		return
	}
	if r.Method != "PUT" && r.Method != "PATCH" {
		fail(w, 405, "Use GET ou PUT")
		return
	}
	var v map[string]any
	if body(r, &v) != nil {
		fail(w, 400, "Dados inválidos")
		return
	}
	old := cloneMap(p)
	for _, key := range []string{"title", "briefing", "aspect", "duration", "audio", "resolution", "source", "source_asset_id", "folder", "scope", "operations", "attachments"} {
		if x, ok := v[key]; ok {
			old[key] = x
		}
	}
	b, _ := json.Marshal(old)
	if json.Unmarshal(b, &p) != nil {
		fail(w, 400, "Dados inválidos")
		return
	}
	if p.Scope != "company" && p.Scope != "folder" && p.Scope != "computer" && p.Scope != "files" {
		fail(w, 400, "Escopo inválido")
		return
	}
	if p.Source != "" {
		if _, e := a.authorizePath(p, p.Source); e != nil {
			fail(w, 403, e.Error())
			return
		}
	}
	if v["refresh_brand"] == true || v["use_latest_brand"] == true {
		c, _ := a.store.company(p.CompanyID)
		p.Revisions = append(p.Revisions, map[string]any{"type": "brand_update", "at": now(), "previous_brand": p.Brand})
		p.Brand = buildBrand(c)
	}
	p.Updated = now()
	a.store.mu.Lock()
	a.store.db.Projects[id] = p
	e := a.store.saveLocked()
	a.store.mu.Unlock()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	reply(w, p)
}
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 100<<30)
	reader, e := r.MultipartReader()
	if e != nil {
		fail(w, 400, "Envie um arquivo multipart")
		return
	}
	fields := map[string]string{}
	tempDir := filepath.Join(a.root, "incoming")
	_ = os.MkdirAll(tempDir, 0700)
	var name, tmp string
	var sum string
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		if part.FileName() == "" {
			b, _ := io.ReadAll(io.LimitReader(part, 8192))
			fields[part.FormName()] = string(b)
			continue
		}
		if tmp != "" {
			fail(w, 400, "Envie um arquivo por vez")
			_ = os.Remove(tmp)
			return
		}
		name = filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		f, e := os.CreateTemp(tempDir, "import-*")
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		tmp = f.Name()
		h := sha256.New()
		_, e = io.Copy(io.MultiWriter(f, h), part)
		_ = f.Close()
		if e != nil {
			_ = os.Remove(tmp)
			fail(w, 500, "Importação interrompida: "+e.Error())
			return
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	if tmp == "" {
		fail(w, 400, "Nenhum arquivo recebido")
		return
	}
	defer os.Remove(tmp)
	cid := fields["company_id"]
	if _, ok := a.store.company(cid); !ok {
		fail(w, 400, "Cadastre ou selecione uma empresa antes de importar")
		return
	}
	if pid := fields["project_id"]; pid != "" {
		p, ok := a.store.project(pid)
		if !ok || p.CompanyID != cid {
			fail(w, 403, "Projeto incompatível com a empresa")
			return
		}
	}
	kind := fields["kind"]
	switch kind {
	case "logo", "font", "reference", "manual", "briefing", "instructions", "file", "video", "audio", "image":
	default:
		kind = "file"
	}
	ext := strings.ToLower(filepath.Ext(name))
	if kind == "logo" && !contains([]string{".png", ".svg", ".jpg", ".jpeg", ".webp"}, ext) {
		fail(w, 400, "Logo: use PNG, SVG, JPG ou WebP")
		return
	}
	if kind == "font" && !contains([]string{".ttf", ".otf", ".woff2"}, ext) {
		fail(w, 400, "Fonte: use TTF, OTF ou WOFF2")
		return
	}
	a.store.mu.RLock()
	for _, x := range a.store.db.Assets {
		if x.CompanyID == cid && x.SHA256 == sum && x.Kind == kind {
			a.store.mu.RUnlock()
			reply(w, x)
			return
		}
	}
	a.store.mu.RUnlock()
	id := newID()
	dest := filepath.Join(a.root, "companies", cid, "assets", id+ext)
	_ = os.MkdirAll(filepath.Dir(dest), 0700)
	if e = os.Rename(tmp, dest); e != nil {
		fail(w, 500, e.Error())
		return
	}
	asset := Asset{ID: id, CompanyID: cid, ProjectID: fields["project_id"], Name: name, Kind: kind, Path: dest, SHA256: sum, Status: "ready", Created: now(), Family: fields["family"], Role: fields["role"]}
	isDoc := contains([]string{".pdf", ".docx", ".txt", ".md", ".json", ".csv"}, ext)
	if isDoc {
		asset.Status = "pending"
	}
	a.store.mu.Lock()
	a.store.db.Assets[id] = asset
	if c, ok := a.store.db.Companies[cid]; ok {
		c.Version++
		a.store.db.Companies[cid] = c
	}
	e = a.store.saveLocked()
	a.store.mu.Unlock()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	if isDoc {
		go a.indexAsset(id)
	}
	reply(w, asset)
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func (a *App) assetAction(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/assets/"), "/")
	a.store.mu.RLock()
	asset, ok := a.store.db.Assets[p[0]]
	a.store.mu.RUnlock()
	if !ok {
		fail(w, 404, "Arquivo não encontrado")
		return
	}
	if r.Method == "DELETE" {
		a.store.mu.Lock()
		delete(a.store.db.Assets, asset.ID)
		for k, c := range a.store.db.Chunks {
			if c.AssetID == asset.ID {
				delete(a.store.db.Chunks, k)
			}
		}
		_ = a.store.saveLocked()
		a.store.mu.Unlock()
		reply(w, map[string]any{"ok": true, "message": "Removido da consulta. Cópias de projetos e backups anteriores são preservadas."})
		return
	}
	if len(p) > 1 && p[1] == "reindex" && r.Method == "POST" {
		go a.indexAsset(asset.ID)
		reply(w, map[string]bool{"ok": true})
		return
	}
	if len(p) > 1 && p[1] == "content" {
		ext := strings.ToLower(filepath.Ext(asset.Path))
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
		if !contains([]string{".png", ".jpg", ".jpeg", ".webp", ".svg", ".mp4", ".webm", ".mp3", ".wav", ".ttf", ".otf", ".woff2"}, ext) {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": asset.Name}))
		}
		http.ServeFile(w, r, asset.Path)
		return
	}
	reply(w, asset)
}
func (a *App) memories(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.mu.RLock()
		defer a.store.mu.RUnlock()
		out := []Memory{}
		cid := r.URL.Query().Get("company_id")
		for _, m := range a.store.db.Memories {
			if m.CompanyID == cid || m.Scope == "personal" {
				out = append(out, m)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
		reply(w, out)
		return
	}
	if r.Method != "POST" && r.Method != "PUT" {
		fail(w, 405, "Use GET ou POST")
		return
	}
	var m Memory
	if body(r, &m) != nil || strings.TrimSpace(m.Text) == "" {
		fail(w, 400, "Informe a memória")
		return
	}
	if m.Scope == "personal" {
		m.CompanyID = ""
		m.ProjectID = ""
	} else if _, ok := a.store.company(m.CompanyID); !ok {
		fail(w, 400, "Selecione a empresa")
		return
	}
	if m.ProjectID != "" {
		p, ok := a.store.project(m.ProjectID)
		if !ok || p.CompanyID != m.CompanyID {
			fail(w, 403, "Projeto de outra empresa")
			return
		}
		m.Scope = "project"
	} else if m.Scope != "personal" {
		m.Scope = "company"
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if m.ID == "" {
		m.ID = newID()
		m.Created = now()
	} else {
		old, ok := a.store.db.Memories[m.ID]
		if !ok || old.CompanyID != m.CompanyID {
			fail(w, 404, "Memória não encontrada")
			return
		}
		m.Created = old.Created
	}
	m.Updated = now()
	m.Source = "Edição do usuário"
	a.store.db.Memories[m.ID] = m
	delete(a.store.db.Forgotten, forgottenKey(m.CompanyID, m.Text))
	_ = a.store.saveLocked()
	reply(w, m)
}
func (a *App) memoryAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		fail(w, 405, "Use DELETE")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/memories/")
	a.store.mu.Lock()
	if m, ok := a.store.db.Memories[id]; ok {
		a.store.db.Forgotten[forgottenKey(m.CompanyID, m.Text)] = true
	}
	delete(a.store.db.Memories, id)
	_ = a.store.saveLocked()
	a.store.mu.Unlock()
	reply(w, map[string]any{"ok": true, "message": "Esquecida nas próximas consultas. Backups já exportados continuam contendo seu histórico."})
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		reply(w, a.store.settings())
		return
	}
	if r.Method != "PUT" && r.Method != "POST" {
		fail(w, 405, "Use PUT")
		return
	}
	s := a.store.settings()
	if body(r, &s) != nil {
		fail(w, 400, "Configurações inválidas")
		return
	}
	a.store.mu.Lock()
	a.store.db.Settings = s
	_ = a.store.saveLocked()
	a.store.mu.Unlock()
	reply(w, s)
}
func (a *App) pick(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	var v map[string]any
	if body(r, &v) != nil {
		fail(w, 400, "Dados inválidos")
		return
	}
	if runtime.GOOS != "windows" {
		fail(w, 501, "O seletor nativo será disponibilizado no Windows. Neste ambiente de desenvolvimento, use a importação de arquivos.")
		return
	}
	script := `Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.Form; $f.TopMost=$true; $d=New-Object System.Windows.Forms.OpenFileDialog; $d.Title='Selecionar arquivo'; if($d.ShowDialog($f) -eq 'OK'){[Console]::OutputEncoding=[Text.Encoding]::UTF8; Write-Output $d.FileName}`
	if str(v, "kind") == "folder" {
		script = `Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.Form; $f.TopMost=$true; $d=New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description='Selecionar pasta para o Ateliê'; if($d.ShowDialog($f) -eq 'OK'){[Console]::OutputEncoding=[Text.Encoding]::UTF8; Write-Output $d.SelectedPath}`
	}
	out, e := powershellDialog(script)
	if e != nil {
		fail(w, 500, "O Windows não permitiu abrir o seletor. Verifique a política do computador com o TI.")
		return
	}
	reply(w, map[string]string{"path": strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff"))})
}
func (a *App) setupGet(w http.ResponseWriter, r *http.Request) {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	reply(w, a.setup)
}
func (a *App) setupCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	a.installMu.Lock()
	if a.installCancel != nil {
		a.installCancel()
	}
	a.installMu.Unlock()
	reply(w, map[string]bool{"ok": true})
}
func (a *App) setupStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	var v struct {
		DataDir string `json:"data_dir"`
		License bool   `json:"remotion_license_ack"`
	}
	if body(r, &v) != nil {
		fail(w, 400, "Dados inválidos")
		return
	}
	if !v.License {
		fail(w, 400, "Revise a elegibilidade e os termos do Remotion antes de instalar.")
		return
	}
	if runtime.GOOS != "windows" {
		fail(w, 400, "Este instalador prepara Windows x64. O ambiente atual é de desenvolvimento; a instalação não foi executada.")
		return
	}
	a.installMu.Lock()
	if a.setup.Status == "running" {
		a.installMu.Unlock()
		fail(w, 409, "A instalação já está em andamento")
		return
	}
	if v.DataDir != "" && filepath.Clean(v.DataDir) != a.root {
		a.store.mu.RLock()
		already := a.store.db.Installed
		a.store.mu.RUnlock()
		if already {
			a.installMu.Unlock()
			fail(w, 400, "Para mover uma instalação existente, exporte o backup e use uma nova instalação. Os dados atuais foram preservados.")
			return
		}
		newRoot, e := filepath.Abs(v.DataDir)
		if e != nil {
			a.installMu.Unlock()
			fail(w, 400, e.Error())
			return
		}
		newRoot = filepath.Join(newRoot, "AtelieLocal")
		if e = a.relocate(newRoot); e != nil {
			a.installMu.Unlock()
			fail(w, 500, e.Error())
			return
		}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.installCancel = cancel
	a.setup = SetupStatus{Status: "running", Stage: "preflight", Message: "Preparando instalação", Events: []InstallEvent{}}
	a.installMu.Unlock()
	a.store.mu.Lock()
	a.store.db.Settings.RemotionLicenseAck = true
	_ = a.store.saveLocked()
	a.store.mu.Unlock()
	go func() {
		cfg, e := RunInstall(ctx, a.root, func(ev InstallEvent) {
			a.installMu.Lock()
			a.setup.Stage = ev.Stage
			a.setup.Progress = ev.Progress
			a.setup.Message = ev.Message
			a.setup.Events = append(a.setup.Events, ev)
			if len(a.setup.Events) > 80 {
				a.setup.Events = a.setup.Events[len(a.setup.Events)-80:]
			}
			a.installMu.Unlock()
		})
		if e == nil {
			a.runtimeMu.Lock()
			a.runtime = cfg
			a.runtimeMu.Unlock()
			e = a.startRuntime()
		}
		if e == nil {
			e = a.createShortcut()
		}
		a.store.mu.Lock()
		if e == nil {
			a.store.db.Installed = true
		}
		_ = a.store.saveLocked()
		a.store.mu.Unlock()
		a.installMu.Lock()
		if e != nil {
			a.setup.Status = "failed"
			if ctx.Err() != nil {
				a.setup.Status = "cancelled"
			}
			a.setup.Error = e.Error()
			a.setup.Message = e.Error()
		} else {
			a.setup.Status = "completed"
			a.setup.Progress = 1
			a.setup.Message = "Instalação concluída. Abra uma empresa para começar."
		}
		a.installMu.Unlock()

	}()
	reply(w, map[string]bool{"started": true})
}
func (a *App) relocate(dest string) error {
	if _, e := os.Stat(filepath.Join(dest, "database.json")); e == nil {
		return fmt.Errorf("A pasta já contém outra instalação. Selecione uma pasta vazia.")
	}
	old := a.root
	if within(dest, old) || within(old, dest) {
		return fmt.Errorf("Escolha uma pasta independente da instalação atual")
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if e := copyTree(old, dest); e != nil {
		return e
	}
	for id, x := range a.store.db.Assets {
		if within(x.Path, old) {
			rel, _ := filepath.Rel(old, x.Path)
			x.Path = filepath.Join(dest, rel)
			a.store.db.Assets[id] = x
		}
	}
	a.root = dest
	a.store.root = dest
	if e := a.store.saveLocked(); e != nil {
		return e
	}
	return atomicJSON(filepath.Join(defaultRoot(), "location.json"), map[string]string{"Root": dest})
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		_, e = io.Copy(f, in)
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	})
}
func (a *App) createShortcut() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	dest := filepath.Join(a.root, "EstudioLocal.exe")
	if !strings.EqualFold(exe, dest) {
		b, e := os.ReadFile(exe)
		if e != nil {
			return e
		}
		if e = os.WriteFile(dest, b, 0700); e != nil {
			return e
		}
	}
	_, e = powershell(`$s=New-Object -ComObject WScript.Shell; $l=$s.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Desktop')) 'Estúdio Local.lnk')); $l.TargetPath=$env:ATELIE_EXE; $l.Arguments='--data-dir "'+$env:ATELIE_ROOT+'"'; $l.WorkingDirectory=$env:ATELIE_ROOT; $l.Save()`, "ATELIE_EXE="+dest, "ATELIE_ROOT="+a.root)
	return e
}
func (a *App) openPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	var v map[string]any
	if body(r, &v) != nil {
		fail(w, 400, "Dados inválidos")
		return
	}
	path := str(v, "path")
	if path == "" {
		path = filepath.Join(a.root, "projects")
	}
	p, e := canonical(path)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	allowed := within(p, a.root)
	if pid := str(v, "project_id"); pid != "" {
		proj, ok := a.store.project(pid)
		if ok {
			_, e := a.authorizePath(proj, p)
			allowed = allowed || e == nil
		}
	}
	if !allowed {
		fail(w, 403, "Selecione o arquivo no projeto antes de abrir")
		return
	}
	info, _ := os.Stat(p)
	if info != nil && !info.IsDir() {
		if !contains([]string{".mp4", ".webm", ".png", ".jpg", ".jpeg", ".wav", ".mp3", ".srt", ".vtt", ".txt", ".json", ".pdf"}, strings.ToLower(filepath.Ext(p))) {
			fail(w, 400, "Abra esta pasta pelo Explorador para acessar este tipo de arquivo")
			return
		}
	}
	if e = openExternal(p); e != nil {
		fail(w, 500, e.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}
func (a *App) outputFile(w http.ResponseWriter, r *http.Request) {
	p, ok := a.store.project(r.URL.Query().Get("project_id"))
	if !ok {
		fail(w, 404, "Projeto não encontrado")
		return
	}
	path, e := canonical(r.URL.Query().Get("path"))
	root := filepath.Join(a.root, "projects", p.ID)
	allowed := e == nil && within(path, root)
	if !allowed && e == nil && p.Source != "" {
		if source, se := a.authorizePath(p, p.Source); se == nil && source == path {
			allowed = true
		}
	}
	if !allowed {
		fail(w, 403, "Saída fora deste projeto")
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeFile(w, r, path)
}
func (a *App) diagnostics(w http.ResponseWriter, r *http.Request) {
	cfg := a.runtimeConfig()
	out := map[string]any{"version": appVersion, "os": runtime.GOOS, "arch": runtime.GOARCH, "runtime": cfg, "limitations": []string{"Esta compilação não foi executada na RTX 5060 Ti de destino.", "O seletor de empresa organiza dados; não autentica pessoas em uma conta Windows compartilhada."}}
	if b, e := os.ReadFile(filepath.Join(a.root, "logs", "readiness.json")); e == nil {
		var report any
		if json.Unmarshal(b, &report) == nil {
			out["readiness"] = diagnosticNumbers(report, "")
		}
	}
	if r.URL.Query().Get("export") == "1" {
		delete(out, "runtime")
		out["runtime_configured"] = cfg.Python != ""
		w.Header().Set("Content-Disposition", `attachment; filename="diagnostico-sem-conteudo.json"`)
	} else if cfg.OllamaURL != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var ps any
		if e := a.ollamaJSON(ctx, "/api/ps", nil, &ps); e == nil {
			out["models_loaded"] = ps
		} else {
			out["ollama_error"] = e.Error()
		}
	}
	if runtime.GOOS == "windows" && r.URL.Query().Get("export") != "1" {
		c, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		if target, e := InspectInstallTarget(c, a.root); e == nil {
			for k, v := range target {
				out[k] = v
			}
		} else {
			out["inspection_error"] = e.Error()
		}
	}
	reply(w, out)
}

// Keep measured counters and a small set of non-content labels; never export
// document/transcript text, arbitrary logs, personal paths or credentials.
func diagnosticNumbers(v any, key string) any {
	switch x := v.(type) {
	case bool, float64:
		return x
	case string:
		if contains([]string{"at", "platform", "model", "kind", "device", "compute_type"}, key) && !strings.ContainsAny(x, "/\\@") {
			return x
		}
	case map[string]any:
		m := map[string]any{}
		for k, child := range x {
			if val := diagnosticNumbers(child, k); val != nil {
				m[k] = val
			}
		}
		if len(m) > 0 {
			return m
		}
	case []any:
		a := []any{}
		for _, child := range x {
			if val := diagnosticNumbers(child, key); val != nil {
				a = append(a, val)
			}
		}
		if len(a) > 0 {
			return a
		}
	}
	return nil
}
func (a *App) backup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	a.store.mu.RLock()
	b, e := json.MarshalIndent(a.store.db, "", "  ")
	a.store.mu.RUnlock()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="atelie-backup-dados.zip"`)
	z := zip.NewWriter(w)
	f, _ := z.Create("database.json")
	_, _ = f.Write(b)
	f, _ = z.Create("LEIA-ME.txt")
	_, _ = f.Write([]byte("Backup de cadastros, conversas, índice e memória. Contém dados privados. Não contém mídia/fontes originais nem dependências. Restaurar requer os arquivos locais correspondentes. Memórias esquecidas depois desta exportação continuam neste arquivo."))
	_ = z.Close()
}

var _ = exec.Command

func (a *App) saveTranscript(w http.ResponseWriter, r *http.Request, pid string) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	p, ok := a.store.project(pid)
	if !ok {
		fail(w, 404, "Projeto não encontrado")
		return
	}
	var v struct {
		Path       string         `json:"path"`
		Transcript map[string]any `json:"transcript"`
	}
	if body(r, &v) != nil {
		fail(w, 400, "Transcrição inválida")
		return
	}
	path, e := canonical(v.Path)
	if e != nil || !within(path, filepath.Join(a.root, "projects", pid)) {
		fail(w, 403, "Transcrição fora deste projeto")
		return
	}
	if filepath.Ext(path) != ".json" {
		fail(w, 400, "Formato inválido")
		return
	}
	segments, ok := v.Transcript["segments"].([]any)
	if !ok {
		fail(w, 400, "Transcrição sem segmentos")
		return
	}
	for _, item := range segments {
		seg, ok := item.(map[string]any)
		if !ok || number(seg["start"]) < 0 || number(seg["end"]) < number(seg["start"]) {
			fail(w, 400, "Tempos inválidos na transcrição")
			return
		}
	}
	original, e := os.ReadFile(path)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	backup := path + ".before-" + newID() + ".json"
	if e = os.WriteFile(backup, original, 0600); e != nil {
		fail(w, 500, e.Error())
		return
	}
	if e = atomicJSON(path, v.Transcript); e != nil {
		fail(w, 500, e.Error())
		return
	}
	a.rememberProject(p.ID, "Transcrição revisada pelo usuário: "+path, "Edição manual")
	reply(w, map[string]any{"ok": true, "path": path, "previous_revision": backup})
}

func (a *App) player(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/player/")
	if name != "player.html" && name != "player.bundle.js" {
		fail(w, 404, "Prévia indisponível")
		return
	}
	cfg := a.runtimeConfig()
	if cfg.RemotionDir == "" {
		fail(w, 400, "Conclua a instalação do renderizador")
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; object-src 'none'")
	http.ServeFile(w, r, filepath.Join(cfg.RemotionDir, "dist", name))
}
func (a *App) jobAsset(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/job-assets/"), "/", 2)
	if len(parts) != 2 {
		fail(w, 400, "Asset inválido")
		return
	}
	a.store.mu.RLock()
	j, ok := a.store.db.Jobs[parts[0]]
	a.store.mu.RUnlock()
	if !ok {
		fail(w, 404, "Trabalho não encontrado")
		return
	}
	base := filepath.Join(j.OutputDir, "motion-assets")
	p, e := canonical(filepath.Join(base, parts[1]))
	root, re := canonical(base)
	if e != nil || re != nil || !within(p, root) {
		fail(w, 403, "Asset fora do projeto")
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeFile(w, r, p)
}
