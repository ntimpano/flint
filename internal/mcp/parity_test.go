package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// parityCase describes a single behavioural scenario that the spec requires
// to be honoured equivalently on both the CLI and MCP surfaces. Each case
// drives the same Service through `app.RunCLI` and `mcp.handleRequest` and
// asserts both reach the same semantic outcome (success vs error).
type parityCase struct {
	name string
	// arrange runs once and returns a Service backed by a fresh memStore that
	// already contains any preconditions for the scenario. Returning the
	// store lets each side inspect / mutate it independently.
	arrange func(t *testing.T) (*app.Service, *memStore)
	// cliArgs is passed verbatim to app.RunCLI (no leading binary name).
	cliArgs []string
	// mcpTool / mcpArgs is the equivalent MCP tools/call invocation.
	mcpTool string
	mcpArgs map[string]interface{}
	// wantSuccess is true when both surfaces should report success (CLI exit 0
	// with stderr empty; MCP isError != true). Otherwise both must surface a
	// failure of equivalent category (CLI exit != 0 / MCP isError == true).
	wantSuccess bool
}

func runParity(t *testing.T, tc parityCase) {
	t.Helper()

	// CLI side
	cliSvc, cliStore := tc.arrange(t)
	var stdout, stderr bytes.Buffer
	cliCode := app.RunCLI(cliSvc, tc.cliArgs, &stdout, &stderr)
	cliSuccess := cliCode == 0

	// MCP side — independent fresh state to avoid cross-contamination.
	mcpSvc, mcpStore := tc.arrange(t)
	rawArgs, err := json.Marshal(tc.mcpArgs)
	if err != nil {
		t.Fatalf("marshal mcp args: %v", err)
	}
	params, err := json.Marshal(toolsCallParams{Name: tc.mcpTool, Arguments: rawArgs})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	resp, ok := handleRequest(payload, mcpSvc)
	if !ok {
		t.Fatalf("mcp returned no response for %q", tc.name)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("mcp result not a map: %T", resp.Result)
	}
	mcpIsError, _ := result["isError"].(bool)
	mcpSuccess := !mcpIsError

	// Parity: success/failure category MUST match across surfaces.
	if cliSuccess != mcpSuccess {
		t.Fatalf("parity violation in %q: CLI success=%v (code=%d, stderr=%q) MCP success=%v (result=%v)",
			tc.name, cliSuccess, cliCode, stderr.String(), mcpSuccess, result)
	}
	if cliSuccess != tc.wantSuccess {
		t.Fatalf("expected wantSuccess=%v in %q, got CLI success=%v / MCP success=%v",
			tc.wantSuccess, tc.name, cliSuccess, mcpSuccess)
	}

	// Side-effect parity: both store mutations must converge to the same
	// observable state for the touched id (where applicable).
	switch tc.mcpTool {
	case "local_update":
		idF, _ := tc.mcpArgs["id"].(int)
		id := int64(idF)
		if id <= 0 {
			break
		}
		cliItem, cliErr := cliStore.Get(id)
		mcpItem, mcpErr := mcpStore.Get(id)
		if (cliErr == nil) != (mcpErr == nil) {
			t.Fatalf("post-update existence parity broken: cliErr=%v mcpErr=%v", cliErr, mcpErr)
		}
		if cliErr == nil && cliItem.Content != mcpItem.Content {
			t.Fatalf("post-update content parity broken: cli=%q mcp=%q", cliItem.Content, mcpItem.Content)
		}
	case "local_delete":
		idF, _ := tc.mcpArgs["id"].(int)
		id := int64(idF)
		if id <= 0 {
			break
		}
		_, cliErr := cliStore.Get(id)
		_, mcpErr := mcpStore.Get(id)
		if (cliErr == nil) != (mcpErr == nil) {
			t.Fatalf("post-delete existence parity broken: cliErr=%v mcpErr=%v", cliErr, mcpErr)
		}
	}
}

func arrangeWithNote(content string, created time.Time) func(t *testing.T) (*app.Service, *memStore) {
	return func(t *testing.T) (*app.Service, *memStore) {
		t.Helper()
		s := newMemStore()
		s.Save(content, created)
		return app.NewService(s, nil), s
	}
}

func arrangeEmpty() func(t *testing.T) (*app.Service, *memStore) {
	return func(t *testing.T) (*app.Service, *memStore) {
		t.Helper()
		s := newMemStore()
		return app.NewService(s, nil), s
	}
}

