package orch

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/popododo0720/pvebot/internal/store"
)

//go:embed templates/*.html static/app.css
var assets embed.FS

type pages struct {
	tmpl *template.Template
}

func newPages() *pages {
	t := template.Must(template.ParseFS(assets, "templates/*.html"))
	return &pages{tmpl: t}
}

func (p *pages) static() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
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
	Logs     string
}

func (p *pages) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := p.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *pages) index(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bots, err := svc.List(r.Context())
		data := pageData{Title: "Bots", Bots: bots}
		if err != nil {
			data.Error = err.Error()
		}
		p.render(w, "index", data)
	}
}

func (p *pages) newBot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.render(w, "newbot", p.formData(r, svc, ""))
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
	return data
}

func (p *pages) createBot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			p.render(w, "newbot", p.formData(r, svc, err.Error()))
			return
		}
		_, err := svc.CreateBot(r.Context(), r.FormValue("name"), r.FormValue("node"), r.FormValue("backend"), r.FormValue("account"))
		if err != nil {
			p.render(w, "newbot", p.formData(r, svc, err.Error()))
			return
		}
		http.Redirect(w, r, "/bots/"+r.FormValue("name"), http.StatusSeeOther)
	}
}

func (p *pages) bot(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.render(w, "bot", p.botData(r, svc, r.PathValue("name"), ""))
	}
}

func (p *pages) botData(r *http.Request, svc *Service, name, msg string) pageData {
	view, err := svc.Get(r.Context(), name)
	data := pageData{Title: name, Refresh: true, Bot: view, Error: msg}
	if err != nil {
		data.Error = err.Error()
		return data
	}
	msgs, err := svc.Messages(name)
	if err != nil && data.Error == "" {
		data.Error = err.Error()
	}
	data.Messages = msgs
	logs, err := svc.Logs(r.Context(), name)
	if err != nil {
		logs = err.Error()
	}
	data.Logs = logs
	return data
}

func (p *pages) postMessage(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := r.ParseForm(); err != nil {
			p.render(w, "bot", p.botData(r, svc, name, err.Error()))
			return
		}
		if _, err := svc.PostUser(name, r.FormValue("body")); err != nil {
			p.render(w, "bot", p.botData(r, svc, name, err.Error()))
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func redirectAction(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/bots/"+r.PathValue("name"), http.StatusSeeOther)
	}
}

func (p *pages) accounts(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.Store.Accounts()
		data := pageData{Title: "Accounts", Accounts: list}
		if err != nil {
			data.Error = err.Error()
		}
		p.render(w, "accounts", data)
	}
}

func (p *pages) createAccount(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			p.render(w, "accounts", pageData{Title: "Accounts", Error: err.Error()})
			return
		}
		secret := r.FormValue("secret")
		secret = strings.ReplaceAll(secret, "\r\n", "\n")
		if err := svc.PutAccount(r.FormValue("name"), r.FormValue("kind"), secret); err != nil {
			list, _ := svc.Store.Accounts()
			p.render(w, "accounts", pageData{Title: "Accounts", Accounts: list, Error: err.Error()})
			return
		}
		http.Redirect(w, r, "/accounts", http.StatusSeeOther)
	}
}
