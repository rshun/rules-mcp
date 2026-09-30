package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
)

const ProtocolVersion = "2025-11-25"

var Version = "0.4.0"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func supportedVersion(v string) bool {
	return v == "2025-03-26" || v == "2025-06-18" || v == ProtocolVersion
}

func (a *App) Handler() http.Handler { return http.HandlerFunc(a.serveHTTP) }
func (a *App) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Same-host server-to-server clients do not send Origin. Reject browser
	// requests entirely, including null Origin, to prevent DNS rebinding.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if r.Header.Get("Origin") != "" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		http.Error(w, "forbidden origin or host", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedVersion(v) {
		http.Error(w, "unsupported protocol version", http.StatusBadRequest)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		http.Error(w, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "request body exceeds limit or cannot be read", http.StatusRequestEntityTooLarge)
		return
	}
	var req rpcRequest
	if !json.Valid(body) {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request"}})
		return
	}
	if len(req.ID) == 0 {
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
		} else {
			http.Error(w, "requests require an id", http.StatusBadRequest)
		}
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.ProtocolVersion == "" {
			resp.Error = &rpcError{-32602, "invalid initialize parameters"}
			break
		}
		version := p.ProtocolVersion
		if !supportedVersion(version) {
			version = ProtocolVersion
		}
		resp.Result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "rules-mcp", "version": Version}, "instructions": "Use rules_preview before rules_apply and pass its revision as expected_revision. Apply commits and atomically pushes configured branches. Use rules_resume for partial failures."}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefinitions()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if Decode(req.Params, &p) != nil || !knownTool(p.Name) {
			resp.Error = &rpcError{-32602, "unknown tool or invalid tool parameters"}
			break
		}
		if err = validateArguments(p.Name, p.Arguments); err != nil {
			resp.Result = toolResult(nil, err)
			break
		}
		// An HTTP disconnect must not cancel an already started publication.
		result, callErr := a.Execute(context.WithoutCancel(r.Context()), p.Name, p.Arguments)
		resp.Result = toolResult(result, callErr)
	default:
		resp.Error = &rpcError{-32601, "method not found"}
	}
	writeRPC(w, resp)
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := v.Int64()
		return err == nil
	}
	return false
}
func writeRPC(w http.ResponseWriter, r rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(r)
}
func toolResult(value any, err error) any {
	if err != nil {
		if value == nil {
			value = map[string]any{"error": err.Error()}
		} else if m, ok := value.(map[string]any); ok {
			m["error"] = err.Error()
		}
	}
	b, _ := json.Marshal(value)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "isError": err != nil}
}

var toolFields = map[string][]string{
	"rules_list": {}, "rules_status": {}, "rules_resume": {},
	"rules_read":    {"name", "offset", "limit"},
	"rules_preview": {"name", "add", "remove"},
	"rules_apply":   {"name", "add", "remove", "expected_revision"},
}

func knownTool(name string) bool { _, ok := toolFields[name]; return ok }
func validateArguments(name string, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fmt.Errorf("arguments must be an object")
	}
	allowed := map[string]bool{}
	for _, field := range toolFields[name] {
		allowed[field] = true
	}
	for field, value := range fields {
		if !allowed[field] || string(value) == "null" {
			return fmt.Errorf("invalid tool argument")
		}
	}
	if allowed["name"] {
		var value string
		if json.Unmarshal(fields["name"], &value) != nil || value == "" {
			return fmt.Errorf("name is required")
		}
	}
	return nil
}

func toolDefinitions() []any {
	type spec struct {
		name, description string
		readOnly          bool
	}
	specs := []spec{
		{"rules_list", "列出 YAML 规则组。", true},
		{"rules_read", "分页读取规则、版本、JSON 规则验证结果及 YAML/JSON 一致性，默认 100 条、最多 500 条。", true},
		{"rules_preview", "预览新增/删除规则并返回 revision；空修改可预览重新生成 JSON。不写规则。", true},
		{"rules_apply", "拉取最新开发分支后应用变更，生成 JSON、自动 commit、原子 push 开发分支及 publish_branch；不备份。必须提供预览 revision。", false},
		{"rules_status", "查看最近操作阶段和提交，不查询远端。", true},
		{"rules_resume", "继续未完成的写入、提交或推送；不强推，不解决 Git 冲突。", false},
	}
	result := []any{}
	for _, s := range specs {
		properties := map[string]any{}
		for _, field := range toolFields[s.name] {
			switch field {
			case "add", "remove":
				properties[field] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 2000}
			case "offset":
				properties[field] = map[string]any{"type": "integer", "minimum": 0}
			case "limit":
				properties[field] = map[string]any{"type": "integer", "minimum": 1, "maximum": 500}
			default:
				properties[field] = map[string]any{"type": "string"}
			}
		}
		required := []string{}
		if _, ok := properties["name"]; ok {
			required = append(required, "name")
		}
		if s.name == "rules_apply" {
			required = append(required, "expected_revision")
		}
		result = append(result, map[string]any{"name": s.name, "description": s.description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}, "annotations": map[string]any{"readOnlyHint": s.readOnly, "destructiveHint": !s.readOnly, "idempotentHint": s.readOnly || s.name == "rules_resume", "openWorldHint": !s.readOnly}})
	}
	return result
}
