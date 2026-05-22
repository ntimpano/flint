package app_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
	"flint/internal/model"
)

// seedContinuityStore populates a memStore with content matching the
// fixture markers in testdata/parity/queries.json so the harness can
// exercise the real Recall path without depending on the on-disk
// SQLite store. Returns the path to a tmp baseline output for the test.
func seedContinuityStore(t *testing.T) (*memStore, string) {
	t.Helper()
	store := newMemStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	contents := []string{
		"JWT auth middleware: switched from sessions to JWT for stateless auth",
		"FTS5 sanitize: wrapped query terms in quotes to escape MATCH operators",
		"Parity scorecard verdict weighted dimensions with critical floors",
		"rollout phase plan: shadow → partial → full cutover",
		"session lifecycle hook: on_session_start, on_session_summary, on_session_end",
		"backup restore lossless single-file artifact format",
		"doctor read-only diagnostics surface returns SchemaVersion + integrity",
		"memory relations graph table memory_relations(source_id,target_id)",
		"knowledge continuity baseline harness records hit-rate p95",
		"actionable recall next_action response with checklist",
		"import idempotent dedupe by topic_key + sha256 hash",
		"feature flag rollback: NTCLI_FF_GRAPH environment toggle",
	}
	for i, c := range contents {
		if _, err := store.Save(c, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed save: %v", err)
		}
	}
	dir := t.TempDir()
	return store, filepath.Join(dir, "baseline.json")
}

// TestRunContinuityHarness_WritesBaselineJSON exercises the read-only
// harness end-to-end against a real Service: load fixture → replay
// queries through Service.Recall → write baseline.json. Hit-rate must
// be 1.0 because the seeded store carries every expected_marker.
func TestRunContinuityHarness_WritesBaselineJSON(t *testing.T) {
	store, outPath := seedContinuityStore(t)
	svc := app.NewService(store, nil)

	baseline, err := svc.RunContinuityHarness("../../testdata/parity/queries.json", outPath)
	if err != nil {
		t.Fatalf("RunContinuityHarness: %v", err)
	}
	if baseline.Count < 10 {
		t.Errorf("Count: want >=10, got %d", baseline.Count)
	}
	if baseline.TopKHitRate != 1.0 {
		t.Errorf("TopKHitRate: want 1.0 (all markers seeded), got %v", baseline.TopKHitRate)
	}
	// File must exist and parse back to the same struct.
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var parsed model.ContinuityBaseline
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if parsed.Version != model.ContinuityContractVersion {
		t.Errorf("Version: want %q, got %q", model.ContinuityContractVersion, parsed.Version)
	}
	if parsed.Count != baseline.Count {
		t.Errorf("Count round-trip: want %d, got %d", baseline.Count, parsed.Count)
	}
}

