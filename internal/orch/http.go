package orch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/popododo0720/pvebot/internal/host"
	"github.com/popododo0720/pvebot/internal/store"
)

type Handler struct {
	svc *Service
	web *pages
	mux http.Handler
}

func NewHandler(svc *Service) *Handler {
	h := &Handler{svc: svc, web: newPages()}
	h.mux = h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/nodes", h.nodes)
	mux.HandleFunc("GET /api/accounts", h.listAccounts)
	mux.HandleFunc("POST /api/accounts", h.createAccount)
	mux.HandleFunc("GET /api/bots", h.listBots)
	mux.HandleFunc("POST /api/bots", h.createBot)
	mux.HandleFunc("GET /api/bots/{name}", h.getBot)
	mux.HandleFunc("DELETE /api/bots/{name}", h.deleteBot)
	mux.HandleFunc("POST /api/bots/{name}/start", h.startBot)
	mux.HandleFunc("POST /api/bots/{name}/stop", h.stopBot)
	mux.HandleFunc("GET /api/bots/{name}/logs", h.logs)
	mux.HandleFunc("GET /api/bots/{name}/messages", h.listMessages)
	mux.HandleFunc("POST /api/bots/{name}/messages", h.postMessage)
	mux.HandleFunc("GET /api/bots/{name}/inbox", h.inbox)
	mux.HandleFunc("POST /api/bots/{name}/inbox/{id}/log", h.logChunk)
	mux.HandleFunc("POST /api/bots/{name}/inbox/{id}/result", h.result)
	mux.HandleFunc("POST /mcp", h.mcp)
	mux.Handle("GET /static/", h.web.static())
	mux.HandleFunc("GET /{$}", h.web.index(h.svc))
	mux.HandleFunc("GET /bots/new", h.web.newBot(h.svc))
	mux.HandleFunc("POST /bots", h.web.createBot(h.svc))
	mux.HandleFunc("GET /bots/{name}", h.web.bot(h.svc))
	mux.HandleFunc("POST /bots/{name}/message", h.web.postMessage(h.svc))
	mux.HandleFunc("POST /bots/{name}/start", h.web.start(h.svc))
	mux.HandleFunc("POST /bots/{name}/stop", h.web.stop(h.svc))
	mux.HandleFunc("POST /bots/{name}/delete", h.web.delete(h.svc))
	mux.HandleFunc("GET /accounts", h.web.accounts(h.svc))
	mux.HandleFunc("POST /accounts", h.web.createAccount(h.svc))
	return mux
}

func (h *Handler) nodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.svc.Nodes(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if nodes == nil {
		nodes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Store.Accounts()
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []store.Account{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Secret string `json:"secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, BadRequest{Msg: err.Error()})
		return
	}
	if err := h.svc.PutAccount(req.Name, req.Kind, req.Secret); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) createBot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Node    string `json:"node"`
		Backend string `json:"backend"`
		Account string `json:"account"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, BadRequest{Msg: err.Error()})
		return
	}
	token, err := h.svc.CreateBot(r.Context(), req.Name, req.Node, req.Backend, req.Account)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "token": token})
}

func (h *Handler) listBots(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getBot(w http.ResponseWriter, r *http.Request) {
	view, err := h.svc.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) deleteBot(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) startBot(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Start(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) stopBot(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Stop(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	text, err := h.svc.Logs(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": text})
}

func (h *Handler) listMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.svc.Messages(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, BadRequest{Msg: err.Error()})
		return
	}
	id, err := h.svc.PostUser(r.PathValue("name"), req.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"id": id})
}

func (h *Handler) inbox(w http.ResponseWriter, r *http.Request) {
	msg, ok, err := h.svc.Inbox(bearer(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, BadRequest{Msg: "bad message id"})
		return
	}
	var req struct {
		Body   string `json:"body"`
		Failed bool   `json:"failed"`
		Status string `json:"status"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, BadRequest{Msg: err.Error()})
		return
	}
	status := req.Status
	if status == "" {
		status = store.StatusDone
		if req.Failed {
			status = store.StatusError
		}
	}
	if err := h.svc.Complete(bearer(r), id, req.Body, status); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logChunk(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, BadRequest{Msg: "bad message id"})
		return
	}
	var req struct {
		Chunk string `json:"chunk"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, BadRequest{Msg: err.Error()})
		return
	}
	if err := h.svc.AppendLog(bearer(r), id, req.Chunk); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readJSON(r *http.Request, dest any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var bad BadRequest
	switch {
	case errors.As(err, &bad):
		code = http.StatusBadRequest
	case errors.Is(err, host.ErrExists), errors.Is(err, store.ErrExists):
		code = http.StatusConflict
	case errors.Is(err, host.ErrNotFound), errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
