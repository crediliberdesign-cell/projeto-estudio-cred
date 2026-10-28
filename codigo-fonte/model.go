package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const appVersion = "1.0.0-preview.1"

type Color struct {
	Hex  string `json:"hex"`
	Name string `json:"name"`
	Role string `json:"role"`
}
type Company struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Instructions string            `json:"instructions"`
	Colors       []Color           `json:"colors"`
	FontRoles    map[string]string `json:"font_roles"`
	Folders      []string          `json:"folders"`
	Version      int               `json:"version"`
	Updated      string            `json:"updated"`
	Shared       bool              `json:"shared"`
	Assets       []Asset           `json:"assets,omitempty"`
	LogoURI      string            `json:"logo_uri,omitempty"`
}
type Asset struct {
	ID        string  `json:"id"`
	CompanyID string  `json:"company_id"`
	ProjectID string  `json:"project_id,omitempty"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Path      string  `json:"path"`
	SHA256    string  `json:"sha256"`
	Status    string  `json:"status"`
	Error     string  `json:"error,omitempty"`
	Created   string  `json:"created"`
	Family    string  `json:"family,omitempty"`
	Role      string  `json:"role,omitempty"`
	Chunks    []Chunk `json:"-"`
}
type Chunk struct {
	ID        string    `json:"id"`
	AssetID   string    `json:"asset_id"`
	CompanyID string    `json:"company_id"`
	Text      string    `json:"text"`
	Page      int       `json:"page"`
	Vector    []float64 `json:"vector,omitempty"`
}
type Project struct {
	ID            string           `json:"id"`
	CompanyID     string           `json:"company_id"`
	Title         string           `json:"title"`
	Briefing      string           `json:"briefing"`
	Aspect        string           `json:"aspect"`
	Duration      any              `json:"duration"`
	Audio         any              `json:"audio"`
	Resolution    any              `json:"resolution"`
	Source        string           `json:"source"`
	SourceAssetID string           `json:"source_asset_id"`
	Folder        string           `json:"folder"`
	Scope         string           `json:"scope"`
	Operations    []string         `json:"operations"`
	Attachments   []string         `json:"attachments"`
	Brand         map[string]any   `json:"brand"`
	Created       string           `json:"created"`
	Updated       string           `json:"updated"`
	Messages      []ChatMessage    `json:"messages"`
	Revisions     []map[string]any `json:"revisions"`
}
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Created string `json:"created,omitempty"`
}
type Memory struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	ProjectID string `json:"project_id,omitempty"`
	Scope     string `json:"scope"`
	Text      string `json:"text"`
	Source    string `json:"source"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
}
type Job struct {
	ID        string         `json:"id"`
	ProjectID string         `json:"project_id"`
	CompanyID string         `json:"company_id"`
	Action    string         `json:"action"`
	Status    string         `json:"status"`
	Stage     string         `json:"stage"`
	Progress  float64        `json:"progress"`
	Error     string         `json:"error,omitempty"`
	Options   map[string]any `json:"options"`
	Result    map[string]any `json:"result,omitempty"`
	OutputDir string         `json:"output_dir"`
	Created   string         `json:"created"`
	Updated   string         `json:"updated"`
}
type Settings struct {
	MemoryEnabled      bool   `json:"memory_enabled"`
	WebEnabled         bool   `json:"web_enabled"`
	ModelQuick         string `json:"model_quick"`
	ModelAgent         string `json:"model_agent"`
	EmbeddingModel     string `json:"embedding_model"`
	RemotionLicenseAck bool   `json:"remotion_license_ack"`
}
type Database struct {
	Forgotten map[string]bool    `json:"forgotten"`
	Schema    int                `json:"schema"`
	Companies map[string]Company `json:"companies"`
	Assets    map[string]Asset   `json:"assets"`
	Projects  map[string]Project `json:"projects"`
	Memories  map[string]Memory  `json:"memories"`
	Chunks    map[string]Chunk   `json:"chunks"`
	Jobs      map[string]Job     `json:"jobs"`
	Settings  Settings           `json:"settings"`
	Installed bool               `json:"installed"`
}
type Store struct {
	mu   sync.RWMutex
	root string
	db   Database
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
func newID() string {
	b := make([]byte, 12)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func openStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	s := &Store{root: root}
	p := filepath.Join(root, "database.json")
	if b, e := os.ReadFile(p); e == nil {
		if e = json.Unmarshal(b, &s.db); e != nil {
			return nil, errors.New("Banco local inválido. Preserve database.json e restaure um backup pela assistência técnica.")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	s.initialize()
	return s, nil
}
func (s *Store) initialize() {
	d := &s.db
	if d.Forgotten == nil {
		d.Forgotten = map[string]bool{}
	}
	if d.Schema == 0 {
		d.Schema = 1
		d.Settings = Settings{MemoryEnabled: true, WebEnabled: true, ModelQuick: "qwen3.5:4b", ModelAgent: "qwen3.5:9b", EmbeddingModel: "embeddinggemma"}
	}
	if d.Companies == nil {
		d.Companies = map[string]Company{}
	}
	if d.Assets == nil {
		d.Assets = map[string]Asset{}
	}
	if d.Projects == nil {
		d.Projects = map[string]Project{}
	}
	if d.Memories == nil {
		d.Memories = map[string]Memory{}
	}
	if d.Chunks == nil {
		d.Chunks = map[string]Chunk{}
	}
	if d.Jobs == nil {
		d.Jobs = map[string]Job{}
	}
}
func atomicJSON(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	tmp := path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return replaceFile(tmp, path)
}
func (s *Store) saveLocked() error { return atomicJSON(filepath.Join(s.root, "database.json"), s.db) }
func (s *Store) company(id string) (Company, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.db.Companies[id]
	c.Assets = []Asset{}
	for _, a := range s.db.Assets {
		if a.CompanyID == id {
			c.Assets = append(c.Assets, a)
			if a.Kind == "logo" {
				c.LogoURI = "/api/assets/" + a.ID + "/content"
			}
		}
	}
	return c, ok
}
func (s *Store) project(id string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.db.Projects[id]
	return p, ok
}
func (s *Store) settings() Settings { s.mu.RLock(); defer s.mu.RUnlock(); return s.db.Settings }
func cloneMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func normalizedHex(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(v, "#"))
	if len(v) == 3 {
		v = string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	if len(v) != 6 {
		return ""
	}
	if _, e := hex.DecodeString(v); e != nil {
		return ""
	}
	return "#" + strings.ToUpper(v)
}
func buildBrand(c Company) map[string]any {
	fonts := []map[string]any{}
	logos := []map[string]any{}
	for _, a := range c.Assets {
		if a.Kind == "font" {
			family := a.Family
			if family == "" {
				family = strings.TrimSuffix(a.Name, filepath.Ext(a.Name))
			}
			added := false
			for role, id := range c.FontRoles {
				if id == a.ID {
					fonts = append(fonts, map[string]any{"path": a.Path, "family": family, "role": role})
					added = true
				}
			}
			if !added {
				fonts = append(fonts, map[string]any{"path": a.Path, "family": family, "role": fontRole(c, a)})
			}
		}
		if a.Kind == "logo" {
			logos = append(logos, map[string]any{"path": a.Path})
		}
	}
	return map[string]any{"name": c.Name, "version": c.Version, "colors": c.Colors, "fonts": fonts, "logos": logos}
}
func fontRole(c Company, a Asset) string {
	for role, id := range c.FontRoles {
		if id == a.ID {
			return role
		}
	}
	if a.Role != "" {
		return a.Role
	}
	return "body"
}

func forgottenKey(cid, text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return cid + ":" + hex.EncodeToString(sum[:])
}
