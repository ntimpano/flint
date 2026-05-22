package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// metaFakeStore implements MetadataStore on top of fakeStore so we can
// observe what the service forwards to the store layer. Keeping it separate
// from fakeStore preserves the legacy fakeStore behaviour for callers that
// do not exercise metadata.
type metaFakeStore struct {
	fakeStore
	saveMetaCalls int
	lastReq       SaveRequest
	saveMetaID    int64
	saveMetaErr   error
}

func (m *metaFakeStore) SaveWithMeta(req SaveRequest) (int64, error) {
	m.saveMetaCalls++
	m.lastReq = req
	if m.saveMetaID == 0 {
		return 42, m.saveMetaErr
	}
	return m.saveMetaID, m.saveMetaErr
}

// TestServiceSaveWithMeta_AppliesDefaultsWhenFieldsEmpty proves spec
// scenario "Save without metadata stays backward-compatible": when the
// caller does not provide type/scope, the service stamps `type=manual`
// and `scope=project` per design.md.
func TestServiceSaveWithMeta_AppliesDefaultsWhenFieldsEmpty(t *testing.T) {
	store := &metaFakeStore{}
	svc := NewService(store, nil)

	id, err := svc.SaveWithMeta(SaveRequest{Content: "hello"})
	if err != nil {
		t.Fatalf("SaveWithMeta: %v", err)
	}
	if id != 42 {
		t.Fatalf("expected id forwarded from store, got %d", id)
	}
	if store.saveMetaCalls != 1 {
		t.Fatalf("expected SaveWithMeta to be invoked once, got %d", store.saveMetaCalls)
	}
	if store.lastReq.Type != "manual" {
		t.Fatalf("expected default type=manual, got %q", store.lastReq.Type)
	}
	if store.lastReq.Scope != "project" {
		t.Fatalf("expected default scope=project, got %q", store.lastReq.Scope)
	}
	if store.lastReq.Content != "hello" {
		t.Fatalf("expected content forwarded, got %q", store.lastReq.Content)
	}
	// CreatedAt MUST be stamped by the service so the store gets a
	// deterministic, non-zero timestamp.
	if store.lastReq.CreatedAt.IsZero() {
		t.Fatalf("service must stamp CreatedAt before forwarding")
	}
}

// TestServiceSaveWithMeta_PreservesExplicitMetadata proves spec scenario
// "Save with full metadata persists fields": user-supplied metadata is
// forwarded verbatim and is NOT overwritten by defaults.
func TestServiceSaveWithMeta_PreservesExplicitMetadata(t *testing.T) {
	store := &metaFakeStore{}
	svc := NewService(store, nil)

	_, err := svc.SaveWithMeta(SaveRequest{
		Content:  "decision body",
		Title:    "Auth Model",
		Type:     "decision",
		TopicKey: "arch/auth",
		Scope:    "personal",
	})
	if err != nil {
		t.Fatalf("SaveWithMeta: %v", err)
	}
	if store.lastReq.Type != "decision" {
		t.Fatalf("expected type preserved, got %q", store.lastReq.Type)
	}
	if store.lastReq.Scope != "personal" {
		t.Fatalf("expected scope preserved, got %q", store.lastReq.Scope)
	}
	if store.lastReq.Title != "Auth Model" {
		t.Fatalf("expected title preserved, got %q", store.lastReq.Title)
	}
	if store.lastReq.TopicKey != "arch/auth" {
		t.Fatalf("expected topic_key preserved, got %q", store.lastReq.TopicKey)
	}
}

// TestServiceSaveWithMeta_TrimsAndValidatesContent proves the same content
// validation Save() applies — empty/whitespace content is rejected before
// the store is called.
func TestServiceSaveWithMeta_TrimsAndValidatesContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"whitespace only", "   \t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &metaFakeStore{}
			svc := NewService(store, nil)
			_, err := svc.SaveWithMeta(SaveRequest{Content: tc.content})
			if err == nil {
				t.Fatalf("expected error for %q content", tc.content)
			}
			if store.saveMetaCalls != 0 {
				t.Fatalf("store must not be called when validation fails (calls=%d)", store.saveMetaCalls)
			}
		})
	}

	// Trimming: leading/trailing whitespace is stripped before forwarding.
	store := &metaFakeStore{}
	svc := NewService(store, nil)
	if _, err := svc.SaveWithMeta(SaveRequest{Content: "  body  "}); err != nil {
		t.Fatalf("SaveWithMeta: %v", err)
	}
	if store.lastReq.Content != "body" {
		t.Fatalf("expected trimmed content %q, got %q", "body", store.lastReq.Content)
	}
}

// TestServiceSaveWithMeta_FailsGracefullyWhenStoreLacksMetadata proves the
// MetadataStore degradation path: when the underlying store does not
// implement MetadataStore (legacy fakes, embedded backends), the service
// MUST return a clear error rather than silently dropping metadata.
func TestServiceSaveWithMeta_FailsGracefullyWhenStoreLacksMetadata(t *testing.T) {
	legacy := &fakeStore{} // implements Store but NOT MetadataStore
	svc := NewService(legacy, nil)

	_, err := svc.SaveWithMeta(SaveRequest{Content: "hello", Type: "decision"})
	if err == nil {
		t.Fatalf("expected error when store does not support metadata")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "metadata") {
		t.Fatalf("error must mention metadata support, got %q", err.Error())
	}
	if legacy.saveCalls != 0 {
		t.Fatalf("legacy Save MUST NOT be silently called, got %d", legacy.saveCalls)
	}
}

// TestServiceSaveWithMeta_PropagatesStoreError proves errors from the store
// surface directly to the caller — no swallowing.
func TestServiceSaveWithMeta_PropagatesStoreError(t *testing.T) {
	store := &metaFakeStore{saveMetaErr: errors.New("boom")}
	svc := NewService(store, nil)
	_, err := svc.SaveWithMeta(SaveRequest{Content: "hello"})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected store error to propagate, got %v", err)
	}
}

// TestServiceSaveWithMeta_StampsCreatedAtUTC proves the service stamps
// CreatedAt in UTC even when the system locale differs, matching the
// existing Save() contract.
func TestServiceSaveWithMeta_StampsCreatedAtUTC(t *testing.T) {
	store := &metaFakeStore{}
	svc := NewService(store, nil)

	before := time.Now().UTC().Add(-time.Second)
	if _, err := svc.SaveWithMeta(SaveRequest{Content: "x"}); err != nil {
		t.Fatalf("SaveWithMeta: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	got := store.lastReq.CreatedAt
	if got.Location() != time.UTC {
		t.Fatalf("expected CreatedAt in UTC, got %s", got.Location())
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("CreatedAt %s outside [%s, %s]", got, before, after)
	}
}
