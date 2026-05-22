package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flint/internal/app"
)

func TestSSEHandshake_EmitsEndpointEvent(t *testing.T) {
	srv := &Server{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleSSE))
	defer httpSrv.Close()

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	const endpointEvent = "event: endpoint\ndata: /message\n\n"
	body := make([]byte, len(endpointEvent))
	_, err = io.ReadFull(resp.Body, body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "event: endpoint") {
		t.Fatalf("missing endpoint event, got %q", text)
	}
	if !strings.Contains(text, "data: /message") {
		t.Fatalf("missing /message endpoint data, got %q", text)
	}
}

func TestSSEMessage_PostReturnsJSONRPCResponse(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, nil)
	srv := &Server{svc: svc}

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleMessage))
	defer httpSrv.Close()

	raw, err := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp, err := http.Post(httpSrv.URL+"/message", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /message: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got response
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", got.Error)
	}
	if got.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", got.JSONRPC)
	}
}

func TestSSEMessage_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleMessage))
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/message")
	if err != nil {
		t.Fatalf("GET /message: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRunSSEBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	srv := &Server{}
	err = srv.RunSSE(port, "127.0.0.1")
	if err == nil {
		t.Fatalf("expected bind error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "address already in use") {
		t.Fatalf("expected address-in-use error, got %v", err)
	}
}

func TestSSEToolParityWithStdioToolsList(t *testing.T) {
	store := newMemStore()
	svc := app.NewService(store, nil)

	stdioReq, err := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if err != nil {
		t.Fatalf("marshal stdio req: %v", err)
	}
	stdioResp, ok := handleRequest(stdioReq, svc)
	if !ok || stdioResp.Error != nil {
		t.Fatalf("stdio tools/list failed: ok=%v err=%+v", ok, stdioResp.Error)
	}
	stdioTools := toolNamesFromResponseResult(t, stdioResp.Result)

	srv := &Server{svc: svc}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleMessage))
	defer httpSrv.Close()

	httpResp, err := http.Post(httpSrv.URL+"/message", "application/json", bytes.NewReader(stdioReq))
	if err != nil {
		t.Fatalf("POST /message: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", httpResp.StatusCode)
	}
	var sseResp response
	if err := json.NewDecoder(httpResp.Body).Decode(&sseResp); err != nil {
		t.Fatalf("decode SSE response: %v", err)
	}
	if sseResp.Error != nil {
		t.Fatalf("SSE tools/list rpc error: %+v", sseResp.Error)
	}
	sseTools := toolNamesFromResponseResult(t, sseResp.Result)

	if len(stdioTools) != len(sseTools) {
		t.Fatalf("tool parity mismatch: stdio=%d sse=%d", len(stdioTools), len(sseTools))
	}
	for _, name := range stdioTools {
		if !containsString(sseTools, name) {
			t.Fatalf("tool %q present in stdio but missing in sse", name)
		}
	}
}

func toolNamesFromResponseResult(t *testing.T, result interface{}) []string {
	t.Helper()
	rm, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result type %T, want map", result)
	}
	rawTools := rm["tools"]
	if rawTools == nil {
		t.Fatalf("missing tools field in result")
	}
	arr, ok := rawTools.([]map[string]interface{})
	if ok {
		names := make([]string, 0, len(arr))
		for _, tool := range arr {
			name, _ := tool["name"].(string)
			names = append(names, name)
		}
		return names
	}
	ifaceArr, ok := rawTools.([]interface{})
	if !ok {
		t.Fatalf("tools type %T, want []interface{}", rawTools)
	}
	names := make([]string, 0, len(ifaceArr))
	for _, item := range ifaceArr {
		tool, _ := item.(map[string]interface{})
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