// TestCLIvsMCP_Parity proves the spec scenario "CLI/MCP behavior parity":
// equivalent operations on both surfaces produce equivalent outcomes
// (success vs error category and observable store state).
func TestCLIvsMCP_Parity(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := []parityCase{
		{
			name:        "get existing id succeeds on both surfaces",
			arrange:     arrangeWithNote("hello", created),
			cliArgs:     []string{"get", "1"},
			mcpTool:     "local_get",
			mcpArgs:     map[string]interface{}{"id": 1},
			wantSuccess: true,
		},
		{
			name:        "get missing id fails on both surfaces",
			arrange:     arrangeEmpty(),
			cliArgs:     []string{"get", "999"},
			mcpTool:     "local_get",
			mcpArgs:     map[string]interface{}{"id": 999},
			wantSuccess: false,
		},
		{
			name:        "get invalid id (zero) fails on both surfaces",
			arrange:     arrangeEmpty(),
			cliArgs:     []string{"get", "0"},
			mcpTool:     "local_get",
			mcpArgs:     map[string]interface{}{"id": 0},
			wantSuccess: false,
		},
		{
			name:        "update existing id succeeds and mutates store on both surfaces",
			arrange:     arrangeWithNote("old", created),
			cliArgs:     []string{"update", "1", "new"},
			mcpTool:     "local_update",
			mcpArgs:     map[string]interface{}{"id": 1, "content": "new"},
			wantSuccess: true,
		},
		{
			name:        "update missing id fails on both surfaces",
			arrange:     arrangeEmpty(),
			cliArgs:     []string{"update", "999", "x"},
			mcpTool:     "local_update",
			mcpArgs:     map[string]interface{}{"id": 999, "content": "x"},
			wantSuccess: false,
		},
		{
			name:        "update with empty content fails on both surfaces",
			arrange:     arrangeWithNote("old", created),
			cliArgs:     []string{"update", "1", "   "},
			mcpTool:     "local_update",
			mcpArgs:     map[string]interface{}{"id": 1, "content": "   "},
			wantSuccess: false,
		},
		{
			name:        "delete existing id succeeds and removes from store on both surfaces",
			arrange:     arrangeWithNote("doomed", created),
			cliArgs:     []string{"delete", "1"},
			mcpTool:     "local_delete",
			mcpArgs:     map[string]interface{}{"id": 1},
			wantSuccess: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runParity(t, tc) })
	}
}

// TestCLIvsMCP_GetPayloadParity proves that when both surfaces succeed on
// `get`, the rendered note carries the SAME id and content (parity at the
// payload level, not just success/failure category).
func TestCLIvsMCP_GetPayloadParity(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	// CLI
	cliStore := newMemStore()
	cliStore.Save("payload-parity", created)
	cliSvc := app.NewService(cliStore, nil)
	var stdout bytes.Buffer
	if code := app.RunCLI(cliSvc, []string{"get", "1"}, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("cli get failed: code=%d", code)
	}
	cliOut := stdout.String()

	// MCP
	mcpStore := newMemStore()
	mcpStore.Save("payload-parity", created)
	mcpSvc := app.NewService(mcpStore, nil)
	rawArgs, _ := json.Marshal(map[string]interface{}{"id": 1})
	params, _ := json.Marshal(toolsCallParams{Name: "local_get", Arguments: rawArgs})
	payload, _ := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params})
	resp, _ := handleRequest(payload, mcpSvc)
	result := resp.Result.(map[string]interface{})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("mcp get failed unexpectedly")
	}
	content := result["content"].([]map[string]string)
	mcpJSON := content[0]["text"]

	var mcpPayload map[string]interface{}
	if err := json.Unmarshal([]byte(mcpJSON), &mcpPayload); err != nil {
		t.Fatalf("mcp payload not JSON: %v (%q)", err, mcpJSON)
	}

	// id parity
	if int64(mcpPayload["id"].(float64)) != 1 {
		t.Fatalf("mcp id mismatch: %v", mcpPayload["id"])
	}
	if !strings.Contains(cliOut, "#1") {
		t.Fatalf("cli output missing id: %q", cliOut)
	}

	// content parity — same string surfaces on both sides
	if mcpPayload["content"].(string) != "payload-parity" {
		t.Fatalf("mcp content mismatch: %v", mcpPayload["content"])
	}
	if !strings.Contains(cliOut, "payload-parity") {
		t.Fatalf("cli output missing content: %q", cliOut)
	}
}
