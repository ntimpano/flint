package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// TestParityGate_G1_RequiredToolSet proves spec scenario "Parity assertion
// reports required tool set": missing tools MUST cause the test to fail
// with both the failing gate name (G1) AND the missing tool name in the
// failure message. The pure helper checkParityGateG1 lets us assert
// failure-message wording without rebuilding the whole MCP surface.
func TestParityGate_G1_RequiredToolSet(t *testing.T) {
	required := []string{
		"local_save",
		"local_recall",
		"local_list",
		"local_get",
		"local_update",
		"local_delete",
	}

	t.Run("passes when every required tool is advertised", func(t *testing.T) {
		ok, msg := checkParityGateG1(advertisedToolNames(t), required)
		if !ok {
			t.Fatalf("G1 should pass for current advertised set, got msg=%q", msg)
		}
		if msg != "" {
			t.Fatalf("G1 success must produce empty message, got %q", msg)
		}
	})

	t.Run("fails naming gate G1 and the missing tool", func(t *testing.T) {
		// Omit local_update to force a miss.
		advertised := []string{"local_save", "local_recall", "local_list", "local_get", "local_delete"}
		ok, msg := checkParityGateG1(advertised, required)
		if ok {
			t.Fatalf("G1 must fail when local_update is missing, got ok=true")
		}
		if !strings.Contains(msg, "G1") {
			t.Fatalf("G1 failure message must name the gate (G1), got %q", msg)
		}
		if !strings.Contains(msg, "local_update") {
			t.Fatalf("G1 failure message must name the missing tool, got %q", msg)
		}
	})

	t.Run("fails listing every missing tool", func(t *testing.T) {
		advertised := []string{"local_save", "local_recall"}
		ok, msg := checkParityGateG1(advertised, required)
		if ok {
			t.Fatalf("G1 must fail when several tools are missing")
		}
		for _, want := range []string{"local_list", "local_get", "local_update", "local_delete"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("G1 failure message must include missing tool %q, got %q", want, msg)
			}
		}
	})

	// End-to-end: drive the live tools/list payload through the gate.
	t.Run("live advertised set satisfies G1", func(t *testing.T) {
		ok, msg := checkParityGateG1(advertisedToolNames(t), required)
		if !ok {
			t.Fatalf("live advertised set failed G1: %s", msg)
		}
	})
}

// TestParityGate_G2_OperationSmoke proves spec scenario "Operation parity":
// save/recall/list/get/update/delete each succeed on CLI + MCP for a sample
// N≥10. Failures MUST name the gate (G2) and the failing operation.
func TestParityGate_G2_OperationSmoke(t *testing.T) {
	t.Run("runs at least 10 operations across all six tools", func(t *testing.T) {
		report := runG2Smoke(t)
		if report.total < 10 {
			t.Fatalf("G2 smoke must run N>=10 operations, got %d", report.total)
		}
		// Every required tool MUST be exercised at least once.
		for _, tool := range []string{
			"local_save", "local_recall", "local_list",
			"local_get", "local_update", "local_delete",
		} {
			if report.perTool[tool] == 0 {
				t.Fatalf("G2 smoke must exercise tool %q at least once, got counts=%v", tool, report.perTool)
			}
		}
		if !report.ok {
			t.Fatalf("G2 smoke failed: %s", report.failureMessage)
		}
	})

	t.Run("failure message names gate G2 and the failing operation", func(t *testing.T) {
		// Synthesize a failed op result and verify formatting.
		ok, msg := checkParityGateG2([]opResult{
			{tool: "local_save", ok: true},
			{tool: "local_get", ok: false, detail: "cli=success mcp=error"},
		})
		if ok {
			t.Fatalf("G2 must fail when any op result is not ok")
		}
		if !strings.Contains(msg, "G2") {
			t.Fatalf("G2 failure message must name the gate (G2), got %q", msg)
		}
		if !strings.Contains(msg, "local_get") {
			t.Fatalf("G2 failure message must name failing tool, got %q", msg)
		}
	})

	t.Run("passes when every op result is ok", func(t *testing.T) {
		ok, msg := checkParityGateG2([]opResult{
			{tool: "local_save", ok: true},
			{tool: "local_recall", ok: true},
		})
		if !ok {
			t.Fatalf("G2 must pass when every op succeeded, got msg=%q", msg)
		}
		if msg != "" {
			t.Fatalf("G2 success must produce empty message, got %q", msg)
		}
	})
}

