package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	actor, admin := s.actor(r)
	if actor == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if !s.allowWrite(r) { w.Header().Set("Retry-After", "60"); http.Error(w, "rate limited", 429); return }
	var q request
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if json.NewDecoder(r.Body).Decode(&q) != nil || q.JSONRPC != "2.0" {
		http.Error(w, "invalid JSON-RPC", 400)
		return
	}
	if len(q.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	var result any
	switch q.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "botpages", "version": "0.1.0"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "publish_page", "description": "Publish one HTML/CSS/JS file to a site", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"site": map[string]string{"type": "string"}, "path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"site", "path", "content"}}}, map[string]any{"name": "list_sites", "description": "List sites visible to the bot", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(q.Params, &p) != nil {
			result = map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "invalid arguments"}}}
			break
		}
		rr := httptest.NewRequest("GET", "/api/sites", nil)
		rr.Header = r.Header.Clone()
		if p.Name == "publish_page" {
			rr = httptest.NewRequest("POST", "/api/publish", bytes.NewReader(p.Arguments))
			rr.Header = r.Header.Clone()
			rr.Header.Set("Authorization", r.Header.Get("Authorization"))
		}
		rw := httptest.NewRecorder()
		if p.Name == "list_sites" {
			s.list(rw, rr)
		} else if p.Name == "publish_page" {
			s.publish(rw, rr, actor, admin)
		} else {
			rw.Code = 400
			rw.Body.WriteString("unknown tool")
		}
		result = map[string]any{"isError": rw.Code >= 400, "content": []any{map[string]string{"type": "text", "text": rw.Body.String()}}}
	default:
		respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
}
