package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// graphMemStore extends memStore with RelationStore so MCP tests can
// exercise the relate / graph_neighbors surface without booting SQLite.
// Mirrors sessionMemStore (M3) and metaMemStore (M1).
type graphMemStore struct {
	*memStore

	createCalls    int
	lastSrc        int64
	lastTgt        int64
	lastType       string
	createErr      error

	neighborsCalls int
	lastNbrID      int64
	lastDir        app.RelationDirection
	neighborsRet   []app.MemoryRelation
	neighborsErr   error
}

func newGraphMemStore() *graphMemStore { return &graphMemStore{memStore: newMemStore()} }

func (g *graphMemStore) CreateRelation(src, tgt int64, t string, _ time.Time) error {
	g.createCalls++
	g.lastSrc = src
	g.lastTgt = tgt
	g.lastType = t
	return g.createErr
}

func (g *graphMemStore) Neighbors(id int64, dir app.RelationDirection) ([]app.MemoryRelation, error) {
	g.neighborsCalls++
	g.lastNbrID = id
	g.lastDir = dir
	return g.neighborsRet, g.neighborsErr
}

var _ app.Store = (*graphMemStore)(nil)
var _ app.RelationStore = (*graphMemStore)(nil)

// withGraphFlag toggles NTCLI_FF_GRAPH for the duration of a test and
// restores the previous value on cleanup. Centralised so individual
// tests don't leak environment state into siblings.
func withGraphFlag(t *testing.T, value string) {
	t.Helper()
	prev, had := os.LookupEnv("NTCLI_FF_GRAPH")
	if value == "" {
		_ = os.Unsetenv("NTCLI_FF_GRAPH")
	} else {
		_ = os.Setenv("NTCLI_FF_GRAPH", value)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NTCLI_FF_GRAPH", prev)
		} else {
			_ = os.Unsetenv("NTCLI_FF_GRAPH")
		}
	})
}

// TestMCP_GraphTools_HiddenWhenFlagOff covers task 3.4:
// the graph tools default to OFF. Without NTCLI_FF_GRAPH=1, tools/list
// MUST NOT advertise relate / graph_neighbors so legacy clients see
// the same surface they had before this milestone.
func TestMCP_GraphTools_HiddenWhenFlagOff(t *testing.T) {
	withGraphFlag(t, "")
	store := newGraphMemStore()
	svc := app.NewService(store, nil)

	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, _ := json.Marshal(req)
	resp, _ := handleRequest(payload, svc)
	m := resp.Result.(map[string]interface{})
	tools := m["tools"].([]map[string]interface{})
	for _, tool := range tools {
		name := tool["name"].(string)
		if name == "relate" || name == "graph_neighbors" {
			t.Fatalf("flag-off: graph tool %q must NOT be advertised", name)
		}
	}
}

// TestMCP_GraphTools_AdvertisedWhenFlagOn covers task 3.4:
// with NTCLI_FF_GRAPH=1 the two graph tools are listed alongside the
// existing surface.
func TestMCP_GraphTools_AdvertisedWhenFlagOn(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphMemStore()
	svc := app.NewService(store, nil)

	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, _ := json.Marshal(req)
	resp, _ := handleRequest(payload, svc)
	m := resp.Result.(map[string]interface{})
	tools := m["tools"].([]map[string]interface{})
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	if !names["relate"] {
		t.Fatalf("flag-on: relate must be advertised; got %v", names)
	}
	if !names["graph_neighbors"] {
		t.Fatalf("flag-on: graph_neighbors must be advertised; got %v", names)
	}
}

// TestMCP_RelateCall_FlagOffRejects covers task 3.4:
// even if a client guesses the tool name, calling it with the flag OFF
// must produce a structured isError response — never a silent success
// nor a nil-method panic.
func TestMCP_RelateCall_FlagOffRejects(t *testing.T) {
	withGraphFlag(t, "")
	store := newGraphMemStore()
	svc := app.NewService(store, nil)

	result, _ := callTool(t, svc, "relate", map[string]interface{}{
		"source_id":     1,
		"target_id":     2,
		"relation_type": "related",
	})
	// tools/call falls through to "tool not found" when the case is
	// not registered, which surfaces as an rpcError on the response.
	// callTool returns rpcError separately, so result may be nil here.
	if result != nil {
		isErr, _ := result["isError"].(bool)
		if !isErr {
			t.Fatalf("flag-off relate must return error result, got %+v", result)
		}
	}
	if store.createCalls != 0 {
		t.Fatalf("store.CreateRelation must NOT be reached when flag is off")
	}
}

