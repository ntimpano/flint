package mcp

import (
	"strings"
	"testing"

	"flint/internal/app"
)

// doctorMemStoreMCP wraps memStore with DoctorStore so MCP can dispatch
// local_doctor without booting SQLite.
type doctorMemStoreMCP struct {
	*memStore

	report app.DoctorReport
	calls  int
}

func newDoctorMemStoreMCP() *doctorMemStoreMCP {
	return &doctorMemStoreMCP{
		memStore: newMemStore(),
		report: app.DoctorReport{
			SchemaVersion:    3,
			FTSHealthy:       true,
			IntegrityOK:      true,
			MemoryItemsCount: 4,
			SessionsCount:    1,
			Summary:          "schema_version=3  fts=healthy  integrity=ok  memory_items=4  sessions=1",
		},
	}
}

func (d *doctorMemStoreMCP) Doctor() (app.DoctorReport, error) {
	d.calls++
	return d.report, nil
}

var _ app.Store = (*doctorMemStoreMCP)(nil)
var _ app.DoctorStore = (*doctorMemStoreMCP)(nil)

// TestMCP_LocalDoctor_DispatchHealthy proves the MCP doctor scenario:
// `local_doctor` runs against the live store and returns the human
// summary verbatim, so MCP clients see every diagnostic axis.
func TestMCP_LocalDoctor_DispatchHealthy(t *testing.T) {
	store := newDoctorMemStoreMCP()
	svc := app.NewService(store, nil)
	result, rpcErr := callTool(t, svc, "local_doctor", map[string]interface{}{})
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if store.calls != 1 {
		t.Fatalf("expected exactly 1 Doctor call, got %d", store.calls)
	}
	text := toolTextOf(result)
	if !strings.Contains(text, store.report.Summary) {
		t.Fatalf("expected summary in tool text, got %q", text)
	}
}

// TestMCP_LocalDoctor_AdvertisedSchema confirms tools/list advertises
// local_doctor with zero required args + Spanish description containing
// the mandatory "local-only" + "backend externo" markers per parity
// convention.
func TestMCP_LocalDoctor_AdvertisedSchema(t *testing.T) {
	tools := advertisedTools(t)
	var doctor map[string]interface{}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name == "local_doctor" {
			doctor = tool
			break
		}
	}
	if doctor == nil {
		t.Fatalf("local_doctor not advertised; got %v", toolNames(tools))
	}
	desc, _ := doctor["description"].(string)
	low := strings.ToLower(desc)
	for _, must := range []string{"local-only", "backend externo"} {
		if !strings.Contains(low, must) {
			t.Fatalf("description missing %q: %q", must, desc)
		}
	}
	schema, _ := doctor["inputSchema"].(map[string]interface{})
	if required, ok := schema["required"]; ok {
		if list, ok := required.([]string); ok && len(list) > 0 {
			t.Fatalf("local_doctor must declare zero required args, got %v", list)
		}
	}
}

// toolTextOf extracts the first content text item from a tool result —
// helper kept local since other MCP tests inline this pattern.
func toolTextOf(result map[string]interface{}) string {
	content, _ := result["content"].([]map[string]string)
	if len(content) == 0 {
		return ""
	}
	return content[0]["text"]
}
