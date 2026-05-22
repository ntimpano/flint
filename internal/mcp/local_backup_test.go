package mcp

import (
	"strings"
	"testing"

	"flint/internal/app"
)

// backupMemStoreMCP wraps memStore with BackupStore so MCP can dispatch
// local_backup / local_restore without booting SQLite.
type backupMemStoreMCP struct {
	*memStore

	backupCalls  int
	restoreCalls int
	lastBackup   string
	lastRestore  string
}

func newBackupMemStoreMCP() *backupMemStoreMCP {
	return &backupMemStoreMCP{memStore: newMemStore()}
}

func (b *backupMemStoreMCP) Backup(dst string) error {
	b.backupCalls++
	b.lastBackup = dst
	return nil
}

func (b *backupMemStoreMCP) Restore(src string) error {
	b.restoreCalls++
	b.lastRestore = src
	return nil
}

var _ app.Store = (*backupMemStoreMCP)(nil)
var _ app.BackupStore = (*backupMemStoreMCP)(nil)

func TestMCP_LocalBackup_Dispatches(t *testing.T) {
	store := newBackupMemStoreMCP()
	svc := app.NewService(store, nil)
	result, rpcErr := callTool(t, svc, "local_backup", map[string]interface{}{
		"path": "/tmp/snap.db",
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.backupCalls != 1 || store.lastBackup != "/tmp/snap.db" {
		t.Fatalf("expected backup call, got calls=%d path=%q",
			store.backupCalls, store.lastBackup)
	}
}

func TestMCP_LocalRestore_Dispatches(t *testing.T) {
	store := newBackupMemStoreMCP()
	svc := app.NewService(store, nil)
	result, rpcErr := callTool(t, svc, "local_restore", map[string]interface{}{
		"path": "/tmp/in.db",
	})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.restoreCalls != 1 || store.lastRestore != "/tmp/in.db" {
		t.Fatalf("expected restore call, got calls=%d path=%q",
			store.restoreCalls, store.lastRestore)
	}
}

func TestMCP_LocalBackupRestore_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]interface{}
	}{
		{"backup empty path", "local_backup", map[string]interface{}{"path": "  "}},
		{"backup missing path", "local_backup", map[string]interface{}{}},
		{"restore empty path", "local_restore", map[string]interface{}{"path": ""}},
		{"restore missing path", "local_restore", map[string]interface{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newBackupMemStoreMCP()
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

func TestMCP_LocalBackupRestore_AdvertisedSchemas(t *testing.T) {
	tools := advertisedTools(t)
	byName := map[string]map[string]interface{}{}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name != "" {
			byName[name] = tool
		}
	}
	for _, name := range []string{"local_backup", "local_restore"} {
		t.Run(name, func(t *testing.T) {
			tool, ok := byName[name]
			if !ok {
				t.Fatalf("tool %q not advertised: %v", name, toolNames(tools))
			}
			schema, _ := tool["inputSchema"].(map[string]interface{})
			required := toStringSlice(schema["required"])
			if !sameStringSet(required, []string{"path"}) {
				t.Fatalf("required mismatch: want [path] got %v", required)
			}
			desc, _ := tool["description"].(string)
			lower := strings.ToLower(desc)
			if !strings.Contains(lower, "local-only") && !strings.Contains(lower, "local sqlite") {
				t.Fatalf("description must mark local-only, got %q", desc)
			}
			if !strings.Contains(lower, "backend externo") &&
				!strings.Contains(lower, "external backend") {
				t.Fatalf("description must disambiguate from external backends, got %q", desc)
			}
		})
	}
}
