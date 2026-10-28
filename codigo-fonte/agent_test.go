package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSimpleChatStreamingAndSettings(t *testing.T) {
	a, h := newIntegrationApp(t)
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["model"] != "qwen3.5:4b" {
			t.Error("simple greeting did not select fast model")
		}
		if p["think"] != false {
			t.Error("reasoning not disabled")
		}
		if _, ok := p["tools"]; ok {
			t.Error("greeting loaded tools")
		}
		if p["options"].(map[string]any)["num_ctx"] != float64(2048) {
			t.Error("oversized simple context")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte("{\"message\":{\"role\":\"assistant\",\"content\":\"Olá!\"},\"done\":false}\n{\"message\":{\"role\":\"assistant\",\"content\":\" Tudo bem.\"},\"done\":true,\"eval_count\":4,\"eval_duration\":1000000}\n"))
	}))
	defer model.Close()
	a.runtime = RuntimeConfig{OllamaURL: model.URL}
	a.store.db.Companies["c"] = Company{ID: "c", Name: "Teste"}
	a.store.db.Projects["p"] = Project{ID: "p", CompanyID: "c"}
	r := integrationRequest(t, a, h, "POST", "/api/chat", map[string]any{"company_id": "c", "project_id": "p", "message": "oi, tudo bem?"})
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Olá!") || !strings.Contains(r.Body.String(), `"type":"done"`) {
		t.Fatal(r.Code, r.Body.String())
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	p, _ := a.store.project("p")
	if len(p.Messages) != 2 {
		t.Fatal("chat not persisted")
	}
}

func TestAgentWaitReleasesGPUForDependentJob(t *testing.T) {
	a, h := newIntegrationApp(t)
	a.store.db.Companies["c"] = Company{ID: "c", Name: "X"}
	a.store.db.Projects["p"] = Project{ID: "p", CompanyID: "c"}
	a.store.db.Jobs["j"] = Job{ID: "j", ProjectID: "p", CompanyID: "c", Status: "queued"}
	trigger := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		<-trigger
		if !lockWithContext(a.ctx, &a.chatMu) {
			return
		}
		a.updateJob("j", func(j *Job) { j.Status = "completed" })
		a.chatMu.Unlock()
	}()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			close(trigger)
			w.Write([]byte(`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"job_status","arguments":{"job_id":"j"}}}]},"done":true}` + "\n"))
		} else {
			w.Write([]byte(`{"message":{"role":"assistant","content":"Etapa concluída."},"done":true}` + "\n"))
		}
	}))
	defer server.Close()
	a.runtime = RuntimeConfig{OllamaURL: server.URL}
	w := integrationRequest(t, a, h, "POST", "/api/chat", map[string]any{"company_id": "c", "project_id": "p", "message": "Leia a transcrição do vídeo"})
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker could not acquire GPU lease")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Etapa concluída") || calls != 2 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
}

func TestGreetingWithTaskUsesAgent(t *testing.T) {
	for _, s := range []string{"oi, crie um vídeo", "bom dia, leia o arquivo", "olá, pesquise as referências", "obrigado, exporte o projeto"} {
		if isSimple(s) {
			t.Errorf("task incorrectly routed to greeting model: %s", s)
		}
	}
	if !isSimple("oi, tudo bem?") {
		t.Fatal("ordinary greeting lost fast route")
	}
}

func TestPublicSearchDoesNotIntroduceDocumentTerms(t *testing.T) {
	if !publicQueryFromUser("Remotion captions", "Pesquise Remotion captions na web") {
		t.Fatal("explicit public query refused")
	}
	for _, q := range []string{"Remotion contrato interno secreto", "password example", "me@company.com", "C:\\Users\\Private", "123456789"} {
		if publicQueryFromUser(q, "Pesquise Remotion na web") {
			t.Fatal("private/unrequested query accepted", q)
		}
	}
}

func TestDiagnosticExportOmitsPathsContentAndCredentials(t *testing.T) {
	v := map[string]any{"model": "qwen3.5:4b", "ttft_ms": 12.5, "path": "C:\\Users\\Ana\\private.mp4", "text": "conteúdo privado", "token": "secret", "nested": map[string]any{"device": "cuda", "success": true}}
	b, _ := json.Marshal(diagnosticNumbers(v, ""))
	s := string(b)
	if strings.Contains(s, "private") || strings.Contains(s, "secret") || strings.Contains(s, "conteúdo") || !strings.Contains(s, "cuda") || !strings.Contains(s, "12.5") {
		t.Fatal(s)
	}
}

