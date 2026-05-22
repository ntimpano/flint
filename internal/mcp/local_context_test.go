package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// filterMemStore extends memStore with FilterStore + MetadataStore so
// MCP tests can drive local_context and the new local_recall filters.
type filterMemStore struct {
	*memStore
}

func newFilterMemStore() *filterMemStore {
	return &filterMemStore{memStore: newMemStore()}
}

func (f *filterMemStore) SaveWithMeta(req app.SaveRequest) (int64, error) {
	id := f.nextID
	f.nextID++
	stamp := req.CreatedAt
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}
	f.items[id] = app.MemoryItem{
		ID:        id,
		Content:   req.Content,
		CreatedAt: stamp,
		UpdatedAt: stamp,
		Title:     req.Title,
		Type:      req.Type,
		TopicKey:  req.TopicKey,
		Scope:     req.Scope,
	}
	return id, nil
}

func (f *filterMemStore) RecallFiltered(opts app.RecallOptions) ([]app.MemoryItem, error) {
	out := []app.MemoryItem{}
	for _, it := range f.items {
		if opts.Query != "" && !strings.Contains(it.Content, opts.Query) {
			continue
		}
		if opts.Type != "" && it.Type != opts.Type {
			continue
		}
		out = append(out, it)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

func (f *filterMemStore) Context(n int, scope string) ([]app.MemoryItem, error) {
	all := []app.MemoryItem{}
	for _, it := range f.items {
		if scope != "" && it.Scope != scope {
			continue
		}
		all = append(all, it)
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].CreatedAt.After(all[i].CreatedAt) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all, nil
}

// ContextFiltered satisfies FilterStore (PR2b) — stub.
func (f *filterMemStore) ContextFiltered(opts app.ContextOptions) ([]app.MemoryItem, error) {
	return f.Context(opts.N, opts.Scope)
}

// ListFiltered satisfies FilterStore (PR2b) — stub.
func (f *filterMemStore) ListFiltered(opts app.ListOptions) ([]app.MemoryItem, error) {
	return nil, nil
}

// TestToolsList_IncludesLocalContext proves the new context view is
// advertised via tools/list (spec scenario "Context returns recent
// items" — the surface MUST be reachable through MCP).
func TestToolsList_IncludesLocalContext(t *testing.T) {
	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, _ := json.Marshal(req)
	resp, ok := handleRequest(payload, nil)
	if !ok {
		t.Fatalf("expected response")
	}
	m := resp.Result.(map[string]interface{})
	tools := m["tools"].([]map[string]interface{})
	for _, tool := range tools {
		if tool["name"].(string) == "local_context" {
			schema := tool["inputSchema"].(map[string]interface{})
			props := schema["properties"].(map[string]interface{})
			if _, ok := props["n"]; !ok {
				t.Fatalf("local_context schema MUST advertise property 'n', got %v", props)
			}
			if _, ok := props["scope"]; !ok {
				t.Fatalf("local_context schema MUST advertise property 'scope', got %v", props)
			}
			return
		}
	}
	t.Fatalf("expected local_context tool to be advertised")
}

// TestLocalContext_ReturnsRecentN proves the local_context tool returns
// exactly N items newest-first.
func TestLocalContext_ReturnsRecentN(t *testing.T) {
	store := newFilterMemStore()
	svc := app.NewService(store, nil)

	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		if _, err := store.SaveWithMeta(app.SaveRequest{
			Content:   "ctx-" + string(rune('A'+i)),
			Scope:     "project",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	payload := newCallReq(t, "local_context", map[string]interface{}{"n": 3})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		t.Fatalf("expected JSON list, got %q (err %v)", text, err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d (%v)", len(items), items)
	}
	want := []string{"ctx-F", "ctx-E", "ctx-D"}
	for i, w := range want {
		if items[i]["content"].(string) != w {
			t.Fatalf("items[%d].content = %q, want %q", i, items[i]["content"], w)
		}
	}
}

// TestLocalContext_ScopeFilter narrows by scope.
func TestLocalContext_ScopeFilter(t *testing.T) {
	store := newFilterMemStore()
	svc := app.NewService(store, nil)
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []struct{ scope, content string }{
		{"project", "p1"}, {"personal", "x1"},
		{"project", "p2"}, {"personal", "x2"},
	}
	for i, p := range pairs {
		if _, err := store.SaveWithMeta(app.SaveRequest{
			Content: p.content, Scope: p.scope,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	payload := newCallReq(t, "local_context", map[string]interface{}{"n": 10, "scope": "personal"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		t.Fatalf("expected JSON list, got %q", text)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 personal items, got %d", len(items))
	}
	for _, it := range items {
		c := it["content"].(string)
		if c != "x1" && c != "x2" {
			t.Fatalf("unexpected non-personal item: %s", c)
		}
	}
}

// TestLocalRecall_TypeFilterNarrows proves the local_recall MCP tool
// accepts a `type` filter argument and narrows by metadata.
func TestLocalRecall_TypeFilterNarrows(t *testing.T) {
	store := newFilterMemStore()
	svc := app.NewService(store, nil)
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha decision", Type: "decision", CreatedAt: base,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha manual", Type: "manual", CreatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{
		"query": "alpha",
		"type":  "decision",
	})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		t.Fatalf("expected JSON list, got %q", text)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 decision row, got %d (%v)", len(items), items)
	}
	if items[0]["type"].(string) != "decision" {
		t.Fatalf("expected type=decision, got %v", items[0]["type"])
	}
}
