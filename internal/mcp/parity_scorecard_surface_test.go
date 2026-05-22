package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"flint/internal/app"
	"flint/internal/model"
)

// callParityScorecardMCP invokes the MCP `parity_scorecard` tool with the
// given signal arguments and returns the parsed text payload + isError flag.
// Centralising the JSON-RPC plumbing keeps individual tests focused on the
// scorecard contract instead of transport boilerplate.
func callParityScorecardMCP(t *testing.T, args map[string]interface{}) (string, bool) {
	t.Helper()
	svc := app.NewService(newMemStore(), nil)
	rawArgs, _ := json.Marshal(args)
	params, _ := json.Marshal(toolsCallParams{Name: "parity_scorecard", Arguments: rawArgs})
	payload, _ := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("mcp returned no response")
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("mcp result not a map: %T", resp.Result)
	}
	isError, _ := result["isError"].(bool)
	// content is []map[string]interface{} (Go slice from toolText), not
	// []interface{} from JSON; handle both shapes for safety.
	var text string
	switch c := result["content"].(type) {
	case []interface{}:
		if len(c) == 0 {
			t.Fatalf("mcp result missing content: %v", result)
		}
		first, _ := c[0].(map[string]interface{})
		text, _ = first["text"].(string)
	case []map[string]interface{}:
		if len(c) == 0 {
			t.Fatalf("mcp result missing content: %v", result)
		}
		text, _ = c[0]["text"].(string)
	case []map[string]string:
		if len(c) == 0 {
			t.Fatalf("mcp result missing content: %v", result)
		}
		text = c[0]["text"]
	default:
		t.Fatalf("mcp content has unexpected type %T: %v", result["content"], result)
	}
	return text, isError
}

// TestParityScorecard_MCPSurface_ReturnsContract proves task 1.4: the MCP
// tool `parity_scorecard` MUST return JSON matching {total, dimensions[],
// version, verdict, hold_reason} so external clients can pin the contract.
func TestParityScorecard_MCPSurface_ReturnsContract(t *testing.T) {
	text, isError := callParityScorecardMCP(t, map[string]interface{}{
		"core_ops":                100,
		"metadata_retrieval":      100,
		"session_workflow":        100,
		"import_export_backup":    100,
		"reliability_operability": 100,
		"knowledge_continuity":    100,
		"ux_api_contract":         100,
		"soak_days":               30,
	})
	if isError {
		t.Fatalf("parity_scorecard with valid signals must not return isError, got text=%q", text)
	}

	var got model.ScorecardVerdict
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("MCP payload must be JSON-decodable into model.ScorecardVerdict: %v\npayload=%q", err, text)
	}
	if got.Total != 100.0 {
		t.Fatalf("expected total=100.0, got %.2f", got.Total)
	}
	if got.Verdict != model.VerdictPass {
		t.Fatalf("expected verdict=pass, got %q", got.Verdict)
	}
	if len(got.Dimensions) != 7 {
		t.Fatalf("expected 7 dimensions, got %d", len(got.Dimensions))
	}
	if got.Version == "" {
		t.Fatalf("contract must include a non-empty version")
	}
}

// TestParityScorecard_MCPSurface_HoldOnSoak triangulates the MCP surface:
// soak<14 with otherwise-green signals MUST yield verdict=hold and
// hold_reason="soak_window" through the live JSON-RPC path.
func TestParityScorecard_MCPSurface_HoldOnSoak(t *testing.T) {
	text, isError := callParityScorecardMCP(t, map[string]interface{}{
		"core_ops":                100,
		"metadata_retrieval":      100,
		"session_workflow":        100,
		"import_export_backup":    100,
		"reliability_operability": 100,
		"knowledge_continuity":    100,
		"ux_api_contract":         100,
		"soak_days":               5,
	})
	if isError {
		t.Fatalf("parity_scorecard hold path must not be reported as MCP error: %q", text)
	}
	var got model.ScorecardVerdict
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.Verdict != model.VerdictHold {
		t.Fatalf("soak<14 must produce verdict=hold, got %q", got.Verdict)
	}
	if got.HoldReason != "soak_window" {
		t.Fatalf("hold_reason MUST be 'soak_window', got %q", got.HoldReason)
	}
}

// TestParityScorecard_MCPSurface_AdvertisesTool proves the new tool is
// listed by tools/list so MCP hosts discover it without manual config.
func TestParityScorecard_MCPSurface_AdvertisesTool(t *testing.T) {
	names := advertisedToolNames(t)
	for _, n := range names {
		if n == "parity_scorecard" {
			return
		}
	}
	t.Fatalf("tools/list MUST advertise parity_scorecard; got %v", names)
}

// TestParityScorecard_CLISurface_ReturnsJSON proves the CLI side of task
// 1.4: `nt-cli parity scorecard` MUST emit the same JSON contract on
// stdout so the CLI is parity with the MCP tool. Signals come from
// command-line flags so the runbook can replay deterministic verdicts.
func TestParityScorecard_CLISurface_ReturnsJSON(t *testing.T) {
	svc := app.NewService(newMemStore(), nil)
	var stdout, stderr bytes.Buffer
	args := []string{
		"parity", "scorecard",
		"--core-ops=100",
		"--metadata-retrieval=100",
		"--session-workflow=100",
		"--import-export-backup=100",
		"--reliability-operability=100",
		"--knowledge-continuity=100",
		"--ux-api-contract=100",
		"--soak-days=20",
	}
	code := app.RunCLI(svc, args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("parity scorecard with green signals must exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	var got model.ScorecardVerdict
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("CLI stdout must be JSON-decodable into model.ScorecardVerdict: %v\nstdout=%q", err, stdout.String())
	}
	if got.Verdict != model.VerdictPass {
		t.Fatalf("expected verdict=pass, got %q (total=%.2f)", got.Verdict, got.Total)
	}
}

// TestParityScorecard_CLISurface_RejectsUnknownSubcommand proves the CLI
// surfaces typos instead of silently no-oping. `nt-cli parity` without a
// subcommand or with an unknown one MUST exit non-zero with usage.
func TestParityScorecard_CLISurface_RejectsUnknownSubcommand(t *testing.T) {
	svc := app.NewService(newMemStore(), nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, []string{"parity", "bogus"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("unknown parity subcommand must exit non-zero, got 0")
	}
	if !strings.Contains(stderr.String(), "parity") {
		t.Fatalf("error must mention 'parity' usage, got stderr=%q", stderr.String())
	}
}
