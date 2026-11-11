package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/pvebot/internal/host"
	"github.com/popododo0720/pvebot/internal/orch"
	"github.com/popododo0720/pvebot/internal/store"
)

func TestCLIAckAndSend(t *testing.T) {
	var gotAgent, gotText, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Params struct {
				Arguments struct {
					Agent string `json:"agent"`
					Text  string `json:"text"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotAgent = req.Params.Arguments.Agent
		gotText = req.Params.Arguments.Text
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "ack:hello"}},
				"isError": false,
			},
		})
	}))
	defer srv.Close()
	t.Setenv("PVEBOT_ORCHESTRATOR_URL", srv.URL)
	t.Setenv("PVEBOT_BOT_TOKEN", "tok")
	var out bytes.Buffer
	if code := CLI(strings.NewReader("ping"), &out); code != 0 || out.String() != "ack:ping" {
		t.Fatalf("ack code=%d out=%q", code, out.String())
	}
	out.Reset()
	if code := CLI(strings.NewReader("SEND b hello"), &out); code != 0 {
		t.Fatalf("send code=%d out=%q", code, out.String())
	}
	if out.String() != "ack:hello" || gotAgent != "b" || gotText != "hello" || gotAuth != "Bearer tok" {
		t.Fatalf("send out=%q agent=%s text=%s auth=%s", out.String(), gotAgent, gotText, gotAuth)
	}
}

func TestClaudeAndCodexCommands(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", dir)
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HOME/claude-args\"\nprintf 'claude:%s' \"$2\"\n")
	writeExe(t, filepath.Join(dir, "codex"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HOME/codex-args\"\nprintf 'codex:%s' \"$3\"\n")
	authSrc := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authSrc, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BotID: "a", Token: "t", OrchestratorURL: "http://orch", Backend: "claude",
		Workdir: t.TempDir(), Client: http.DefaultClient,
	}
	out, err := Execute(context.Background(), cfg, "fix the tests")
	if err != nil {
		t.Fatal(err)
	}
	if out != "claude:fix the tests" {
		t.Fatalf("claude out = %q", out)
	}
	args, _ := os.ReadFile(filepath.Join(home, "claude-args"))
	if !strings.Contains(string(args), "-p fix the tests") {
		t.Fatalf("claude args = %q", args)
	}
	cfg.Backend = "codex"
	cfg.CodexAuthFile = authSrc
	out, err = Execute(context.Background(), cfg, "review the diff")
	if err != nil {
		t.Fatal(err)
	}
	if out != "codex:review the diff" {
		t.Fatalf("codex out = %q", out)
	}
	copied, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != `{"access_token":"x"}` {
		t.Fatalf("copied auth = %s", copied)
	}
	info, err := os.Stat(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %o", info.Mode().Perm())
	}
}

func TestAPIKeyBackend(t *testing.T) {
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		body = string(buf)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": "api-reply"}}},
		})
	}))
	defer srv.Close()
	t.Setenv("PVEBOT_API_KEY", "sk-test")
	cfg := Config{
		BotID: "a", Token: "t", OrchestratorURL: "http://orch", Backend: "apikey",
		Workdir: t.TempDir(), APIBase: srv.URL, APIModel: "test-model", Client: srv.Client(),
	}
	out, err := Execute(context.Background(), cfg, "hello api")
	if err != nil {
		t.Fatal(err)
	}
	if out != "api-reply" || auth != "Bearer sk-test" || !strings.Contains(body, "hello api") || !strings.Contains(body, "test-model") {
		t.Fatalf("out=%q auth=%s body=%s", out, auth, body)
	}
}

func TestTwoBotsExchangeThroughMCP(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := &memHost{}
	svc := &orch.Service{Store: st, Host: mem, Timeout: 8 * time.Second}
	srv := httptest.NewServer(orch.NewHandler(svc))
	defer srv.Close()

	tokenA := createBot(t, srv.URL, "a", "worker-1")
	tokenB := createBot(t, srv.URL, "b", "worker-1")
	if mem.specs["a"].Node != "worker-1" || mem.specs["a"].Backend != "stub" {
		t.Fatalf("placement = %+v", mem.specs["a"])
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 2)
	start := func(id, token string) {
		errc <- Run(ctx, Config{
			BotID: id, Token: token, OrchestratorURL: srv.URL, Backend: "stub",
			Workdir: t.TempDir(), Client: srv.Client(),
		})
	}
	go start("a", tokenA)
	go start("b", tokenB)

	postMessage(t, srv.URL, "a", "SEND b hello")
	deadline := time.Now().Add(8 * time.Second)
	var reply string
	for time.Now().Before(deadline) {
		msgs := getMessages(t, srv.URL, "a")
		if len(msgs) == 1 && msgs[0].Status == "done" {
			reply = msgs[0].Reply
			break
		}
		if len(msgs) == 1 && msgs[0].Status == "error" {
			t.Fatalf("bot a failed: %s", msgs[0].Reply)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reply != "ack:hello" {
		t.Fatalf("reply = %q", reply)
	}
	bmsgs := getMessages(t, srv.URL, "b")
	if len(bmsgs) != 1 || bmsgs[0].Sender != "a" || bmsgs[0].Body != "hello" || bmsgs[0].Reply != "ack:hello" {
		t.Fatalf("b transcript = %+v", bmsgs)
	}
	page := getText(t, srv.URL+"/bots/a")
	if !strings.Contains(page, "ack:hello") || !strings.Contains(page, "SEND b hello") {
		t.Fatalf("bot page missing transcript: %s", page)
	}
	cancel()
}

func TestInboxRejectsBadToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: &memHost{}}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/bots/a/inbox", nil)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWebFormCreatesBotWithoutLeakingSecret(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := &memHost{}
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: mem}))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	const secret = "sk-form-secret-value"
	resp, err := client.PostForm(srv.URL+"/accounts", map[string][]string{
		"name": {"main"}, "kind": {"claude"}, "secret": {secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("account status %d", resp.StatusCode)
	}
	page := getText(t, srv.URL+"/accounts")
	if !strings.Contains(page, "main") || strings.Contains(page, secret) {
		t.Fatalf("accounts page = %s", page)
	}
	resp, err = client.PostForm(srv.URL+"/bots", map[string][]string{
		"name": {"alpha"}, "node": {"worker-1"}, "backend": {"claude"}, "account": {"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/bots/alpha" {
		t.Fatalf("create status %d location %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	page = getText(t, srv.URL+"/bots/alpha")
	if !strings.Contains(page, "worker-1") || !strings.Contains(page, "claude") || strings.Contains(page, secret) {
		t.Fatalf("bot page = %s", page)
	}
	got := string(mem.specs["alpha"].Secret[host.KeyClaudeToken])
	if got != secret {
		t.Fatalf("injected secret = %q", got)
	}
	newPage := getText(t, srv.URL+"/bots/new")
	if !strings.Contains(newPage, "worker-1") || !strings.Contains(newPage, "main") {
		t.Fatalf("new bot form = %s", newPage)
	}
}

func TestDuplicateBotConflicts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: &memHost{}}))
	defer srv.Close()
	createBot(t, srv.URL, "a", "")
	resp := postJSON(t, srv.URL+"/api/bots", `{"name":"a","backend":"stub"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

type memHost struct {
	mu    sync.Mutex
	specs map[string]host.Spec
}

func (m *memHost) Create(_ context.Context, spec host.Spec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs == nil {
		m.specs = map[string]host.Spec{}
	}
	if _, ok := m.specs[spec.Name]; ok {
		return host.ErrExists
	}
	m.specs[spec.Name] = spec
	return nil
}
func (m *memHost) Start(context.Context, string) error { return nil }
func (m *memHost) Stop(context.Context, string) error  { return nil }
func (m *memHost) Status(_ context.Context, name string) (host.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, ok := m.specs[name]
	if !ok {
		return host.Status{}, host.ErrNotFound
	}
	return host.Status{Name: name, Phase: "running", Node: spec.Node}, nil
}
func (m *memHost) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.specs, name)
	return nil
}
func (m *memHost) Logs(context.Context, string, int64) (string, error) { return "log-line", nil }
func (m *memHost) ListNodes(context.Context) ([]string, error) {
	return []string{"worker-1"}, nil
}

func createBot(t *testing.T, base, name, node string) string {
	t.Helper()
	resp := postJSON(t, base+"/api/bots", `{"name":"`+name+`","node":"`+node+`","backend":"stub"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s status %d", name, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("empty token")
	}
	return out.Token
}

func postMessage(t *testing.T, base, bot, body string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"body": body})
	resp := postJSON(t, base+"/api/bots/"+bot+"/messages", string(raw))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("post message status %d", resp.StatusCode)
	}
}

func getMessages(t *testing.T, base, bot string) []store.Message {
	t.Helper()
	resp, err := http.Get(base + "/api/bots/" + bot + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msgs []store.Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func getText(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 3)
	}
	return b
}
