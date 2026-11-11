package orch

import (
	"encoding/json"
	"io"
	"net/http"
)

func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req jsonRPC
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, jsonRPC{Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	token := bearer(r)
	from, authErr := h.svc.Store.BotByToken(token)
	resp := jsonRPC{ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pvebot", "version": "0.1.0"},
		}
	case "notifications/initialized", "initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{sendTool()}}
	case "tools/call":
		if authErr != nil {
			resp.Error = &rpcError{Code: -32001, Message: "unauthorized"}
			break
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			break
		}
		if params.Name != "send_to_agent" {
			resp.Error = &rpcError{Code: -32602, Message: "unknown tool"}
			break
		}
		var args struct {
			Agent string `json:"agent"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid arguments"}
			break
		}
		reply, err := h.svc.SendToAgent(r.Context(), from, args.Agent, args.Text)
		if err != nil {
			resp.Result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			break
		}
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": reply}},
			"isError": false,
		}
	default:
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	writeRPC(w, resp)
}

func sendTool() map[string]any {
	return map[string]any{
		"name":        "send_to_agent",
		"description": "Send a message to another bot and wait for its reply.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{"type": "string"},
				"text":  map[string]any{"type": "string"},
			},
			"required": []string{"agent", "text"},
		},
	}
}

type jsonRPC struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, resp jsonRPC) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
