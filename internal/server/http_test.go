package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(a *App, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestHTTPProtocolAndTools(t *testing.T) {
	a, _ := fixture(t)
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25", "unknown"} {
		w := request(a, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
		var resp rpcResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Error != nil {
			t.Fatalf("initialize failed: %s", w.Body.String())
		}
		want := version
		if version == "unknown" {
			want = ProtocolVersion
		}
		if resp.Result.(map[string]any)["protocolVersion"] != want {
			t.Fatal("wrong negotiation")
		}
	}
	w := request(a, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if w.Code != 202 || w.Body.Len() != 0 {
		t.Fatal("notification handling")
	}
	w = request(a, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`, nil)
	var resp struct {
		Result struct {
			Tools []any `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(w.Body.Bytes(), &resp) != nil || len(resp.Result.Tools) != 6 {
		t.Fatal("tool discovery failed")
	}
	if strings.Contains(w.Body.String(), "rules_sync") {
		t.Fatal("removed tool still advertised")
	}
	w = request(a, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"rules_sync","arguments":{}}}`, nil)
	if !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatal("removed tool still callable")
	}
	w = request(a, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rules_read","arguments":{"name":"sample"}}}`, nil)
	if !strings.Contains(w.Body.String(), `"isError":false`) || !strings.Contains(w.Body.String(), "example.com") {
		t.Fatalf("read failed: %s", w.Body.String())
	}
	w = request(a, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rules_preview","arguments":{"name":"sample","typo":true}}}`, nil)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("unknown argument accepted")
	}
	w = request(a, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rules_read","arguments":{"name":"../escape"}}}`, nil)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("traversal accepted")
	}
}

func TestHTTPRejectsBrowserAndMalformedRequests(t *testing.T) {
	a, _ := fixture(t)
	for _, tc := range []struct {
		headers map[string]string
		status  int
	}{
		{map[string]string{"Origin": "http://malicious.example"}, 403},
		{map[string]string{"Origin": "null"}, 403},
		{map[string]string{"Host": "malicious.example"}, 403},
		{map[string]string{"MCP-Protocol-Version": "unknown"}, 400},
		{map[string]string{"Content-Type": "text/plain"}, 415},
		{map[string]string{"Accept": "application/json"}, 406},
	} {
		w := request(a, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, tc.headers)
		if w.Code != tc.status {
			t.Fatalf("want %d got %d", tc.status, w.Code)
		}
	}
	for _, body := range []string{`{`, `[]`, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, `{"jsonrpc":"2.0","id":false,"method":"ping"}`} {
		w := request(a, body, nil)
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("malformed accepted: %s", body)
		}
	}
	w := request(a, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"rules_apply","arguments":{}}}`, nil)
	if w.Code != 400 {
		t.Fatal("request without id must not execute")
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/mcp", nil))
	if w.Code != 405 {
		t.Fatal("GET should reject optional SSE stream")
	}
	w = request(a, strings.Repeat(" ", (1<<20)+1), nil)
	if w.Code != 413 {
		t.Fatal("oversized request accepted")
	}
}

func TestBusyOperationDoesNotQueueMutation(t *testing.T) {
	a, _ := fixture(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	w := request(a, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rules_status","arguments":{}}}`, nil)
	if !strings.Contains(w.Body.String(), "another rules-mcp operation") {
		t.Fatal("concurrent request not rejected")
	}
}