// TestMCP_RelateCall_HappyPath covers task 3.4 with the flag ON:
// valid args reach the service, the store records one CreateRelation
// call with the trimmed type, and the response is a non-error confirmation.
func TestMCP_RelateCall_HappyPath(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphMemStore()
	// Seed two memory rows so the foreign-key contract is plausible at
	// the service layer (memStore doesn't enforce FK, but seeding keeps
	// the test intent close to the real wiring).
	_, _ = store.Save("a", time.Now().UTC())
	_, _ = store.Save("b", time.Now().UTC())
	svc := app.NewService(store, nil)

	result, rpcErr := callTool(t, svc, "relate", map[string]interface{}{
		"source_id":     1,
		"target_id":     2,
		"relation_type": "  refines  ",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	isErr, _ := result["isError"].(bool)
	if isErr {
		text := result["content"].([]map[string]string)[0]["text"]
		t.Fatalf("expected success, got isError=true text=%q", text)
	}
	if store.createCalls != 1 {
		t.Fatalf("expected 1 CreateRelation, got %d", store.createCalls)
	}
	if store.lastSrc != 1 || store.lastTgt != 2 || store.lastType != "refines" {
		t.Fatalf("dispatch mismatch: src=%d tgt=%d type=%q", store.lastSrc, store.lastTgt, store.lastType)
	}
}

// TestMCP_RelateCall_ValidationErrorsSurface covers task 3.4:
// every service-layer validation rejection must reach the caller as a
// structured isError result (not a nil-content crash).
func TestMCP_RelateCall_ValidationErrorsSurface(t *testing.T) {
	withGraphFlag(t, "1")
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"missing source", map[string]interface{}{"target_id": 2, "relation_type": "related"}, "source"},
		{"self-loop", map[string]interface{}{"source_id": 5, "target_id": 5, "relation_type": "related"}, "self-loop"},
		{"unknown type", map[string]interface{}{"source_id": 1, "target_id": 2, "relation_type": "bogus"}, "not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newGraphMemStore()
			svc := app.NewService(store, nil)
			result, _ := callTool(t, svc, "relate", tc.args)
			if result == nil {
				t.Fatalf("expected result map, got nil")
			}
			isErr, _ := result["isError"].(bool)
			if !isErr {
				t.Fatalf("expected isError=true on %s", tc.name)
			}
			text := result["content"].([]map[string]string)[0]["text"]
			if !strings.Contains(strings.ToLower(text), tc.want) {
				t.Fatalf("error %q must mention %q", text, tc.want)
			}
		})
	}
}

// TestMCP_GraphNeighborsCall_HappyPath covers task 3.4:
// graph_neighbors with direction=outbound forwards to the store and
// renders the rows as a JSON array payload (one map per relation,
// snake-cased fields so MCP clients don't need Go-style key handling).
func TestMCP_GraphNeighborsCall_HappyPath(t *testing.T) {
	withGraphFlag(t, "1")
	store := newGraphMemStore()
	store.neighborsRet = []app.MemoryRelation{
		{ID: 7, SourceID: 1, TargetID: 2, RelationType: "related", CreatedAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)},
	}
	svc := app.NewService(store, nil)

	result, rpcErr := callTool(t, svc, "graph_neighbors", map[string]interface{}{
		"id":        1,
		"direction": "outbound",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	isErr, _ := result["isError"].(bool)
	if isErr {
		t.Fatalf("expected success, got error")
	}
	if store.neighborsCalls != 1 {
		t.Fatalf("expected 1 Neighbors call, got %d", store.neighborsCalls)
	}
	if store.lastNbrID != 1 || store.lastDir != app.RelationDirectionOutbound {
		t.Fatalf("dispatch mismatch: id=%d dir=%v", store.lastNbrID, store.lastDir)
	}

	text := result["content"].([]map[string]string)[0]["text"]
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("payload must be JSON array: %v (raw=%q)", err, text)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if int64(rows[0]["id"].(float64)) != 7 {
		t.Fatalf("row id mismatch: %+v", rows[0])
	}
	if rows[0]["relation_type"].(string) != "related" {
		t.Fatalf("relation_type missing or wrong: %+v", rows[0])
	}
	if _, ok := rows[0]["source_id"]; !ok {
		t.Fatalf("source_id field required in payload: %+v", rows[0])
	}
}

// TestMCP_GraphNeighborsCall_DirectionParsing covers task 3.4:
// "inbound" / "outbound" map to the corresponding enum; missing or
// unknown direction defaults to outbound (forward links are the most
// common navigation case so callers don't need to spell it every time).
func TestMCP_GraphNeighborsCall_DirectionParsing(t *testing.T) {
	withGraphFlag(t, "1")
	cases := []struct {
		raw    string
		wantD  app.RelationDirection
	}{
		{"outbound", app.RelationDirectionOutbound},
		{"inbound", app.RelationDirectionInbound},
		{"", app.RelationDirectionOutbound},
		{"unknown", app.RelationDirectionOutbound},
	}
	for _, tc := range cases {
		t.Run("dir="+tc.raw, func(t *testing.T) {
			store := newGraphMemStore()
			svc := app.NewService(store, nil)
			args := map[string]interface{}{"id": 1}
			if tc.raw != "" {
				args["direction"] = tc.raw
			}
			_, _ = callTool(t, svc, "graph_neighbors", args)
			if store.lastDir != tc.wantD {
				t.Fatalf("direction %q: got %v want %v", tc.raw, store.lastDir, tc.wantD)
			}
		})
	}
}

// TestMCP_GraphNeighborsCall_FlagOffRejects covers task 3.4 default-off
// posture for the read path too — symmetry with relate.
func TestMCP_GraphNeighborsCall_FlagOffRejects(t *testing.T) {
	withGraphFlag(t, "")
	store := newGraphMemStore()
	svc := app.NewService(store, nil)
	result, _ := callTool(t, svc, "graph_neighbors", map[string]interface{}{"id": 1})
	if result != nil {
		isErr, _ := result["isError"].(bool)
		if !isErr {
			t.Fatalf("flag-off graph_neighbors must error, got %+v", result)
		}
	}
	if store.neighborsCalls != 0 {
		t.Fatalf("store.Neighbors must NOT be reached when flag is off")
	}
}
