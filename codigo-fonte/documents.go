package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

type SearchHit struct {
	AssetID string  `json:"asset_id"`
	Title   string  `json:"title"`
	Kind    string  `json:"kind"`
	Page    int     `json:"page"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	URL     string  `json:"url"`
	Created string  `json:"created"`
}

func (a *App) indexPending() {
	a.store.mu.RLock()
	ids := []string{}
	for id, x := range a.store.db.Assets {
		needs := x.Status == "pending" || x.Status == "failed" || x.Status == "indexing"
		if x.Status == "ready" {
			for _, ch := range a.store.db.Chunks {
				if ch.AssetID == id && len(ch.Vector) == 0 {
					needs = true
					break
				}
			}
		}
		if needs {
			ids = append(ids, id)
		}
	}
	a.store.mu.RUnlock()
	for _, id := range ids {
		a.indexAsset(id)
	}
}
func splitText(text string, maxChars int) []string {
	r := []rune(text)
	out := []string{}
	for len(r) > 0 {
		n := maxChars
		if len(r) < n {
			n = len(r)
		}
		part := strings.TrimSpace(string(r[:n]))
		if part != "" {
			out = append(out, part)
		}
		if n == len(r) {
			break
		}
		advance := n - 180
		if advance < 1 {
			advance = n
		}
		r = r[advance:]
	}
	return out
}
func (a *App) indexAsset(id string) {
	a.docMu.Lock()
	defer a.docMu.Unlock()
	a.store.mu.RLock()
	asset, ok := a.store.db.Assets[id]
	a.store.mu.RUnlock()
	if !ok {
		return
	}
	setStatus := func(status, err string) {
		a.store.mu.Lock()
		if x, ok := a.store.db.Assets[id]; ok {
			x.Status = status
			x.Error = err
			a.store.db.Assets[id] = x
			_ = a.store.saveLocked()
		}
		a.store.mu.Unlock()
	}
	setStatus("indexing", "")
	info, e := os.Stat(asset.Path)
	if e != nil {
		setStatus("failed", e.Error())
		return
	}
	if info.Size() > 50<<20 {
		setStatus("failed", "Documento acima de 50 MB. Divida-o em partes para indexar.")
		return
	}
	cfg := a.runtimeConfig()
	var chunks []Chunk
	ext := strings.ToLower(filepath.Ext(asset.Path))
	if ext == ".pdf" || ext == ".docx" {
		if cfg.Python == "" {
			setStatus("pending", "A extração será feita depois de instalar as ferramentas.")
			return
		}
		dir := filepath.Join(a.root, "indexes", id)
		_ = os.MkdirAll(dir, 0700)
		req := map[string]any{"action": "extract_document", "input": asset.Path, "output_dir": dir, "allowed_roots": []string{filepath.Dir(asset.Path)}}
		reqpath := filepath.Join(dir, "request.json")
		_ = atomicJSON(reqpath, req)
		ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, cfg.Python, filepath.Join(a.root, "assets", "media", "worker.py"), "--request", reqpath)
		HideManagedProcessWindow(cmd)
		cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
		output, e := cmd.Output()
		if e != nil {
			setStatus("failed", "Não foi possível extrair texto do documento.")
			return
		}
		scanner := bufio.NewScanner(bytes.NewReader(output))
		scanner.Buffer(make([]byte, 4096), 16<<20)
		for scanner.Scan() {
			var ev struct {
				Type   string `json:"type"`
				Chunks []struct {
					Text string `json:"text"`
					Page int    `json:"page"`
				} `json:"chunks"`
				Message string `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &ev) != nil {
				continue
			}
			if ev.Type == "result" {
				for _, ch := range ev.Chunks {
					for _, part := range splitText(ch.Text, 1600) {
						chunks = append(chunks, Chunk{ID: newID(), AssetID: id, CompanyID: asset.CompanyID, Text: part, Page: ch.Page})
					}
				}
			}
		}
	} else {
		b, e := os.ReadFile(asset.Path)
		if e != nil {
			setStatus("failed", e.Error())
			return
		}
		for _, part := range splitText(string(b), 1600) {
			chunks = append(chunks, Chunk{ID: newID(), AssetID: id, CompanyID: asset.CompanyID, Text: part, Page: 1})
		}
	}
	if len(chunks) == 0 {
		setStatus("failed", "Nenhum texto extraído. PDF digitalizado requer OCR; este pacote não inclui OCR.")
		return
	}
	if len(chunks) > 3000 {
		chunks = chunks[:3000]
	}
	for i := range chunks {
		if a.ctx.Err() != nil {
			return
		}
		if cfg.OllamaURL != "" {
			ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
			v, e := a.embed(ctx, "title: "+asset.Name+" | text: "+chunks[i].Text)
			cancel()
			if e == nil {
				chunks[i].Vector = v
			}
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if _, ok := a.store.db.Assets[id]; !ok {
		return
	}
	for key, c := range a.store.db.Chunks {
		if c.AssetID == id {
			delete(a.store.db.Chunks, key)
		}
	}
	for _, ch := range chunks {
		a.store.db.Chunks[ch.ID] = ch
	}
	x := a.store.db.Assets[id]
	x.Status = "ready"
	x.Error = ""
	a.store.db.Assets[id] = x
	_ = a.store.saveLocked()
}
func (a *App) ollamaJSON(ctx context.Context, path string, input any, out any) error {
	cfg := a.runtimeConfig()
	if path == "/api/embed" {
		a.runtimeMu.RLock()
		cfg.OllamaURL = a.embeddingURL
		a.runtimeMu.RUnlock()
	}
	if cfg.OllamaURL == "" {
		return errors.New("Modelo local não iniciado. Conclua a instalação ou use Reparar")
	}
	method := "GET"
	var reader io.Reader
	if input != nil {
		method = "POST"
		b, e := json.Marshal(input)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, cfg.OllamaURL+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return fmt.Errorf("O modelo local está indisponível: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("Ollama %d: %s", resp.StatusCode, b)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}
func (a *App) embed(ctx context.Context, text string) ([]float64, error) {
	var res struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	model := a.store.settings().EmbeddingModel
	if model == "" {
		model = "embeddinggemma"
	}
	e := a.ollamaJSON(ctx, "/api/embed", map[string]any{"model": model, "input": text, "keep_alive": "30s", "options": map[string]any{"num_gpu": 0}}, &res)
	if e != nil {
		return nil, e
	}
	if len(res.Embeddings) == 0 {
		return nil, errors.New("Sem embedding")
	}
	return res.Embeddings[0], nil
}
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var sum, na, nb float64
	for i, x := range a {
		sum += x * b[i]
		na += x * x
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return sum / math.Sqrt(na*nb)
}
func (a *App) retrieve(ctx context.Context, cid, query string, limit int) []SearchHit {
	terms := words(query)
	var vec []float64
	if len(terms) > 1 {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		vec, _ = a.embed(c, "task: search result | query: "+query)
		cancel()
	}
	a.store.mu.RLock()
	defer a.store.mu.RUnlock()
	hits := []SearchHit{}
	for _, ch := range a.store.db.Chunks {
		company, exists := a.store.db.Companies[ch.CompanyID]
		if !exists || ch.CompanyID != cid && !company.Shared {
			continue
		}
		asset, ok := a.store.db.Assets[ch.AssetID]
		if !ok {
			continue
		}
		hay := strings.ToLower(ch.Text + " " + asset.Name)
		score := 0.0
		for _, term := range terms {
			if len([]rune(term)) >= 3 && strings.Contains(hay, term) {
				score += 1
			}
		}
		semantic := cosine(vec, ch.Vector)
		if score == 0 && semantic < .50 {
			continue
		}
		score += semantic * 3
		hits = append(hits, SearchHit{AssetID: asset.ID, Title: asset.Name, Kind: asset.Kind, Page: ch.Page, Text: ch.Text, Score: score, URL: "/api/assets/" + asset.ID + "/content", Created: asset.Created})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
func (a *App) search(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("company_id")
	if _, ok := a.store.company(cid); !ok {
		fail(w, 400, "Selecione uma empresa")
		return
	}
	reply(w, a.retrieve(r.Context(), cid, r.URL.Query().Get("q"), 8))
}

func (a *App) readDocument(ctx context.Context, p Project, path string) (map[string]any, error) {
	cfg := a.runtimeConfig()
	if cfg.Python == "" {
		return nil, errors.New("Conclua a instalação para ler PDF/DOCX")
	}
	dir := filepath.Join(a.root, "projects", p.ID, "document-reads", newID())
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	request := map[string]any{"action": "extract_document", "input": path, "output_dir": dir, "allowed_roots": a.allowedRoots(p)}
	rp := filepath.Join(dir, "request.json")
	if e := atomicJSON(rp, request); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Python, filepath.Join(a.root, "assets", "media", "worker.py"), "--request", rp)
	HideManagedProcessWindow(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
	out, e := cmd.Output()
	if e != nil {
		return nil, errors.New("Não foi possível extrair este documento. PDFs digitalizados precisam de OCR.")
	}
	result, e := parseInstallerWorkerResult(out)
	if e != nil {
		return nil, e
	}
	return result, nil
}
