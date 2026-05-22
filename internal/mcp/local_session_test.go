package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// sessionMemStore extends the existing memStore (defined in server_test.go)
// with SessionStore so the MCP handler can exercise the local_session_*
// surface without booting SQLite. Mirrors metaMemStore (M1) and
// filterMemStore (M2).
type sessionMemStore struct {
	*memStore

	startCalls   int
	endCalls     int
	summaryCalls int

	lastID      string
	lastSummary string
	events      map[string][]app.SessionEvent
}

func newSessionMemStoreMCP() *sessionMemStore {
	return &sessionMemStore{memStore: newMemStore(), events: map[string][]app.SessionEvent{}}
}

func (s *sessionMemStore) SessionStart(id string, at time.Time) error {
	s.startCalls++
	s.lastID = id
	s.events[id] = append(s.events[id], app.SessionEvent{SessionID: id, Kind: "start", CreatedAt: at.UTC()})
	return nil
}

func (s *sessionMemStore) SessionEnd(id string, at time.Time) error {
	s.endCalls++
	s.lastID = id
	s.events[id] = append(s.events[id], app.SessionEvent{SessionID: id, Kind: "end", CreatedAt: at.UTC()})
	return nil
}

func (s *sessionMemStore) SessionSummary(id, summary string, at time.Time) error {
	s.summaryCalls++
	s.lastID = id
	s.lastSummary = summary
	s.events[id] = append(s.events[id], app.SessionEvent{SessionID: id, Kind: "summary", Summary: summary, CreatedAt: at.UTC()})
	return nil
}

func (s *sessionMemStore) SessionEvents(id string) ([]app.SessionEvent, error) {
	s.lastID = id
	return s.events[id], nil
}

func (s *sessionMemStore) ActiveSessionID() (string, error) {
	if strings.TrimSpace(s.lastID) == "" {
		return "", nil
	}
	return s.lastID, nil
}

var _ app.Store = (*sessionMemStore)(nil)
var _ app.SessionStore = (*sessionMemStore)(nil)

// callTool is a small helper that builds a tools/call JSON-RPC envelope,
// runs it through handleRequest, and returns (resultMap, rpcError).
func callTool(t *testing.T, svc *app.Service, name string, args map[string]interface{}) (map[string]interface{}, *rpcError) {
	t.Helper()
	argsJSON, _ := json.Marshal(args)
	paramsJSON, _ := json.Marshal(map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(argsJSON),
	})
	req := request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  paramsJSON,
	}
	payload, _ := json.Marshal(req)
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response from %s", name)
	}
	m, _ := resp.Result.(map[string]interface{})
	return m, resp.Error
}

// TestMCP_LocalSessionStart_Dispatches covers the happy path: invoking
// local_session_start MUST forward the trimmed id to the store and
// return a non-error confirmation payload.
func TestMCP_LocalSessionStart_Dispatches(t *testing.T) {
	store := newSessionMemStoreMCP()
	svc := app.NewService(store, nil)

	result, rpcErr := callTool(t, svc, "local_session_start", map[string]interface{}{
		"session_id": "sess-1",
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.startCalls != 1 || store.lastID != "sess-1" {
		t.Fatalf("expected 1 start call for sess-1, got calls=%d id=%q", store.startCalls, store.lastID)
	}
}

// TestMCP_LocalSessionEnd_Dispatches mirrors start under the strict
// close contract: end succeeds when a summary already exists.
func TestMCP_LocalSessionEnd_Dispatches(t *testing.T) {
	store := newSessionMemStoreMCP()
	svc := app.NewService(store, nil)

	_, _ = callTool(t, svc, "local_session_summary", map[string]interface{}{
		"session_id": "sess-2",
		"summary":    "done",
	})

	result, rpcErr := callTool(t, svc, "local_session_end", map[string]interface{}{
		"session_id": "sess-2",
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.endCalls != 1 || store.lastID != "sess-2" {
		t.Fatalf("expected 1 end call for sess-2, got calls=%d id=%q", store.endCalls, store.lastID)
	}
}

// TestMCP_LocalSessionSummary_ForwardsContent proves summary text reaches
// the store verbatim via SessionSummary.
func TestMCP_LocalSessionSummary_ForwardsContent(t *testing.T) {
	store := newSessionMemStoreMCP()
	svc := app.NewService(store, nil)

	result, rpcErr := callTool(t, svc, "local_session_summary", map[string]interface{}{
		"session_id": "sess-3",
		"summary":    "closed deal",
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.summaryCalls != 1 || store.lastSummary != "closed deal" {
		t.Fatalf("expected summary forwarded, got calls=%d summary=%q", store.summaryCalls, store.lastSummary)
	}
}

// TestMCP_LocalSession_ValidationErrors covers empty/missing args. The
// response MUST be a tool-level error (isError=true) — NOT an rpc error
// — so MCP hosts can surface the validation message to the user.
func TestMCP_LocalSession_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]interface{}
	}{
		{"start empty id", "local_session_start", map[string]interface{}{"session_id": ""}},
		{"end empty id", "local_session_end", map[string]interface{}{"session_id": "   "}},
		{"summary missing id", "local_session_summary", map[string]interface{}{"summary": "x"}},
		{"summary empty text", "local_session_summary", map[string]interface{}{"session_id": "s", "summary": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newSessionMemStoreMCP()
			svc := app.NewService(store, nil)
			result, rpcErr := callTool(t, svc, tc.tool, tc.args)
			if rpcErr != nil {
				t.Fatalf("expected tool error, got rpc error: %+v", rpcErr)
			}
			if result["isError"] != true {
				t.Fatalf("expected isError=true, got %+v", result)
			}
		})
	}
}

// TestMCP_LocalSession_AdvertisedSchemas proves tools/list exposes the
// three new tools with correct required fields and local-only +
// no-external-backend markers (TestToolDescriptions_MarkLocalOnly only
// enforces the legacy set; this test extends the contract to the new
// tools).
func TestMCP_LocalSession_AdvertisedSchemas(t *testing.T) {
	tools := advertisedTools(t)
	byName := map[string]map[string]interface{}{}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name != "" {
			byName[name] = tool
		}
	}

	cases := []struct {
		name     string
		required []string
	}{
		{"local_session_start", []string{"session_id"}},
		{"local_session_end", []string{"session_id"}},
		{"local_session_summary", []string{"session_id", "summary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := byName[tc.name]
			if !ok {
				t.Fatalf("tool %q not advertised: %v", tc.name, toolNames(tools))
			}
			schema, _ := tool["inputSchema"].(map[string]interface{})
			required := toStringSlice(schema["required"])
			if !sameStringSet(required, tc.required) {
				t.Fatalf("tool %q required mismatch: want %v got %v", tc.name, tc.required, required)
			}
			desc, _ := tool["description"].(string)
			lower := strings.ToLower(desc)
			if !strings.Contains(lower, "local-only") && !strings.Contains(lower, "local sqlite") {
				t.Fatalf("tool %q description must mark local-only, got %q", tc.name, desc)
			}
			if !strings.Contains(lower, "backend externo") &&
				!strings.Contains(lower, "external backend") {
				t.Fatalf("tool %q description must disambiguate from external backends, got %q", tc.name, desc)
			}
		})
	}
}
