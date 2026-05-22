package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// memStore is a minimal in-memory Store used to drive Service in MCP tests.
type memStore struct {
	items      map[int64]app.MemoryItem
	nextID     int64
	failGet    error
	failUpdate error
	obs        map[int64]app.BehavioralObservation
	obsNextID  int64
}

func newMemStore() *memStore {
	return &memStore{
		items:     map[int64]app.MemoryItem{},
		nextID:    1,
		obs:       map[int64]app.BehavioralObservation{},
		obsNextID: 1,
	}
}

func (m *memStore) Init() error { return nil }

func (m *memStore) Save(content string, createdAt time.Time) (int64, error) {
	id := m.nextID
	m.nextID++
	m.items[id] = app.MemoryItem{ID: id, Content: content, CreatedAt: createdAt, UpdatedAt: createdAt}
	return id, nil
}

func (m *memStore) Recall(query string, limit int) ([]app.MemoryItem, error) {
	return nil, nil
}

func (m *memStore) List(limit int) ([]app.MemoryItem, error) { return nil, nil }

func (m *memStore) Get(id int64) (app.MemoryItem, error) {
	if m.failGet != nil {
		return app.MemoryItem{}, m.failGet
	}
	it, ok := m.items[id]
	if !ok {
		return app.MemoryItem{}, app.ErrNotFound
	}
	return it, nil
}

func (m *memStore) Update(id int64, content string, updatedAt time.Time) (bool, error) {
	if m.failUpdate != nil {
		return false, m.failUpdate
	}
	it, ok := m.items[id]
	if !ok {
		return false, nil
	}
	it.Content = content
	it.UpdatedAt = updatedAt
	m.items[id] = it
	return true, nil
}

func (m *memStore) Delete(id int64) (bool, error) {
	if _, ok := m.items[id]; !ok {
		return false, nil
	}
	delete(m.items, id)
	return true, nil
}

func (m *memStore) Close() error { return nil }

func (m *memStore) RecordObservation(category, field, value string, confidence int, now time.Time) (int64, error) {
	for id, obs := range m.obs {
		if obs.Category == category && obs.Field == field && obs.Value == value {
			obs.OccurrenceCount++
			obs.LastSeen = now
			if obs.OccurrenceCount >= 3 && obs.Confidence > 60 {
				obs.Status = "candidate"
			}
			m.obs[id] = obs
			return id, nil
		}
	}
	id := m.obsNextID
	m.obsNextID++
	m.obs[id] = app.BehavioralObservation{
		ID:              id,
		Category:        category,
		Field:           field,
		Value:           value,
		Confidence:      confidence,
		OccurrenceCount: 1,
		Status:          "observed",
		LastSeen:        now,
		CreatedAt:       now,
	}
	return id, nil
}

func (m *memStore) ListObservations(includeStatuses []string) ([]app.BehavioralObservation, error) {
	allowed := map[string]bool{}
	for _, st := range includeStatuses {
		allowed[st] = true
	}
	out := make([]app.BehavioralObservation, 0, len(m.obs))
	for _, obs := range m.obs {
		if allowed[obs.Status] {
			out = append(out, obs)
		}
	}
	return out, nil
}

func (m *memStore) GetObservation(id int64) (*app.BehavioralObservation, error) {
	obs, ok := m.obs[id]
	if !ok {
		return nil, app.ErrNotFound
	}
	return &obs, nil
}

func (m *memStore) DismissObservation(id int64) error {
	obs, ok := m.obs[id]
	if !ok {
		return nil
	}
	obs.Status = "dismissed"
	m.obs[id] = obs
	return nil
}

func (m *memStore) Candidates() ([]app.BehavioralObservation, error) {
	out := []app.BehavioralObservation{}
	for _, obs := range m.obs {
		if obs.Status == "candidate" {
			out = append(out, obs)
		}
	}
	return out, nil
}

func newCallReq(t *testing.T, name string, args interface{}) []byte {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	params, err := json.Marshal(toolsCallParams{Name: name, Arguments: rawArgs})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	return b
}

func toolPayloadText(t *testing.T, resp response) (string, bool) {
	t.Helper()
	m, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", resp.Result)
	}
	isErr, _ := m["isError"].(bool)
	content, ok := m["content"].([]map[string]string)
	if !ok {
		t.Fatalf("expected content slice, got %T", m["content"])
	}
	if len(content) == 0 {
		t.Fatalf("empty content")
	}
	return content[0]["text"], isErr
}

