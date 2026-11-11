package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	const token = "oauth-token-value"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", token)
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HOME/claude-args\"\nprintf '%s' \"$CLAUDE_CODE_OAUTH_TOKEN\" > \"$HOME/claude-token\"\nprintf 'claude:%s' \"$2\"\n")
	writeExe(t, filepath.Join(dir, "codex"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HOME/codex-args\"\nprintf seen > \"$HOME/.codex/seen\"\nprintf 'codex:%s' \"$3\"\n")
	authSrc := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authSrc, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := Config{
		BotID: "a", Token: "t", OrchestratorURL: "http://orch", Backend: "claude",
		Workdir: workdir, Client: http.DefaultClient,
	}
	out, err := Execute(context.Background(), cfg, "fix the tests")
	if err != nil {
		t.Fatal(err)
	}
	if out != "claude:fix the tests" || strings.Contains(out, token) {
		t.Fatalf("claude out = %q", out)
	}
	home := filepath.Join(workdir, ".home")
	args, err := os.ReadFile(filepath.Join(home, "claude-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "-p fix the tests") || !strings.Contains(string(args), "--permission-mode bypassPermissions") {
		t.Fatalf("claude args = %q", args)
	}
	gotToken, err := os.ReadFile(filepath.Join(home, "claude-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotToken) != token {
		t.Fatalf("claude token env = %q", gotToken)
	}
	mcp, err := os.ReadFile(filepath.Join(workdir, ".mcp.json"))
	if err != nil || !strings.Contains(string(mcp), "mcp-proxy") {
		t.Fatalf("claude mcp = %s %v", mcp, err)
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
	authPath := filepath.Join(home, ".codex", "auth.json")
	copied, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != `{"access_token":"x"}` {
		t.Fatalf("copied auth = %s", copied)
	}
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "seen")); err != nil {
		t.Fatalf("codex home: %v", err)
	}
	conf, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "mcp-proxy") || !strings.Contains(string(conf), `cli_auth_credentials_store = "file"`) || !strings.Contains(string(conf), "PVEBOT_BOT_TOKEN") {
		t.Fatalf("codex config = %s", conf)
	}
	codexArgs, err := os.ReadFile(filepath.Join(home, "codex-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codexArgs), "exec --skip-git-repo-check review the diff") {
		t.Fatalf("codex args = %q", codexArgs)
	}
	settings, err := os.ReadFile(filepath.Join(workdir, ".claude", "settings.local.json"))
	if err != nil || !strings.Contains(string(settings), "enableAllProjectMcpServers") || !strings.Contains(string(settings), "mcp__pvebot__send_to_agent") {
		t.Fatalf("claude settings = %s %v", settings, err)
	}
}

func TestProxySkipsAcceptedNotification(t *testing.T) {
	var sawPing bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "ping" {
			sawPing = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"))
			return
		}
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("PVEBOT_ORCHESTRATOR_URL", srv.URL)
	t.Setenv("PVEBOT_BOT_TOKEN", "tok")
	body, err := forwardMCP([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil || len(body) != 0 {
		t.Fatalf("notification body=%q err=%v", body, err)
	}
	body, err = forwardMCP([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil || !bytes.Contains(body, []byte(`"result"`)) || !sawPing {
		t.Fatalf("ping body=%q err=%v saw=%v", body, err, sawPing)
	}
}

func TestOrchestratorAcceptsPingAndNotification(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: &memHost{}}))
	defer srv.Close()
	note, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	note.Body.Close()
	if note.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status %d", note.StatusCode)
	}
	ping, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer ping.Body.Close()
	buf, _ := io.ReadAll(ping.Body)
	if ping.StatusCode != http.StatusOK || !bytes.Contains(buf, []byte(`"result"`)) {
		t.Fatalf("ping status %d body %s", ping.StatusCode, buf)
	}
}

func TestAuthExpiredAndMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-value")
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\necho 'not logged in · run claude setup-token' >&2\nexit 1\n")
	writeExe(t, filepath.Join(dir, "codex"), "#!/bin/sh\necho 'refresh token expired' >&2\nexit 1\n")
	cfg := Config{
		BotID: "a", Token: "t", OrchestratorURL: "http://orch", Backend: "claude",
		Workdir: t.TempDir(), Client: http.DefaultClient,
	}
	_, err := Execute(context.Background(), cfg, "hello")
	if err == nil || !strings.Contains(err.Error(), "needs re-login") || !strings.Contains(err.Error(), "setup-token") {
		t.Fatalf("claude auth err = %v", err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	start := time.Now()
	_, err = Execute(context.Background(), cfg, "hello")
	if err == nil || !strings.Contains(err.Error(), "needs re-login") || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("missing token err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("missing token hung for %s", time.Since(start))
	}
	cfg.Backend = "codex"
	cfg.CodexAuthFile = filepath.Join(dir, "missing-auth.json")
	_, err = Execute(context.Background(), cfg, "hello")
	if err == nil || !strings.Contains(err.Error(), "needs re-login") || !strings.Contains(err.Error(), "auth.json") {
		t.Fatalf("missing auth err = %v", err)
	}
	if err := os.WriteFile(cfg.CodexAuthFile, []byte(`{"refresh_token":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Execute(context.Background(), cfg, "hello")
	if err == nil || !strings.Contains(err.Error(), "needs re-login") || !strings.Contains(err.Error(), "refresh token expired") {
		t.Fatalf("codex auth err = %v", err)
	}
}

func TestBackendTimeout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-value")
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf 'still working\\n'\n/bin/sleep 5\n")
	cfg := Config{
		BotID: "a", Token: "t", OrchestratorURL: "http://orch", Backend: "claude",
		Workdir: t.TempDir(), RunTimeout: 300 * time.Millisecond, Client: http.DefaultClient,
	}
	var lines []string
	cfg.OnOutput = func(chunk string) { lines = append(lines, chunk) }
	start := time.Now()
	_, err := Execute(context.Background(), cfg, "hello")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "backend timed out") {
		t.Fatalf("timeout err = %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout hung for %s", elapsed)
	}
	if !strings.Contains(strings.Join(lines, ""), "still working") {
		t.Fatalf("streamed lines = %q", lines)
	}
}

func TestNeedsReloginSurfacesInAPI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-value")
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\necho 'token expired' >&2\nexit 1\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: &memHost{}, Timeout: 8 * time.Second}))
	defer srv.Close()
	postJSON(t, srv.URL+"/api/accounts", `{"name":"main","kind":"claude","secret":"oauth-token-value"}`)
	token := createAccountedBot(t, srv.URL, "coder", "claude", "main")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, Config{
			BotID: "coder", Token: token, OrchestratorURL: srv.URL, Backend: "claude",
			Workdir: t.TempDir(), Client: srv.Client(),
		})
	}()
	postMessage(t, srv.URL, "coder", "Reply with pong.")
	deadline := time.Now().Add(8 * time.Second)
	var status, reply string
	for time.Now().Before(deadline) {
		msgs := getMessages(t, srv.URL, "coder")
		if len(msgs) == 1 && msgs[0].Status != "running" && msgs[0].Status != "pending" {
			status = msgs[0].Status
			reply = msgs[0].Reply
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if status != "needs_relogin" || !strings.Contains(reply, "needs re-login") {
		t.Fatalf("status=%q reply=%q", status, reply)
	}
	view := getJSON(t, srv.URL+"/api/bots/coder")
	if view["authState"] != "needs_relogin" {
		t.Fatalf("bot = %v", view)
	}
	page := getText(t, srv.URL+"/bots/coder")
	if !strings.Contains(page, "needs re-login") {
		t.Fatalf("bot page missing needs re-login: %s", page)
	}
	index := getText(t, srv.URL+"/")
	if !strings.Contains(index, "needs re-login") {
		t.Fatalf("index missing needs re-login: %s", index)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestClaudeStreamReachesTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-value")
	writeExe(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf 'partial-line\\n'\nwhile [ ! -f \"$HOME/go\" ]; do /bin/sleep 0.05; done\nprintf 'final-line\\n'\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(orch.NewHandler(&orch.Service{Store: st, Host: &memHost{}}))
	defer srv.Close()
	postJSON(t, srv.URL+"/api/accounts", `{"name":"main","kind":"claude","secret":"oauth-token-value"}`)
	token := createAccountedBot(t, srv.URL, "coder", "claude", "main")
	workdir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, Config{
			BotID: "coder", Token: token, OrchestratorURL: srv.URL, Backend: "claude",
			Workdir: workdir, RunTimeout: 15 * time.Second, Client: srv.Client(),
		})
	}()
	postMessage(t, srv.URL, "coder", "stream please")
	deadline := time.Now().Add(8 * time.Second)
	sawPartial := false
	for time.Now().Before(deadline) {
		msgs := getMessages(t, srv.URL, "coder")
		if len(msgs) == 1 && strings.Contains(msgs[0].Reply, "partial-line") && msgs[0].Status == "running" {
			sawPartial = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !sawPartial {
		t.Fatalf("transcript did not stream: %+v", getMessages(t, srv.URL, "coder"))
	}
	if err := os.WriteFile(filepath.Join(workdir, ".home", "go"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	var reply, status string
	for time.Now().Before(deadline) {
		msgs := getMessages(t, srv.URL, "coder")
		if len(msgs) == 1 && msgs[0].Status == "done" {
			reply = msgs[0].Reply
			status = msgs[0].Status
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if status != "done" || !strings.Contains(reply, "partial-line") || !strings.Contains(reply, "final-line") {
		t.Fatalf("final status=%q reply=%q", status, reply)
	}
	cancel()
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
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer denied.Close()
	cfg.APIBase = denied.URL
	cfg.Client = denied.Client()
	_, err = Execute(context.Background(), cfg, "hello api")
	if err == nil || !strings.Contains(err.Error(), "needs re-login") {
		t.Fatalf("api auth err = %v", err)
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

func createAccountedBot(t *testing.T, base, name, backend, account string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"name": name, "backend": backend, "account": account})
	resp := postJSON(t, base+"/api/bots", string(raw))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %s status %d: %s", name, resp.StatusCode, buf)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
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
