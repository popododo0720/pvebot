package orch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/popododo0720/pvebot/internal/host"
	"github.com/popododo0720/pvebot/internal/store"
)

type BadRequest struct{ Msg string }

func (e BadRequest) Error() string { return e.Msg }

type Service struct {
	Store   *store.Store
	Host    host.Backend
	Timeout time.Duration
}

type BotView struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Node    string `json:"node"`
	Phase   string `json:"phase"`
	Account string `json:"account"`
}

var nameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

func validName(s string) bool {
	return len(s) >= 1 && len(s) <= 40 && nameRE.MatchString(s)
}

func (s *Service) timeout() time.Duration {
	if s.Timeout <= 0 {
		return 45 * time.Second
	}
	return s.Timeout
}

// CreateBot returns the bot token once.
func (s *Service) CreateBot(ctx context.Context, name, node, backend, account string) (string, error) {
	if !validName(name) {
		return "", BadRequest{Msg: "bot name must be a lowercase DNS label of at most 40 characters"}
	}
	switch backend {
	case "claude", "codex", "apikey", "stub":
	default:
		return "", BadRequest{Msg: "backend must be claude, codex, apikey, or stub"}
	}
	secret := map[string][]byte{}
	if backend == "stub" {
		if account != "" {
			return "", BadRequest{Msg: "the stub backend does not use an account"}
		}
	} else {
		if account == "" {
			return "", BadRequest{Msg: "account is required"}
		}
		kind, plain, err := s.Store.Secret(account)
		if err != nil {
			return "", err
		}
		if kind != backend {
			return "", BadRequest{Msg: fmt.Sprintf("account %s is %s, not %s", account, kind, backend)}
		}
		switch kind {
		case "claude":
			secret[host.KeyClaudeToken] = []byte(plain.Reveal())
		case "codex":
			secret[host.KeyCodexAuth] = []byte(plain.Reveal())
		case "apikey":
			secret[host.KeyAPIKey] = []byte(plain.Reveal())
		default:
			return "", BadRequest{Msg: "unknown account kind " + kind}
		}
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	secret[host.KeyBotToken] = []byte(token)
	if err := s.Store.InsertBot(store.Bot{Name: name, Backend: backend, Node: node, Account: account}, token); err != nil {
		return "", err
	}
	if err := s.Host.Create(ctx, host.Spec{Name: name, Node: node, Backend: backend, Secret: secret}); err != nil {
		_ = s.Store.DeleteBot(name)
		return "", err
	}
	return token, nil
}

func (s *Service) Start(ctx context.Context, name string) error {
	if _, err := s.Store.GetBot(name); err != nil {
		return err
	}
	return s.Host.Start(ctx, name)
}

func (s *Service) Stop(ctx context.Context, name string) error {
	if _, err := s.Store.GetBot(name); err != nil {
		return err
	}
	return s.Host.Stop(ctx, name)
}

func (s *Service) Delete(ctx context.Context, name string) error {
	if err := s.Host.Delete(ctx, name); err != nil && !errors.Is(err, host.ErrNotFound) {
		return err
	}
	return s.Store.DeleteBot(name)
}

func (s *Service) List(ctx context.Context) ([]BotView, error) {
	bots, err := s.Store.Bots()
	if err != nil {
		return nil, err
	}
	out := make([]BotView, 0, len(bots))
	for _, b := range bots {
		out = append(out, s.view(ctx, b))
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, name string) (BotView, error) {
	b, err := s.Store.GetBot(name)
	if err != nil {
		return BotView{}, err
	}
	return s.view(ctx, b), nil
}

func (s *Service) view(ctx context.Context, b store.Bot) BotView {
	v := BotView{Name: b.Name, Backend: b.Backend, Node: b.Node, Phase: "unknown", Account: b.Account}
	st, err := s.Host.Status(ctx, b.Name)
	if err == nil {
		v.Phase = st.Phase
		if st.Node != "" {
			v.Node = st.Node
		}
	}
	return v
}

func (s *Service) Nodes(ctx context.Context) ([]string, error) {
	return s.Host.ListNodes(ctx)
}

func (s *Service) PutAccount(name, kind, secret string) error {
	if !validName(name) {
		return BadRequest{Msg: "account name must be a lowercase DNS label of at most 40 characters"}
	}
	switch kind {
	case "claude", "codex", "apikey":
	default:
		return BadRequest{Msg: "account kind must be claude, codex, or apikey"}
	}
	if strings.TrimSpace(secret) == "" {
		return BadRequest{Msg: "secret is empty"}
	}
	return s.Store.PutAccount(name, kind, secret)
}

func (s *Service) PostUser(name, body string) (int64, error) {
	if _, err := s.Store.GetBot(name); err != nil {
		return 0, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, BadRequest{Msg: "message is empty"}
	}
	return s.Store.Enqueue(name, "user", body)
}

func (s *Service) Messages(name string) ([]store.Message, error) {
	if _, err := s.Store.GetBot(name); err != nil {
		return nil, err
	}
	return s.Store.Messages(name)
}

func (s *Service) Logs(ctx context.Context, name string) (string, error) {
	if _, err := s.Store.GetBot(name); err != nil {
		return "", err
	}
	return s.Host.Logs(ctx, name, 200)
}

func (s *Service) Inbox(token string) (store.Message, bool, error) {
	name, err := s.Store.BotByToken(token)
	if err != nil {
		return store.Message{}, false, err
	}
	return s.Store.Claim(name)
}

func (s *Service) Complete(token string, id int64, reply string, failed bool) error {
	name, err := s.Store.BotByToken(token)
	if err != nil {
		return err
	}
	return s.Store.Complete(name, id, reply, failed)
}

// SendToAgent delivers text to another bot and waits for that bot's reply.
func (s *Service) SendToAgent(ctx context.Context, from, to, text string) (string, error) {
	if !validName(to) {
		return "", BadRequest{Msg: "agent name is invalid"}
	}
	if from == to {
		return "", BadRequest{Msg: "a bot cannot message itself"}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", BadRequest{Msg: "text is empty"}
	}
	if _, err := s.Store.GetBot(to); err != nil {
		return "", err
	}
	id, err := s.Store.Enqueue(to, from, text)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	msg, err := s.Store.Wait(ctx, id)
	if err != nil {
		return "", err
	}
	if msg.Status == store.StatusError {
		return "", errors.New(msg.Reply)
	}
	return msg.Reply, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
