package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"flint/internal/app"
)

// metaMemStore extends memStore (defined in server_test.go) with
// MetadataStore so the MCP handler under test can exercise the
// metadata-aware local_save path.
type metaMemStore struct {
	*memStore
	lastReq      app.SaveRequest
	saveMetaHits int
}

func newMetaMemStoreMCP() *metaMemStore {
	return &metaMemStore{memStore: newMemStore()}
}

func (m *metaMemStore) SaveWithMeta(req app.SaveRequest) (int64, error) {
	m.saveMetaHits++
	m.lastReq = req
	id, err := m.memStore.Save(req.Content, req.CreatedAt)
	return id, err
}

var _ app.Store = (*metaMemStore)(nil)
var _ app.MetadataStore = (*metaMemStore)(nil)

// TestMCP_LocalSaveAcceptsMetadataFields proves task 1.7 + spec scenario
// "MCP local_save accepts optional metadata": invoking local_save with
// title/type/topic_key/scope MUST forward them to SaveWithMeta and
// return success.
func TestMCP_LocalSaveAcceptsMetadataFields(t *testing.T) {
	store := newMetaMemStoreMCP()
	svc := app.NewService(store, nil)

	args := map[string]interface{}{
		"content":   "decision body",
		"title":     "Auth Model",
		"type":      "decision",
		"topic_key": "arch/auth",
		"scope":     "personal",
	}
	argsJSON, _ := json.Marshal(args)
	params := map[string]interface{}{
		"name":      "local_save",
		"arguments": json.RawMessage(argsJSON),
	}
	paramsJSON, _ := json.Marshal(params)
	req := request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  paramsJSON,
	}
	payload, _ := json.Marshal(req)

	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	if store.saveMetaHits != 1 {
		t.Fatalf("expected SaveWithMeta to be invoked once, got %d", store.saveMetaHits)
	}
	if store.lastReq.Title != "Auth Model" {
		t.Fatalf("title: %q", store.lastReq.Title)
	}
	if store.lastReq.Type != "decision" {
		t.Fatalf("type: %q", store.lastReq.Type)
	}
	if store.lastReq.TopicKey != "arch/auth" {
		t.Fatalf("topic_key: %q", store.lastReq.TopicKey)
	}
	if store.lastReq.Scope != "personal" {
		t.Fatalf("scope: %q", store.lastReq.Scope)
	}
	if store.lastReq.Content != "decision body" {
		t.Fatalf("content: %q", store.lastReq.Content)
	}
}

// TestMCP_LocalSaveWithoutMetadataStaysBackwardCompatible proves the
// degradation path: when metadata fields are absent from the JSON-RPC
// arguments, local_save MUST behave identically to the legacy contract
// (Service.Save, no MetadataStore requirement).
func TestMCP_LocalSaveWithoutMetadataStaysBackwardCompatible(t *testing.T) {
	store := newMemStore() // legacy fake, no MetadataStore
	svc := app.NewService(store, nil)

	args := map[string]interface{}{"content": "plain"}
	argsJSON, _ := json.Marshal(args)
	params := map[string]interface{}{
		"name":      "local_save",
		"arguments": json.RawMessage(argsJSON),
	}
	paramsJSON, _ := json.Marshal(params)
	req := request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: paramsJSON}
	payload, _ := json.Marshal(req)

	resp, ok := handleRequest(payload, svc)
	if !ok {
		t.Fatalf("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	// Result should be a success text payload with "saved #".
	m, _ := resp.Result.(map[string]interface{})
	if m == nil {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if m["isError"] == true {
		t.Fatalf("legacy save returned isError=true: %+v", m)
	}
	content, _ := m["content"].([]map[string]string)
	if len(content) == 0 || !strings.Contains(content[0]["text"], "saved #") {
		t.Fatalf("expected 'saved #' confirmation, got %+v", m)
	}
}

// TestMCP_LocalSaveSchemaAdvertisesMetadataFields proves the schema
// advertised through tools/list exposes the four optional metadata
// properties so MCP clients can discover them.
func TestMCP_LocalSaveSchemaAdvertisesMetadataFields(t *testing.T) {
	tools := advertisedTools(t)
	var save map[string]interface{}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name == "local_save" {
			save = tool
			break
		}
	}
	if save == nil {
		t.Fatal("local_save not advertised")
	}
	schema, _ := save["inputSchema"].(map[string]interface{})
	props, _ := schema["properties"].(map[string]interface{})
	for _, key := range []string{"title", "type", "topic_key", "scope"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("local_save schema must advertise optional %q, got props=%v", key, propertyNames(props))
		}
	}
	// content remains the only required field.
	required := toStringSlice(schema["required"])
	if !sameStringSet(required, []string{"content"}) {
		t.Fatalf("local_save required must remain %v, got %v", []string{"content"}, required)
	}
}