func TestToolsList_IncludesGetAndUpdate(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	m := resp.Result.(map[string]interface{})
	tools := m["tools"].([]map[string]interface{})
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	if !names["local_get"] {
		t.Fatalf("expected local_get tool to be advertised, got %v", names)
	}
	if !names["local_update"] {
		t.Fatalf("expected local_update tool to be advertised, got %v", names)
	}
}

func TestLocalGet_ExistingIDReturnsRecord(t *testing.T) {
	store := newMemStore()
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	id, _ := store.Save("hello", created)
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_get", map[string]interface{}{"id": id})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("expected non-error result, got error: %s", text)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("expected content in payload, got %q", text)
	}
	// Must contain id and timestamps
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("expected JSON payload, got %q (err %v)", text, err)
	}
	if int64(got["id"].(float64)) != id {
		t.Fatalf("expected id=%d, got %v", id, got["id"])
	}
	if got["content"].(string) != "hello" {
		t.Fatalf("expected content=hello, got %v", got["content"])
	}
	if _, ok := got["created_at"]; !ok {
		t.Fatalf("expected created_at field in payload: %v", got)
	}
	if _, ok := got["updated_at"]; !ok {
		t.Fatalf("expected updated_at field in payload: %v", got)
	}
}

func TestLocalGet_MissingIDReturnsError(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_get", map[string]interface{}{"id": 999})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	text, isErr := toolPayloadText(t, resp)
	if !isErr {
		t.Fatalf("expected isError=true on missing id, got payload %q", text)
	}
}

func TestLocalGet_InvalidIDReturnsError(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_get", map[string]interface{}{"id": 0})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	_, isErr := toolPayloadText(t, resp)
	if !isErr {
		t.Fatalf("expected isError=true on id=0")
	}
}

func TestLocalUpdate_ExistingIDUpdatesContent(t *testing.T) {
	store := newMemStore()
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	id, _ := store.Save("old", created)
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_update", map[string]interface{}{"id": id, "content": "new"})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("expected success, got error %q", text)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Content != "new" {
		t.Fatalf("expected updated content=new, got %q", got.Content)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		// allow equal or after; we only require UTC and forward progress
		t.Fatalf("expected UpdatedAt >= CreatedAt, got created=%s updated=%s", got.CreatedAt, got.UpdatedAt)
	}
}

func TestLocalUpdate_MissingIDReturnsError(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_update", map[string]interface{}{"id": 999, "content": "x"})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	_, isErr := toolPayloadText(t, resp)
	if !isErr {
		t.Fatalf("expected isError=true for missing id update")
	}
}

func TestLocalUpdate_EmptyContentReturnsError(t *testing.T) {
	store := newMemStore()
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	id, _ := store.Save("old", created)
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_update", map[string]interface{}{"id": id, "content": "   "})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	_, isErr := toolPayloadText(t, resp)
	if !isErr {
		t.Fatalf("expected isError=true for empty content")
	}
	got, _ := store.Get(id)
	if got.Content != "old" {
		t.Fatalf("expected content unchanged on validation failure, got %q", got.Content)
	}
}

func TestLocalRecordObservation_ValidMarkerReturnsID(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_record_observation", map[string]interface{}{
		"marker": "[BEHAVIORAL_OBSERVATION: category=tone, field=language, value=es, confidence=90]",
	})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("expected non-error result, got %q", text)
	}
	if !strings.Contains(text, "#") {
		t.Fatalf("expected id in response text, got %q", text)
	}
}

func TestLocalRecordObservation_MalformedMarkerReturnsNonFatalTextError(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, store)

	payload := newCallReq(t, "local_record_observation", map[string]interface{}{
		"marker": "[BEHAVIORAL_OBSERVATION: category=tone, value=es, confidence=90]",
	})
	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("expected non-fatal tool text error, got rpc error: %+v", resp.Error)
	}
	text, isErr := toolPayloadText(t, resp)
	if isErr {
		t.Fatalf("expected non-fatal text (isError=false), got isError=true text=%q", text)
	}
	if !strings.Contains(strings.ToLower(text), "invalid marker") {
		t.Fatalf("expected marker validation text, got %q", text)
	}
}
