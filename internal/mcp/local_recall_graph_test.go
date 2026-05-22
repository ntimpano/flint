package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// graphRecallMemStore extends filterMemStore with GraphRecallStore so
// MCP-level routing tests can prove that local_recall:
//
//   - falls through to plain Recall / RecallFiltered when NTCLI_FF_GRAPH
//     is unset (byte-identical legacy surface),
//   - routes to RecallGraphAware when NTCLI_FF_GRAPH=1 (graph path),
//   - forwards the optional `include_superseded` argument verbatim.
//
// Counter fields let assertions distinguish which path the MCP handler
// reached without poking SQLite.
type graphRecallMemStore struct {
	*filterMemStore

	graphCalls         int
	lastGraphOpts      app.RecallOptions
	graphResultContent string
}

func newGraphRecallMemStore() *graphRecallMemStore {
	return &graphRecallMemStore{
		filterMemStore:     &filterMemStore{memStore: newMemStore()},
		graphResultContent: "from-graph",
	}
}

func (g *graphRecallMemStore) RecallGraphAware(opts app.RecallOptions) ([]app.MemoryItem, error) {
	g.graphCalls++
	g.lastGraphOpts = opts
	// Return a single, distinguishable row so the test can verify the
	// payload came from the graph path (not the plain path).
	return []app.MemoryItem{{
		ID:      999,
		Content: g.graphResultContent,
	}}, nil
}

var _ app.Store = (*graphRecallMemStore)(nil)
var _ app.GraphRecallStore = (*graphRecallMemStore)(nil)
var _ app.FilterStore = (*graphRecallMemStore)(nil)

// TestLocalRecall_FFOff_UsesPlainPath proves the MCP local_recall tool
// keeps its legacy behavior with NTCLI_FF_GRAPH unset: even when the
// underlying store implements GraphRecallStore, the graph path MUST
// NOT be invoked. The plain memStore.Recall returns nil → empty JSON
// list — what we assert is the *route taken*, not the payload shape.
func TestLocalRecall_FFOff_UsesPlainPath(t *testing.T) {
	withGraphFlag(t, "")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha plain", CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "alpha"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if store.graphCalls != 0 {
		t.Fatalf("FF off: graph path must not be called, got %d", store.graphCalls)
	}
	// Plain path must NOT contain the graph-only sentinel content.
	if strings.Contains(text, "from-graph") {
		t.Fatalf("FF off: payload leaked from graph path: %q", text)
	}
}

// TestLocalRecall_FFOn_RoutesToGraphAware proves that with the flag
// ON, local_recall routes through the graph-aware path. The returned
// payload MUST be the one produced by RecallGraphAware (here:
// content "from-graph").
func TestLocalRecall_FFOn_RoutesToGraphAware(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha plain", CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "alpha"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if store.graphCalls != 1 {
		t.Fatalf("FF on: expected 1 graph call, got %d", store.graphCalls)
	}
	if !strings.Contains(text, "from-graph") {
		t.Fatalf("FF on: expected graph payload, got %q", text)
	}
	if store.lastGraphOpts.Query != "alpha" {
		t.Fatalf("FF on: expected query forwarded, got %q", store.lastGraphOpts.Query)
	}
}

// TestLocalRecall_FFOn_ForwardsIncludeSuperseded proves the MCP layer
// parses the optional `include_superseded` JSON arg and forwards it
// to the service / store. Triangulation case for the routing test.
func TestLocalRecall_FFOn_ForwardsIncludeSuperseded(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)

	payload := newCallReq(t, "local_recall", map[string]interface{}{
		"query":              "alpha",
		"include_superseded": true,
	})
	resp, _ := handleRequest(payload, svc)
	if _, isErr := toolPayloadText(t, resp); isErr {
		t.Fatalf("unexpected MCP error response")
	}
	if store.graphCalls != 1 {
		t.Fatalf("expected 1 graph call, got %d", store.graphCalls)
	}
	if !store.lastGraphOpts.IncludeSuperseded {
		t.Fatalf("expected include_superseded=true forwarded, got %+v", store.lastGraphOpts)
	}
}

// TestLocalRecall_FFOn_DefaultIncludeSupersededIsFalse triangulates
// the previous test: when the arg is omitted, the forwarded option
// must default to false. Forces real arg parsing instead of a Fake-It
// hardcoded `true`.
func TestLocalRecall_FFOn_DefaultIncludeSupersededIsFalse(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "alpha"})
	resp, _ := handleRequest(payload, svc)
	if _, isErr := toolPayloadText(t, resp); isErr {
		t.Fatalf("unexpected MCP error response")
	}
	if store.graphCalls != 1 {
		t.Fatalf("expected 1 graph call, got %d", store.graphCalls)
	}
	if store.lastGraphOpts.IncludeSuperseded {
		t.Fatalf("default: include_superseded must be false, got %+v", store.lastGraphOpts)
	}
}

// TestLocalRecall_ToolDescriptor_FFOff_NoIncludeSuperseded proves
// tools/list with the flag OFF does NOT advertise the
// `include_superseded` property — the legacy descriptor must be byte
// identical for clients that have not opted in.
func TestLocalRecall_ToolDescriptor_FFOff_NoIncludeSuperseded(t *testing.T) {
	withGraphFlag(t, "")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)

	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, _ := json.Marshal(req)
	resp, _ := handleRequest(payload, svc)
	tools := resp.Result.(map[string]interface{})["tools"].([]map[string]interface{})
	for _, tool := range tools {
		if tool["name"].(string) != "local_recall" {
			continue
		}
		schema := tool["inputSchema"].(map[string]interface{})
		props := schema["properties"].(map[string]interface{})
		if _, has := props["include_superseded"]; has {
			t.Fatalf("FF off: include_superseded must NOT be advertised, got props=%v", props)
		}
		return
	}
	t.Fatalf("local_recall not found in tools/list")
}

// TestLocalRecall_ToolDescriptor_FFOn_AdvertisesIncludeSuperseded
// triangulates the descriptor test — the property MUST appear when
// the flag is ON so clients can discover the new arg.
func TestLocalRecall_ToolDescriptor_FFOn_AdvertisesIncludeSuperseded(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphRecallMemStore()
	svc := app.NewService(store, nil)

	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, _ := json.Marshal(req)
	resp, _ := handleRequest(payload, svc)
	tools := resp.Result.(map[string]interface{})["tools"].([]map[string]interface{})
	for _, tool := range tools {
		if tool["name"].(string) != "local_recall" {
			continue
		}
		schema := tool["inputSchema"].(map[string]interface{})
		props := schema["properties"].(map[string]interface{})
		entry, has := props["include_superseded"]
		if !has {
			t.Fatalf("FF on: include_superseded MUST be advertised, got props=%v", props)
		}
		em := entry.(map[string]interface{})
		if em["type"] != "boolean" {
			t.Fatalf("FF on: include_superseded must be type=boolean, got %v", em)
		}
		return
	}
	t.Fatalf("local_recall not found in tools/list")
}