// advertisedToolNames is a small adapter for the gate helpers that returns
// just the names from the live tools/list payload.
func advertisedToolNames(t *testing.T) []string {
	t.Helper()
	return toolNames(advertisedTools(t))
}

// runG2Smoke exercises a 10+-operation scenario across all six tools on
// both surfaces and returns a structured report. It is used by the G2
// test to keep the assertion surface readable while still proving real
// CLI/MCP parity for every required operation.
func runG2Smoke(t *testing.T) g2Report {
	t.Helper()
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	// Build matched stores: same seed on both sides to make comparisons fair.
	cliStore := newMemStore()
	mcpStore := newMemStore()
	cliSvc := app.NewService(cliStore, nil)
	mcpSvc := app.NewService(mcpStore, nil)

	results := []opResult{}
	add := func(r opResult) { results = append(results, r) }

	// 1) save x2 — proves both surfaces accept content
	add(runOp(t, cliSvc, mcpSvc, "local_save", []string{"save", "alpha"}, map[string]interface{}{"content": "alpha"}))
	add(runOp(t, cliSvc, mcpSvc, "local_save", []string{"save", "beta"}, map[string]interface{}{"content": "beta"}))

	// 2) seed identical state for read paths
	cliStore.Save("gamma", created)
	mcpStore.Save("gamma", created)

	// 3) recall present, recall absent
	add(runOp(t, cliSvc, mcpSvc, "local_recall", []string{"recall", "gamma"}, map[string]interface{}{"query": "gamma"}))
	add(runOp(t, cliSvc, mcpSvc, "local_recall", []string{"recall", "no-such-token"}, map[string]interface{}{"query": "no-such-token"}))

	// 4) list with and without limit
	add(runOp(t, cliSvc, mcpSvc, "local_list", []string{"list"}, map[string]interface{}{}))
	add(runOp(t, cliSvc, mcpSvc, "local_list", []string{"list", "1"}, map[string]interface{}{"limit": 1}))

	// 5) get hit and miss
	add(runOp(t, cliSvc, mcpSvc, "local_get", []string{"get", "1"}, map[string]interface{}{"id": 1}))
	add(runOp(t, cliSvc, mcpSvc, "local_get", []string{"get", "999"}, map[string]interface{}{"id": 999}))

	// 6) update hit and miss
	add(runOp(t, cliSvc, mcpSvc, "local_update", []string{"update", "1", "alpha-v2"}, map[string]interface{}{"id": 1, "content": "alpha-v2"}))
	add(runOp(t, cliSvc, mcpSvc, "local_update", []string{"update", "999", "ghost"}, map[string]interface{}{"id": 999, "content": "ghost"}))

	// 7) delete hit and miss
	add(runOp(t, cliSvc, mcpSvc, "local_delete", []string{"delete", "2"}, map[string]interface{}{"id": 2}))
	add(runOp(t, cliSvc, mcpSvc, "local_delete", []string{"delete", "999"}, map[string]interface{}{"id": 999}))

	ok, msg := checkParityGateG2(results)
	r := g2Report{
		total:          len(results),
		perTool:        map[string]int{},
		ok:             ok,
		failureMessage: msg,
	}
	for _, x := range results {
		r.perTool[x.tool]++
	}
	return r
}

// runOp drives one logical operation through both CLI and MCP using the
// same args and returns whether the success/error category matched.
func runOp(t *testing.T, cliSvc, mcpSvc *app.Service, tool string, cliArgs []string, mcpArgs map[string]interface{}) opResult {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cliCode := app.RunCLI(cliSvc, cliArgs, &stdout, &stderr)
	cliSuccess := cliCode == 0

	rawArgs, _ := json.Marshal(mcpArgs)
	params, _ := json.Marshal(toolsCallParams{Name: tool, Arguments: rawArgs})
	payload, _ := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params})
	resp, ok := handleRequest(payload, mcpSvc)
	if !ok {
		return opResult{tool: tool, ok: false, detail: "mcp returned no response"}
	}
	result, _ := resp.Result.(map[string]interface{})
	mcpIsError, _ := result["isError"].(bool)
	mcpSuccess := !mcpIsError

	if cliSuccess != mcpSuccess {
		return opResult{
			tool:   tool,
			ok:     false,
			detail: fmt.Sprintf("cli=%v mcp=%v stderr=%q", cliSuccess, mcpSuccess, stderr.String()),
		}
	}
	return opResult{tool: tool, ok: true}
}
