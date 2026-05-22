package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flint/internal/app"
)

// importMemStoreMCP wraps memStore with ImportStore so MCP can dispatch
// local_import without booting SQLite. Mirrors sessionMemStore (M3.A).
type importMemStoreMCP struct {
	*memStore

	calls  int
	lastIn []app.ImportRecord
	result app.ImportResult
}

func newImportMemStoreMCP() *importMemStoreMCP {
	return &importMemStoreMCP{memStore: newMemStore()}
}

func (m *importMemStoreMCP) ImportRecords(rows []app.ImportRecord) (app.ImportResult, error) {
	m.calls++
	m.lastIn = rows
	if m.result == (app.ImportResult{}) {
		m.result = app.ImportResult{Inserted: len(rows)}
	}
	return m.result, nil
}

var _ app.Store = (*importMemStoreMCP)(nil)
var _ app.ImportStore = (*importMemStoreMCP)(nil)

func writeImportFixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// TestMCP_LocalImport_Dispatches: happy path — file path is read, store
// receives parsed records, response is success.
func TestMCP_LocalImport_Dispatches(t *testing.T) {
	store := newImportMemStoreMCP()
	svc := app.NewService(store, nil)
	path := writeImportFixture(t, "in.json",
		`[{"content":"a","topic_key":"k1"},{"content":"b","topic_key":"k2"}]`)

	result, rpcErr := callTool(t, svc, "local_import", map[string]interface{}{
		"path": path,
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.calls != 1 || len(store.lastIn) != 2 {
		t.Fatalf("expected 1 call/2 rows, got calls=%d rows=%d", store.calls, len(store.lastIn))
	}
}

// TestMCP_LocalImport_DryRun: dry_run=true MUST NOT touch the store and
// MUST surface the planned counts in the response text.
func TestMCP_LocalImport_DryRun(t *testing.T) {
	store := newImportMemStoreMCP()
	svc := app.NewService(store, nil)
	path := writeImportFixture(t, "in.json", `[{"content":"x"}]`)

	result, rpcErr := callTool(t, svc, "local_import", map[string]interface{}{
		"path":    path,
		"dry_run": true,
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.calls != 0 {
		t.Fatalf("dry-run must not touch store, got %d calls", store.calls)
	}
}

// TestMCP_LocalImport_ValidationErrors: bad/missing path returns a
// tool-level error (isError=true), NOT a JSON-RPC error.
func TestMCP_LocalImport_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"missing path", map[string]interface{}{}},
		{"empty path", map[string]interface{}{"path": "   "}},
		{"nonexistent file", map[string]interface{}{"path": "/nope/missing.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newImportMemStoreMCP()
			svc := app.NewService(store, nil)
			result, rpcErr := callTool(t, svc, "local_import", tc.args)
			if rpcErr != nil {
				t.Fatalf("expected tool error, got rpc error: %+v", rpcErr)
			}
			if result["isError"] != true {
				t.Fatalf("expected isError=true, got %+v", result)
			}
			if store.calls != 0 {
				t.Fatalf("store must not be called on validation error")
			}
		})
	}
}

// TestMCP_LocalImport_AdvertisedSchema: tools/list MUST expose
// local_import with `path` required and Spanish local-only + no-external-
// backend markers.
func TestMCP_LocalImport_AdvertisedSchema(t *testing.T) {
	tools := advertisedTools(t)
	var found map[string]interface{}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name == "local_import" {
			found = tool
			break
		}
	}
	if found == nil {
		t.Fatalf("local_import not advertised: %v", toolNames(tools))
	}
	schema, _ := found["inputSchema"].(map[string]interface{})
	required := toStringSlice(schema["required"])
	if !sameStringSet(required, []string{"path"}) {
		t.Fatalf("required mismatch: want [path] got %v", required)
	}
	desc, _ := found["description"].(string)
	lower := strings.ToLower(desc)
	if !strings.Contains(lower, "local-only") && !strings.Contains(lower, "local sqlite") {
		t.Fatalf("description must mark local-only, got %q", desc)
	}
	if !strings.Contains(lower, "backend externo") &&
		!strings.Contains(lower, "external backend") {
		t.Fatalf("description must disambiguate from external backends, got %q", desc)
	}
}