// TestRunContinuityHarness_PartialMissReportsFraction triangulates the
// hit-rate path: with a store missing two markers, hit-rate is the
// remaining fraction. Forces real recall logic — a stub that always
// returns 1.0 would fail this test.
func TestRunContinuityHarness_PartialMissReportsFraction(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Seed only 10 of the 12 fixture markers; "import" and "flag"
	// are deliberately absent so they report misses.
	contents := []string{
		"JWT auth middleware",
		"FTS5 sanitize",
		"Parity scorecard verdict",
		"rollout phase",
		"session lifecycle hook",
		"backup restore lossless",
		"doctor read-only diagnostics",
		"memory relations graph",
		"knowledge continuity baseline",
		"actionable recall next_action",
	}
	for i, c := range contents {
		if _, err := store.Save(c, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	dir := t.TempDir()
	svc := app.NewService(store, nil)

	baseline, err := svc.RunContinuityHarness("../../testdata/parity/queries.json", filepath.Join(dir, "baseline.json"))
	if err != nil {
		t.Fatalf("RunContinuityHarness: %v", err)
	}
	wantHits := 10
	wantTotal := baseline.Count
	wantRate := float64(wantHits) / float64(wantTotal)
	if baseline.TopKHitRate != wantRate {
		t.Errorf("TopKHitRate: want %v (10/%d), got %v", wantRate, wantTotal, baseline.TopKHitRate)
	}
	misses := 0
	for _, q := range baseline.Queries {
		if !q.Hit {
			misses++
		}
	}
	if misses != wantTotal-wantHits {
		t.Errorf("misses: want %d, got %d", wantTotal-wantHits, misses)
	}
}

// TestRunContinuityHarness_RegenerationIsIdempotent — running the
// harness twice over the same store + fixture must produce identical
// metric fields (timestamps may differ; everything else must not).
func TestRunContinuityHarness_RegenerationIsIdempotent(t *testing.T) {
	store, _ := seedContinuityStore(t)
	svc := app.NewService(store, nil)
	dir := t.TempDir()

	a, err := svc.RunContinuityHarness("../../testdata/parity/queries.json", filepath.Join(dir, "a.json"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	b, err := svc.RunContinuityHarness("../../testdata/parity/queries.json", filepath.Join(dir, "b.json"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if a.Count != b.Count || a.TopKHitRate != b.TopKHitRate {
		t.Errorf("metrics drift across runs: %+v vs %+v", a, b)
	}
	if len(a.Queries) != len(b.Queries) {
		t.Fatalf("query count drift: %d vs %d", len(a.Queries), len(b.Queries))
	}
	for i := range a.Queries {
		if a.Queries[i].Hit != b.Queries[i].Hit ||
			a.Queries[i].Query != b.Queries[i].Query {
			t.Errorf("queries[%d] drift: %+v vs %+v", i, a.Queries[i], b.Queries[i])
		}
	}
}

// TestRunCLI_ParityContinuity exercises the CLI surface end-to-end:
// `nt-cli parity continuity --fixture=... --out=...` runs the harness
// and prints a JSON summary on stdout (so the runbook can pipe it).
func TestRunCLI_ParityContinuity(t *testing.T) {
	store, _ := seedContinuityStore(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "baseline.json")

	code, stdout, stderr := runCLI(t, store, "parity", "continuity",
		"--fixture=../../testdata/parity/queries.json",
		"--out="+out,
	)
	if code != 0 {
		t.Fatalf("exit: want 0, got %d (stderr=%q)", code, stderr)
	}
	// stdout must be a valid baseline JSON.
	var parsed model.ContinuityBaseline
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil {
		t.Fatalf("stdout is not baseline JSON: %v\nstdout=%q", err, stdout)
	}
	if parsed.Count < 10 {
		t.Errorf("Count: want >=10, got %d", parsed.Count)
	}
	// Out file must also be written.
	if _, err := os.Stat(out); err != nil {
		t.Errorf("out file missing: %v", err)
	}
}

// TestRunCLI_ParityContinuity_RejectsUnknownSubcommand keeps the
// dispatcher honest: typos must error, not silently succeed.
func TestRunCLI_ParityContinuity_RejectsUnknownSubcommand(t *testing.T) {
	store, _ := seedContinuityStore(t)
	code, _, stderr := runCLI(t, store, "parity", "continuityy")
	if code == 0 {
		t.Fatalf("typo should fail, got exit 0")
	}
	if !strings.Contains(stderr, "unknown parity subcommand") {
		t.Errorf("stderr: want unknown-subcommand error, got %q", stderr)
	}
}

// TestRunCLI_ParityContinuity_MissingFixtureErrors — when the fixture
// path doesn't exist, the runner fails loudly so silent zero-scores
// can't poison the scorecard.
func TestRunCLI_ParityContinuity_MissingFixtureErrors(t *testing.T) {
	store, _ := seedContinuityStore(t)
	dir := t.TempDir()
	code, _, stderr := runCLI(t, store, "parity", "continuity",
		"--fixture=/nonexistent/queries.json",
		"--out="+filepath.Join(dir, "out.json"),
	)
	if code == 0 {
		t.Fatalf("missing fixture must fail, got exit 0")
	}
	if !strings.Contains(stderr, "fixture") && !strings.Contains(stderr, "no such file") {
		t.Errorf("stderr should name the fixture problem, got %q", stderr)
	}
}
