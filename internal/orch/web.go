package orch

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/popododo0720/pvebot/internal/host"
	"github.com/popododo0720/pvebot/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type pages struct {
	tmpl *template.Template
}

func newPages() *pages {
	t := template.New("pages").Funcs(template.FuncMap{
		"phaseClass": phaseClass,
	})
	t = template.Must(t.ParseFS(assets, "templates/*.html"))
	return &pages{tmpl: t}
}

func phaseClass(s string) string {
	switch s {
	case "running", "stopped", "pending", "failed", "unknown":
		return "phase phase-" + s
	default:
		return "phase phase-unknown"
	}
}

func (p *pages) static() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

type draft struct {
	Name    string
	Node    string
	Backend string
	Account string
	Kind    string
}

type pageData struct {
	Title    string
	Refresh  bool
	Error    string
	Bots     []BotView
	Bot      BotView
	Nodes    []string
	Accounts []store.Account
	Messages []store.Message
	Peers    []store.Message
	Logs     string
	Draft    draft
}

func (p *pages) render(w http.ResponseWriter, name string, code int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := p.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *pages) fragment(name string, data pageData) (string, error) {
	var buf bytes.Buffer
	if err := p.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func statusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var bad BadRequest
	switch {
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound), errors.Is(err, host.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, host.ErrExists):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (p *pages) index(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bots, err := svc.List(r.Context())
		data := pageData{Title: "Bots", Refresh: true, Bots: bots}
		if err != nil {
			data.Error = err.Error()
		}
		p.render(w, "index", http.StatusOK, data)
	}
}

func (p *pages) newBot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.render(w, "newbot", http.StatusOK, p.formData(r, svc, ""))
	}
}

func (p *pages) formData(r *http.Request, svc *Service, msg string) pageData {
	nodes, err := svc.Nodes(r.Context())
	data := pageData{Title: "New bot", Nodes: nodes, Error: msg}
	if err != nil {
		data.Error = err.Error()
	}
	accounts, err := svc.Store.Accounts()
	if err != nil && data.Error == "" {
		data.Error = err.Error()
	}
	data.Accounts = accounts
	if r.Form != nil {
		data.Draft = draft{
			Name:    r.FormValue("name"),
			Node:    r.FormValue("node"),
			Backend: r.FormValue("backend"),
			Account: r.FormValue("account"),
		}
	}
	return data
}

func (p *pages) createBot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			p.render(w, "newbot", http.StatusBadRequest, p.formData(r, svc, err.Error()))
			return
		}
		_, err := svc.CreateBot(r.Context(), r.FormValue("name"), r.FormValue("node"), r.FormValue("backend"), r.FormValue("account"))
		if err != nil {
			p.render(w, "newbot", statusCode(err), p.formData(r, svc, err.Error()))
			return
		}
		http.Redirect(w, r, "/bots/"+r.FormValue("name"), http.StatusSeeOther)
	}
}

func (p *pages) bot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := p.loadBot(r, svc, r.PathValue("name"), "")
		p.render(w, "bot", statusCode(err), data)
	}
}

func (p *pages) loadBot(r *http.Request, svc *Service, name, msg string) (pageData, error) {
	view, err := svc.Get(r.Context(), name)
	data := pageData{Title: name, Error: msg}
	if err != nil {
		data.Error = err.Error()
		return data, err
	}
	data.Bot = view
	msgs, err := svc.Messages(name)
	if err != nil {
		data.Error = err.Error()
		return data, nil
	}
	data.Messages = msgs
	all, err := svc.Involving(name)
	if err != nil {
		data.Error = err.Error()
		return data, nil
	}
	data.Peers = peersOf(all)
	return data, nil
}

func peersOf(msgs []store.Message) []store.Message {
	var out []store.Message
	for _, m := range msgs {
		if m.Sender != "user" {
			out = append(out, m)
		}
	}
	return out
}

