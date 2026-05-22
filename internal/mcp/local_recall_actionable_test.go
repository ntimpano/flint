package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// withActionableFlag toggles NTCLI_FF_ACTIONABLE for the test and
// restores the prior value on cleanup, matching the pattern used by
// withGraphFlag in local_recall_graph_test.go.
func withActionableFlag(t *testing.T, value string) {
	t.Helper()
	prev, had := os.LookupEnv("NTCLI_FF_ACTIONABLE")
	if value == "" {
		_ = os.Unsetenv("NTCLI_FF_ACTIONABLE")
	} else {
		_ = os.Setenv("NTCLI_FF_ACTIONABLE", value)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NTCLI_FF_ACTIONABLE", prev)
		} else {
			_ = os.Unsetenv("NTCLI_FF_ACTIONABLE")
		}
	})
}

// TestLocalRecall_ActionableFFOff_PayloadIsLegacyArray proves that
// with NTCLI_FF_ACTIONABLE unset the local_recall payload remains a
// raw JSON array of items — byte-identical legacy contract for any
// client that has not opted in.
func TestLocalRecall_ActionableFFOff_PayloadIsLegacyArray(t *testing.T) {
	withActionableFlag(t, "")
	withGraphFlag(t, "")
	store := newFilterMemStore()
	svc := app.NewService(store, nil)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content:   "decision body",
		Title:     "Switch to JWT",
		Type:      "decision",
		CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "decision"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("FF off: payload must be a JSON array, got %q", trimmed)
	}
	if strings.Contains(trimmed, "next_action") {
		t.Fatalf("FF off: payload must NOT contain next_action, got %q", trimmed)
	}
}

// TestLocalRecall_ActionableFFOn_WrappedShape proves that when
// NTCLI_FF_ACTIONABLE=1 the tool returns the actionable shape with
// matches[], next_action, checklist[], and inferred_paths[] fields.
// Triangulates the FF-off test by asserting all four spec keys.
func TestLocalRecall_ActionableFFOn_WrappedShape(t *testing.T) {
	withActionableFlag(t, "1")
	withGraphFlag(t, "")
	store := newFilterMemStore()
	svc := app.NewService(store, nil)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content:   "switch payload **Where**: internal/auth/middleware.go\n- step one\n- step two",
		Title:     "Switch to JWT",
		Type:      "decision",
		CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "switch"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("FF on: payload must be a JSON object, got %q (%v)", text, err)
	}
	for _, key := range []string{"matches", "next_action", "checklist", "inferred_paths"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("FF on: missing key %q in payload %q", key, text)
		}
	}
	matches, ok := got["matches"].([]interface{})
	if !ok || len(matches) != 1 {
		t.Fatalf("FF on: expected 1 match, got %v", got["matches"])
	}
	if got["next_action"] == nil || got["next_action"].(string) == "" {
		t.Fatalf("FF on: expected non-empty next_action for top type=decision, got %v", got["next_action"])
	}
	if !strings.Contains(got["next_action"].(string), "Switch to JWT") {
		t.Fatalf("FF on: next_action must reference top title, got %q", got["next_action"])
	}
	checklist, ok := got["checklist"].([]interface{})
	if !ok || len(checklist) != 2 {
		t.Fatalf("FF on: expected 2 checklist items, got %v", got["checklist"])
	}
	paths, ok := got["inferred_paths"].([]interface{})
	if !ok || len(paths) != 1 || paths[0].(string) != "internal/auth/middleware.go" {
		t.Fatalf("FF on: expected inferred_paths=[internal/auth/middleware.go], got %v", got["inferred_paths"])
	}
}

// TestLocalRecall_ActionableFFOn_NoMatchesShape proves the response
// shape is stable when there are zero matches: matches MUST be empty
// array (not null), next_action MUST be empty string, checklist and
// inferred_paths MUST be empty arrays. This is the contract that
// callers rely on to treat the response uniformly.
func TestLocalRecall_ActionableFFOn_NoMatchesShape(t *testing.T) {
	withActionableFlag(t, "1")
	withGraphFlag(t, "")
	store := newFilterMemStore()
	svc := app.NewService(store, nil)

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "no-such-token"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not a JSON object: %q (%v)", text, err)
	}
	matches, ok := got["matches"].([]interface{})
	if !ok {
		t.Fatalf("matches must be a JSON array even when empty, got %v", got["matches"])
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
	if got["next_action"] != "" {
		t.Fatalf("expected empty next_action, got %v", got["next_action"])
	}
	checklist, ok := got["checklist"].([]interface{})
	if !ok || len(checklist) != 0 {
		t.Fatalf("checklist must be empty array, got %v", got["checklist"])
	}
	paths, ok := got["inferred_paths"].([]interface{})
	if !ok || len(paths) != 0 {
		t.Fatalf("inferred_paths must be empty array, got %v", got["inferred_paths"])
	}
}

// TestLocalRecall_ActionableFFOn_NextActionEmptyForNonActionableType
// triangulates the gating rule from spec: top type ∉ {decision,
// bugfix, pattern} → next_action must be the empty string even when
// matches are present.
func TestLocalRecall_ActionableFFOn_NextActionEmptyForNonActionableType(t *testing.T) {
	withActionableFlag(t, "1")
	withGraphFlag(t, "")
	store := newFilterMemStore()
	svc := app.NewService(store, nil)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content:   "just a manual note",
		Title:     "Manual",
		Type:      "manual",
		CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	payload := newCallReq(t, "local_recall", map[string]interface{}{"query": "manual"})
	resp, _ := handleRequest(payload, svc)
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON object: %v", err)
	}
	if got["next_action"] != "" {
		t.Fatalf("type=manual: expected empty next_action, got %v", got["next_action"])
	}
	matches, _ := got["matches"].([]interface{})
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}
