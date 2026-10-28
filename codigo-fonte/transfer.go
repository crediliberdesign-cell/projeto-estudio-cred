package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const companyBundleCompressedLimit int64 = 512 << 20
const companyBundleExpandedLimit int64 = 1 << 30
const companyBundleEntryLimit = 5000

// The transfer DTO deliberately has no project settings, grants, histories or memory.
type companyTransferIdentity struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Instructions string            `json:"instructions"`
	Colors       []Color           `json:"colors"`
	FontRoles    map[string]string `json:"font_roles"`
}
type companyTransferAsset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Family string `json:"family,omitempty"`
	Role   string `json:"role,omitempty"`
}
type companyTransferManifest struct {
	Format  string                  `json:"format"`
	Version int                     `json:"version"`
	Created string                  `json:"created"`
	Company companyTransferIdentity `json:"company"`
	Assets  []companyTransferAsset  `json:"assets"`
}

func bundleAssetAllowed(asset Asset, projectMedia bool) bool {
	if projectMedia {
		return contains([]string{"logo", "font", "reference", "manual", "briefing", "instructions", "video", "audio", "file", "image"}, asset.Kind)
	}
	return asset.ProjectID == "" && contains([]string{"logo", "font", "reference", "manual", "briefing", "instructions"}, asset.Kind)
}
func validBundleFilename(name string) bool {
	if name == "" || len(name) > 512 || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, " .") != part {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if contains([]string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}, base) {
			return false
		}
	}
	return true
}
func (a *App) companyBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "Use GET")
		return
	}
	company, ok := a.store.company(r.URL.Query().Get("company_id"))
	if !ok {
		fail(w, 404, "Empresa não encontrada")
		return
	}
	include := r.URL.Query().Get("include_assets") == "1"
	projectMedia := r.URL.Query().Get("include_project_media") == "1"
	manifest := companyTransferManifest{Format: "estudio-local-company", Version: 1, Created: now(), Company: companyTransferIdentity{Name: company.Name, Description: company.Description, Instructions: company.Instructions, Colors: company.Colors, FontRoles: map[string]string{}}, Assets: []companyTransferAsset{}}
	// Snapshot role assignments; never transfer dangling IDs or assignments to foreign assets.
	selected := []Asset{}
	selectedIDs := map[string]bool{}
	if include {
		for _, asset := range company.Assets {
			if asset.CompanyID == company.ID && bundleAssetAllowed(asset, projectMedia) {
				selected = append(selected, asset)
				if asset.Kind == "font" {
					selectedIDs[asset.ID] = true
				}
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	for role, id := range company.FontRoles {
		if selectedIDs[id] {
			manifest.Company.FontRoles[role] = id
		}
	}
	tempRoot := filepath.Join(a.root, "transfers")
	if err := os.MkdirAll(tempRoot, 0700); err != nil {
		fail(w, 500, err.Error())
		return
	}
	temp, err := os.CreateTemp(tempRoot, "export-*.zip")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	limited := &bundleLimitWriter{writer: temp, left: companyBundleCompressedLimit}
	zw := zip.NewWriter(limited)
	budget := companyBundleExpandedLimit - int64(2<<20)
	ownRoot, rootErr := canonical(filepath.Join(a.root, "companies", company.ID))
	for _, asset := range selected {
		if r.Context().Err() != nil {
			zw.Close()
			return
		}
		source, e := canonical(asset.Path)
		if e != nil || rootErr != nil || !within(source, ownRoot) {
			zw.Close()
			fail(w, 400, "Um arquivo da empresa está fora da pasta gerenciada ou indisponível. Revise os arquivos antes de exportar.")
			return
		}
		stat, e := os.Stat(source)
		if e != nil || !stat.Mode().IsRegular() {
			zw.Close()
			fail(w, 400, "Arquivo inválido no cadastro da empresa")
			return
		}
		if stat.Size() > budget {
			zw.Close()
			fail(w, 413, "Pacote acima de 1 GB expandido. Exporte somente a identidade e documentos menores.")
			return
		}
		ext := strings.ToLower(filepath.Ext(source))
		entry := "assets/" + asset.ID + ext
		if !validBundleFilename(entry) {
			zw.Close()
			fail(w, 400, "Nome de arquivo inválido no cadastro")
			return
		}
		dst, e := zw.Create(entry)
		if e != nil {
			zw.Close()
			fail(w, 413, "Não foi possível criar o pacote dentro do limite de 512 MB.")
			return
		}
		file, e := os.Open(source)
		if e != nil {
			zw.Close()
			fail(w, 400, "Arquivo de origem indisponível")
			return
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(file, budget+1))
		file.Close()
		if e != nil || n > budget || n != stat.Size() {
			zw.Close()
			fail(w, 413, "Arquivo alterado ou pacote maior que o limite. Nenhum pacote parcial foi enviado.")
			return
		}
		budget -= n
		manifest.Assets = append(manifest.Assets, companyTransferAsset{ID: asset.ID, Name: asset.Name, Kind: asset.Kind, File: entry, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: n, Family: asset.Family, Role: asset.Role})
	}
	metadata, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || len(metadata) > 2<<20 {
		zw.Close()
		fail(w, 413, "Configuração da empresa excede o limite de transferência")
		return
	}
	mw, err := zw.Create("company.json")
	if err == nil {
		_, err = mw.Write(metadata)
	}
	if err != nil {
		zw.Close()
		fail(w, 413, "Pacote excedeu o limite de transferência")
		return
	}
	if err = zw.Close(); err != nil {
		fail(w, 413, "Pacote excedeu o limite de 512 MB comprimidos")
		return
	}
	if err = temp.Sync(); err != nil {
		fail(w, 500, "Não foi possível concluir o pacote")
		return
	}
	if _, err = temp.Seek(0, 0); err != nil {
		fail(w, 500, err.Error())
		return
	}
	stat, err := temp.Stat()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "empresa-" + company.ID + ".zip"}))
	w.Header().Set("Content-Length", fmt.Sprint(stat.Size()))
	_, _ = io.Copy(w, temp)
}