func (p *pages) postMessage(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := r.ParseForm(); err != nil {
			data, loadErr := p.loadBot(r, svc, name, err.Error())
			if loadErr != nil {
				p.render(w, "bot", statusCode(loadErr), data)
				return
			}
			p.render(w, "bot", http.StatusBadRequest, data)
			return
		}
		if _, err := svc.PostUser(name, r.FormValue("body")); err != nil {
			data, loadErr := p.loadBot(r, svc, name, err.Error())
			code := statusCode(err)
			if loadErr != nil {
				code = statusCode(loadErr)
			}
			p.render(w, "bot", code, data)
			return
		}
		http.Redirect(w, r, "/bots/"+name, http.StatusSeeOther)
	}
}

func (p *pages) start(svc *Service) http.HandlerFunc {
	return redirectAction(func(r *http.Request) error {
		return svc.Start(r.Context(), r.PathValue("name"))
	})
}

func (p *pages) stop(svc *Service) http.HandlerFunc {
	return redirectAction(func(r *http.Request) error {
		return svc.Stop(r.Context(), r.PathValue("name"))
	})
}

func (p *pages) delete(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := svc.Delete(r.Context(), r.PathValue("name")); err != nil {
			http.Error(w, err.Error(), statusCode(err))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func redirectAction(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(r); err != nil {
			http.Error(w, err.Error(), statusCode(err))
			return
		}
		http.Redirect(w, r, "/bots/"+r.PathValue("name"), http.StatusSeeOther)
	}
}

func (p *pages) logs(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		view, err := svc.Get(r.Context(), name)
		data := pageData{Title: name + " logs", Refresh: true, Bot: view}
		if err != nil {
			data.Bot = BotView{}
			data.Error = err.Error()
			p.render(w, "logs", statusCode(err), data)
			return
		}
		text, err := svc.Logs(r.Context(), name)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Logs = text
		}
		p.render(w, "logs", http.StatusOK, data)
	}
}

type snapshot struct {
	Phase      string `json:"phase"`
	PhaseClass string `json:"phaseClass"`
	Transcript string `json:"transcript"`
	Peers      string `json:"peers"`
}

func (p *pages) events(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}
		name := r.PathValue("name")
		if _, err := svc.Get(r.Context(), name); err != nil {
			http.Error(w, err.Error(), statusCode(err))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()

		var last string
		send := func() bool {
			data, err := p.loadBot(r, svc, name, "")
			if err != nil || data.Bot.Name == "" {
				return false
			}
			transcript, err := p.fragment("transcript", data)
			if err != nil {
				return false
			}
			peers, err := p.fragment("peers", data)
			if err != nil {
				return false
			}
			raw, err := json.Marshal(snapshot{
				Phase:      data.Bot.Phase,
				PhaseClass: phaseClass(data.Bot.Phase),
				Transcript: transcript,
				Peers:      peers,
			})
			if err != nil {
				return false
			}
			payload := string(raw)
			if payload == last {
				return true
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			last = payload
			return true
		}
		if !send() {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()
			case <-tick.C:
				if !send() {
					return
				}
			}
		}
	}
}

func (p *pages) accounts(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.Store.Accounts()
		data := pageData{Title: "Accounts", Accounts: list}
		if err != nil {
			data.Error = err.Error()
		}
		p.render(w, "accounts", http.StatusOK, data)
	}
}

func (p *pages) createAccount(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			p.render(w, "accounts", http.StatusBadRequest, pageData{Title: "Accounts", Error: err.Error()})
			return
		}
		name := r.FormValue("name")
		kind := r.FormValue("kind")
		secret := strings.ReplaceAll(r.FormValue("secret"), "\r\n", "\n")
		if err := svc.PutAccount(name, kind, secret); err != nil {
			list, _ := svc.Store.Accounts()
			p.render(w, "accounts", statusCode(err), pageData{
				Title:    "Accounts",
				Accounts: list,
				Error:    err.Error(),
				Draft:    draft{Name: name, Kind: kind},
			})
			return
		}
		http.Redirect(w, r, "/accounts", http.StatusSeeOther)
	}
}