func TestMotionPlayerAssetRoute(t *testing.T) {
	a, h := newIntegrationApp(t)
	dir := filepath.Join(a.root, "projects", "p", "outputs", "j")
	assets := filepath.Join(dir, "motion-assets")
	if e := os.MkdirAll(assets, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(assets, "logo.png"), []byte("logo-bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	a.store.db.Jobs["j"] = Job{ID: "j", ProjectID: "p", CompanyID: "c", OutputDir: dir}
	r := integrationRequest(t, a, h, "GET", "/api/job-assets/j/logo.png", nil)
	if r.Code != 200 || r.Body.String() != "logo-bytes" {
		t.Fatal(r.Code, r.Body.String())
	}
	r = integrationRequest(t, a, h, "GET", "/api/job-assets/j/%2E%2E%2F%2E%2E%2Fdatabase.json", nil)
	if r.Code == 200 {
		t.Fatal("asset traversal accepted")
	}
}

func TestProjectPreviewAuthorizesOriginalAndRejectsUnrelatedFile(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Preview")
	file := filepath.Join(a.root, "companies", c.ID, "source.mp4")
	if e := os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(file, []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	p := integrationProject(t, a, h, c.ID, map[string]any{"source": file, "scope": "files"})
	r := integrationRequest(t, a, h, "GET", "/api/file?project_id="+p.ID+"&path="+url.QueryEscape(file), nil)
	if r.Code != 200 || r.Body.String() != "original" {
		t.Fatal(r.Code, r.Body.String())
	}
	other := filepath.Join(filepath.Dir(file), "unrelated.txt")
	os.WriteFile(other, []byte("unrelated"), 0600)
	r = integrationRequest(t, a, h, "GET", "/api/file?project_id="+p.ID+"&path="+url.QueryEscape(other), nil)
	if r.Code == 200 {
		t.Fatal("unrelated preview allowed")
	}
}

func TestExplicitBrandRefreshPreservesVideoChoices(t *testing.T) {
	a, h := newIntegrationApp(t)
	c := integrationCompany(t, a, h, "Brand")
	p := integrationProject(t, a, h, c.ID, map[string]any{"duration": 17, "aspect": "4:5", "audio": map[string]any{"mode": "none"}, "briefing": "Individual"})
	c.Version++
	c.Colors = []Color{{Hex: "#123456", Role: "primary"}}
	a.store.db.Companies[c.ID] = c
	r := integrationRequest(t, a, h, "PUT", "/api/projects/"+p.ID, map[string]any{"use_latest_brand": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	updated, _ := a.store.project(p.ID)
	if updated.Aspect != "4:5" || number(updated.Duration) != 17 || updated.Briefing != "Individual" || len(updated.Revisions) != 1 {
		t.Fatal("brand refresh changed project choices or lost previous brand")
	}
}
func TestAutomaticMemoryScopesAndDisable(t *testing.T) {
	a, _ := newIntegrationApp(t)
	a.store.db.Companies["c"] = Company{ID: "c", Name: "X"}
	p := Project{ID: "p", CompanyID: "c"}
	a.store.db.Projects["p"] = p
	a.autoMemory(p, "Prefiro títulos curtos para esta marca.")
	a.autoMemory(p, "Lembre: este vídeo tem duração de 20 segundos.")
	if len(a.store.db.Memories) != 2 {
		t.Fatal("no automatic memory")
	}
	for _, m := range a.store.db.Memories {
		if strings.Contains(m.Text, "segundos") && m.Scope != "project" {
			t.Fatal("video duration promoted to company")
		}
	}
	a.store.db.Settings.MemoryEnabled = false
	a.autoMemory(p, "Sempre use verde")
	if len(a.store.db.Memories) != 2 {
		t.Fatal("memory toggle ignored")
	}
}
func TestToolRejectsOtherCompanyJobAndArbitraryCode(t *testing.T) {
	a, _ := newIntegrationApp(t)
	a.store.db.Jobs["foreign"] = Job{ID: "foreign", ProjectID: "q", CompanyID: "b"}
	p := Project{ID: "p", CompanyID: "a"}
	call := ToolCall{}
	call.Function.Name = "job_status"
	call.Function.Arguments = map[string]any{"job_id": "foreign"}
	if _, _, e := a.executeTool(context.Background(), p, call, "teste"); e == nil {
		t.Fatal("foreign job leaked")
	}
	call.Function.Name = "run_shell"
	if _, _, e := a.executeTool(context.Background(), p, call, "teste"); e == nil {
		t.Fatal("shell available")
	}
}
func TestBuildBrandReusesFontForMultipleRoles(t *testing.T) {
	c := Company{Name: "X", FontRoles: map[string]string{"heading": "f", "body": "f", "caption": "f"}, Assets: []Asset{{ID: "f", Kind: "font", Path: "font.ttf", Family: "Brand"}}}
	b := buildBrand(c)
	if len(b["fonts"].([]map[string]any)) != 3 {
		t.Fatal("font roles lost")
	}
}