type bundleLimitWriter struct {
	writer io.Writer
	left   int64
}

func (w *bundleLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("limite comprimido excedido")
	}
	n, err := w.writer.Write(p)
	w.left -= int64(n)
	return n, err
}

func (a *App) importCompanyBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "Use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, companyBundleCompressedLimit+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "Envie o arquivo ZIP da empresa")
		return
	}
	tempRoot := filepath.Join(a.root, "transfers")
	if err = os.MkdirAll(tempRoot, 0700); err != nil {
		fail(w, 500, err.Error())
		return
	}
	temp, err := os.CreateTemp(tempRoot, "import-*.zip")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	archivePath := temp.Name()
	defer os.Remove(archivePath)
	received := false
	for {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			temp.Close()
			fail(w, 400, "Upload interrompido ou acima do limite")
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		if received {
			part.Close()
			temp.Close()
			fail(w, 400, "Envie somente um ZIP por importação")
			return
		}
		received = true
		n, e := io.Copy(temp, io.LimitReader(part, companyBundleCompressedLimit+1))
		part.Close()
		if e != nil || n > companyBundleCompressedLimit {
			temp.Close()
			fail(w, 413, "O ZIP deve ter no máximo 512 MB")
			return
		}
	}
	if err = temp.Close(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if !received {
		fail(w, 400, "Nenhum ZIP recebido")
		return
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		fail(w, 400, "O arquivo não é um ZIP válido")
		return
	}
	defer reader.Close()
	manifest, entries, err := validateCompanyBundle(reader)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	c := Company{ID: newID(), Name: strings.TrimSpace(manifest.Company.Name), Description: manifest.Company.Description, Instructions: manifest.Company.Instructions, Colors: manifest.Company.Colors, FontRoles: map[string]string{}, Folders: []string{}, Version: 1, Updated: now(), Shared: false}
	companyRoot := filepath.Join(a.root, "companies")
	if err = os.MkdirAll(companyRoot, 0700); err != nil {
		fail(w, 500, err.Error())
		return
	}
	stage, err := os.MkdirTemp(companyRoot, ".import-")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(stage)
	final := filepath.Join(companyRoot, c.ID)
	if _, err = os.Stat(final); !os.IsNotExist(err) {
		fail(w, 409, "Identificador de importação ocupado; tente novamente")
		return
	}
	if err = os.MkdirAll(filepath.Join(stage, "assets"), 0700); err != nil {
		fail(w, 500, err.Error())
		return
	}
	imported := []Asset{}
	remap := map[string]string{}
	fontIDs := map[string]bool{}
	remaining := companyBundleExpandedLimit
	for _, item := range manifest.Assets {
		if r.Context().Err() != nil {
			return
		}
		id := newID()
		ext := strings.ToLower(path.Ext(item.File))
		dest := filepath.Join(stage, "assets", id+ext)
		in, e := entries[item.File].Open()
		if e != nil {
			fail(w, 400, "Arquivo corrompido no ZIP")
			return
		}
		out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			in.Close()
			fail(w, 500, e.Error())
			return
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, remaining+1))
		in.Close()
		closeErr := out.Close()
		if e != nil || closeErr != nil || n > remaining || n != item.Bytes || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(item.SHA256) {
			fail(w, 400, "Integridade de um arquivo do ZIP divergiu do manifesto. A importação foi descartada.")
			return
		}
		remaining -= n
		status := "ready"
		if contains([]string{".pdf", ".docx", ".txt", ".md", ".json", ".csv"}, ext) {
			status = "pending"
		}
		asset := Asset{ID: id, CompanyID: c.ID, Name: item.Name, Kind: item.Kind, Path: filepath.Join(final, "assets", id+ext), SHA256: strings.ToLower(item.SHA256), Status: status, Created: now(), Family: item.Family, Role: item.Role}
		imported = append(imported, asset)
		remap[item.ID] = id
		if item.Kind == "font" {
			fontIDs[item.ID] = true
		}
	}
	warnings := []string{"A empresa foi importada com um novo cadastro. Pastas locais, compartilhamento, conversas, memórias e configurações de vídeos não são transferidos."}
	for role, oldID := range manifest.Company.FontRoles {
		if fontIDs[oldID] && remap[oldID] != "" {
			c.FontRoles[role] = remap[oldID]
		} else if oldID != "" {
			warnings = append(warnings, "Uma fonte de "+role+" não foi incluída; selecione-a no cadastro da empresa.")
		}
	}
	if err = os.Rename(stage, final); err != nil {
		fail(w, 500, "Não foi possível concluir a pasta da empresa: "+err.Error())
		return
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(final)
		}
	}()
	a.store.mu.Lock()
	if _, exists := a.store.db.Companies[c.ID]; exists {
		a.store.mu.Unlock()
		fail(w, 409, "O cadastro já existe; a importação não foi aplicada")
		return
	}
	a.store.db.Companies[c.ID] = c
	for _, asset := range imported {
		a.store.db.Assets[asset.ID] = asset
	}
	err = a.store.saveLocked()
	if err != nil {
		delete(a.store.db.Companies, c.ID)
		for _, asset := range imported {
			delete(a.store.db.Assets, asset.ID)
		}
	}
	a.store.mu.Unlock()
	if err != nil {
		fail(w, 500, "Não foi possível salvar o cadastro importado; a importação foi descartada: "+err.Error())
		return
	}
	committed = true
	c.Assets = imported
	reply(w, map[string]any{"company": c, "imported_assets": len(imported), "warnings": warnings})
	for _, asset := range imported {
		if asset.Status == "pending" {
			go a.indexAsset(asset.ID)
		}
	}
}
func validateCompanyBundle(reader *zip.ReadCloser) (companyTransferManifest, map[string]*zip.File, error) {
	var m companyTransferManifest
	entries := map[string]*zip.File{}
	caseNames := map[string]bool{}
	total := uint64(0)
	if len(reader.File) > companyBundleEntryLimit {
		return m, nil, errors.New("ZIP com arquivos demais")
	}
	for _, f := range reader.File {
		name := f.Name
		if f.FileInfo().IsDir() {
			name = strings.TrimSuffix(name, "/")
		}
		if !validBundleFilename(name) || f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
			return m, nil, errors.New("ZIP contém caminho ou link não permitido")
		}
		key := strings.ToLower(name)
		if caseNames[key] {
			return m, nil, errors.New("ZIP contém nomes de arquivo duplicados")
		}
		caseNames[key] = true
		if f.UncompressedSize64 > uint64(companyBundleExpandedLimit)-total {
			return m, nil, errors.New("ZIP excede 1 GB expandido")
		}
		total += f.UncompressedSize64
		if f.FileInfo().IsDir() {
			if name != "assets" {
				return m, nil, errors.New("ZIP contém pasta não reconhecida")
			}
			continue
		}
		entries[name] = f
	}
	metadata, ok := entries["company.json"]
	if !ok || metadata.UncompressedSize64 > 2<<20 {
		return m, nil, errors.New("Manifesto company.json ausente ou acima de 2 MB")
	}
	file, err := metadata.Open()
	if err != nil {
		return m, nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, (2<<20)+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return m, nil, errors.New("Manifesto inválido ou contém campos não reconhecidos")
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return m, nil, errors.New("Manifesto contém conteúdo adicional inválido")
	}
	if m.Format != "estudio-local-company" || m.Version != 1 {
		return m, nil, errors.New("Versão de pacote de empresa não suportada")
	}
	if strings.TrimSpace(m.Company.Name) == "" || len(m.Company.Name) > 500 || len(m.Company.Description) > 100000 || len(m.Company.Instructions) > 200000 || len(m.Company.Colors) > 128 || len(m.Company.FontRoles) > 128 {
		return m, nil, errors.New("Dados da empresa fora dos limites permitidos")
	}
	for i, col := range m.Company.Colors {
		hex := normalizedHex(col.Hex)
		if hex == "" {
			return m, nil, errors.New("Cor inválida no pacote da empresa")
		}
		m.Company.Colors[i].Hex = hex
	}
	ids := map[string]bool{}
	used := map[string]bool{"company.json": true}
	for _, asset := range m.Assets {
		if asset.ID == "" || len(asset.ID) > 128 || ids[asset.ID] || len(asset.Name) > 1000 || len(asset.Family) > 1000 || len(asset.Role) > 128 {
			return m, nil, errors.New("Cadastro de arquivo inválido ou duplicado")
		}
		ids[asset.ID] = true
		if !bundleAssetAllowed(Asset{Kind: asset.Kind}, true) || !validBundleFilename(asset.File) || !strings.HasPrefix(asset.File, "assets/") || strings.Count(asset.File, "/") != 1 || used[asset.File] {
			return m, nil, errors.New("Referência de arquivo não permitida")
		}
		entry, ok := entries[asset.File]
		if !ok || asset.Bytes < 0 || uint64(asset.Bytes) != entry.UncompressedSize64 || len(asset.SHA256) != 64 {
			return m, nil, errors.New("Arquivo ausente ou tamanho/hash inválido no manifesto")
		}
		if _, err = hex.DecodeString(asset.SHA256); err != nil {
			return m, nil, errors.New("Hash de arquivo inválido")
		}
		ext := strings.ToLower(path.Ext(asset.File))
		if asset.Kind == "font" && !contains([]string{".ttf", ".otf", ".woff2"}, ext) {
			return m, nil, errors.New("Extensão de fonte não permitida")
		}
		if asset.Kind == "logo" && !contains([]string{".png", ".svg", ".jpg", ".jpeg", ".webp"}, ext) {
			return m, nil, errors.New("Extensão de logo não permitida")
		}
		used[asset.File] = true
	}
	for name := range entries {
		if !used[name] {
			return m, nil, errors.New("ZIP contém arquivo não declarado no manifesto")
		}
	}
	return m, entries, nil
}
