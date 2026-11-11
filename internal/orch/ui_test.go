package orch

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/pvebot/internal/host"
	"github.com/popododo0720/pvebot/internal/store"
)

func TestIndexShowsBotsAndRefreshes(t *testing.T) {
	_, srv, svc, h := newTestServer(t, nil)
	code, page := getResp(t, srv.URL+"/")
	if code != http.StatusOK || !strings.Contains(page, "No bots yet") || !strings.Contains(page, `http-equiv="refresh"`) {
		t.Fatalf("empty index %d %s", code, page)
	}
	mustBot(t, svc, "alpha", "worker-1", "stub", "")
	h.setPhase("stopped")
	_, page = getResp(t, srv.URL+"/")
	for _, want := range []string{"alpha", "phase-stopped", "stub", "worker-1", `http-equiv="refresh"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("index missing %s\n%s", want, page)
		}
	}
	h.setPhase("running")
	_, page = getResp(t, srv.URL+"/")
	if !strings.Contains(page, "phase-running") || strings.Contains(page, "phase-stopped") {
		t.Fatalf("running index %s", page)
	}
}

func TestNewBotFormOffersNodesAndBackends(t *testing.T) {
	_, srv, _, _ := newTestServer(t, newFake("worker-a", "worker-b"))
	page := getBody(t, srv.URL+"/bots/new")
	for _, want := range []string{"worker-a", "worker-b", "Any node", ">stub<", ">claude<", ">codex<", ">apikey<", "Create bot"} {
		if !strings.Contains(page, want) {
			t.Fatalf("form missing %s\n%s", want, page)
		}
	}
	_, empty, _, _ := newTestServer(t, &fakeHost{})
	page = getBody(t, empty.URL+"/bots/new")
	if !strings.Contains(page, "no schedulable nodes") || !strings.Contains(page, "Any node") {
		t.Fatalf("empty nodes form %s", page)
	}
}

func TestCreateBotFromForm(t *testing.T) {
	_, srv, _, h := newTestServer(t, nil)
	resp := postForm(t, noFollow(), srv.URL+"/bots", map[string]string{
		"name": "alpha", "node": "worker-1", "backend": "stub", "account": "",
	})
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/bots/alpha" {
		t.Fatalf("create %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	spec := h.spec("alpha")
	if spec.Node != "worker-1" || spec.Backend != "stub" {
		t.Fatalf("spec %+v", spec)
	}
	page := getBody(t, srv.URL+"/bots/alpha")
	if !strings.Contains(page, "alpha") || !strings.Contains(page, "worker-1") || !strings.Contains(page, "stub") {
		t.Fatal(page)
	}
	token := string(spec.Secret[host.KeyBotToken])
	if token == "" || strings.Contains(page, token) {
		t.Fatal("bot token rendered on the bot page")
	}
}

func TestCreateBotFormShowsValidationError(t *testing.T) {
	_, srv, _, _ := newTestServer(t, nil)
	resp := postForm(t, noFollow(), srv.URL+"/bots", map[string]string{
		"name": "Bad", "node": "worker-1", "backend": "claude", "account": "",
	})
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{"lowercase", `value="Bad"`, `value="claude" selected`, `value="worker-1" selected`} {
		if !strings.Contains(body, want) {
			t.Fatalf("error form missing %s\n%s", want, body)
		}
	}
}

func TestBotPageShowsChatPeersAndActions(t *testing.T) {
	st, srv, svc, _ := newTestServer(t, nil)
	mustBot(t, svc, "a", "worker-1", "stub", "")
	mustBot(t, svc, "b", "worker-1", "stub", "")
	if _, err := st.Enqueue("a", "user", "hello-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue("a", "b", "from-b-to-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue("b", "a", "from-a-to-b"); err != nil {
		t.Fatal(err)
	}
	code, page := getResp(t, srv.URL+"/bots/a")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		"hello-user", "from-b-to-a", "from-a-to-b", "Bot to bot",
		`action="/bots/a/start"`, `action="/bots/a/stop"`, `action="/bots/a/delete"`,
		`href="/bots/a/logs"`, `/static/chat.js`, "Start", "Stop", "Delete",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("bot page missing %s\n%s", want, page)
		}
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Fatal("chat page reloads and would wipe the composer")
	}
	peers := between(t, page, `id="peers"`, `</section>`)
	if strings.Contains(peers, "hello-user") || !strings.Contains(peers, "from-a-to-b") || !strings.Contains(peers, `class="peer"`) {
		t.Fatalf("peers %s", peers)
	}
	script := getBody(t, srv.URL+"/static/chat.js")
	if !strings.Contains(script, "EventSource") {
		t.Fatal(script)
	}
	missing, _ := getResp(t, srv.URL+"/bots/missing")
	if missing != http.StatusNotFound {
		t.Fatalf("missing bot %d", missing)
	}
}

func TestPostMessageFromChat(t *testing.T) {
	_, srv, svc, _ := newTestServer(t, nil)
	mustBot(t, svc, "alpha", "worker-1", "stub", "")
	client := noFollow()
	resp := postForm(t, client, srv.URL+"/bots/alpha/message", map[string]string{"body": "hello-from-form"})
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/bots/alpha" {
		t.Fatalf("post %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	page := getBody(t, srv.URL+"/bots/alpha")
	if !strings.Contains(page, "hello-from-form") {
		t.Fatal(page)
	}
	resp = postForm(t, client, srv.URL+"/bots/alpha/message", map[string]string{"body": "   "})
	body = readClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "message is empty") {
		t.Fatalf("empty %d %s", resp.StatusCode, body)
	}
}

func TestStartStopDeleteFromBotPage(t *testing.T) {
	_, srv, svc, h := newTestServer(t, nil)
	mustBot(t, svc, "alpha", "worker-1", "stub", "")
	client := noFollow()
	for _, action := range []string{"start", "stop"} {
		resp := postForm(t, client, srv.URL+"/bots/alpha/"+action, nil)
		body := readClose(t, resp)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/bots/alpha" {
			t.Fatalf("%s %d %s %s", action, resp.StatusCode, resp.Header.Get("Location"), body)
		}
	}
	started, stopped, _ := h.names()
	if len(started) != 1 || started[0] != "alpha" || len(stopped) != 1 || stopped[0] != "alpha" {
		t.Fatalf("start %v stop %v", started, stopped)
	}
	resp := postForm(t, client, srv.URL+"/bots/alpha/delete", nil)
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("delete %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	page := getBody(t, srv.URL+"/")
	if strings.Contains(page, "alpha") || !strings.Contains(page, "No bots yet") {
		t.Fatal(page)
	}
	resp = postForm(t, client, srv.URL+"/bots/missing/start", nil)
	readClose(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing start %d", resp.StatusCode)
	}
}

func TestNeedsReloginRendersOnListAndChat(t *testing.T) {
	st, srv, svc, _ := newTestServer(t, nil)
	mustBot(t, svc, "alpha", "worker-1", "stub", "")
	id, err := st.Enqueue("alpha", "user", "ping")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Claim("alpha"); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := st.Complete("alpha", id, "needs re-login: token expired", store.StatusNeedsRelogin); err != nil {
		t.Fatal(err)
	}
	page := getBody(t, srv.URL+"/bots/alpha")
	index := getBody(t, srv.URL+"/")
	if !strings.Contains(page, "needs re-login") || !strings.Contains(index, "needs re-login") {
		t.Fatalf("page %s\nindex %s", page, index)
	}
}

func TestLogsPageShowsPodLog(t *testing.T) {
	_, srv, svc, h := newTestServer(t, nil)
	mustBot(t, svc, "alpha", "worker-1", "stub", "")
	h.setLogs("kernel line from kata")
	code, page := getResp(t, srv.URL+"/bots/alpha/logs")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"kernel line from kata", `http-equiv="refresh"`, `href="/bots/alpha"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("logs missing %s\n%s", want, page)
		}
	}
	missing, _ := getResp(t, srv.URL+"/bots/missing/logs")
	if missing != http.StatusNotFound {
		t.Fatalf("missing logs %d", missing)
	}
}

func TestChatEventsStreamReply(t *testing.T) {
	st, srv, svc, _ := newTestServer(t, nil)
	mustBot(t, svc, "a", "worker-1", "stub", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/bots/a/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	first := parseSnap(t, nextData(t, br))
	if first.Phase != "running" || strings.Contains(first.Transcript, "pong-from-daemon") {
		t.Fatalf("first %#v", first)
	}
	if !strings.Contains(first.Peers, "No bot-to-bot messages") {
		t.Fatal(first.Peers)
	}
	id, err := st.Enqueue("a", "user", "hello-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Claim("a"); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := st.Complete("a", id, "pong-from-daemon", store.StatusDone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue("a", "b", "from-b-to-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue("b", "a", "from-a-to-b"); err != nil {
		t.Fatal(err)
	}
	next := parseSnap(t, nextData(t, br))
	if !strings.Contains(next.Transcript, "hello-user") || !strings.Contains(next.Transcript, "pong-from-daemon") {
		t.Fatalf("transcript %s", next.Transcript)
	}
	if strings.Contains(next.Peers, "hello-user") || !strings.Contains(next.Peers, "from-b-to-a") || !strings.Contains(next.Peers, "from-a-to-b") {
		t.Fatalf("peers %s", next.Peers)
	}
}

func TestPagesNeverRenderSecrets(t *testing.T) {
	const marker = "NEVER-RENDER-7f3c"
	secret := marker + "-claude-token"
	_, srv, _, h := newTestServer(t, nil)
	client := noFollow()

	resp := postForm(t, client, srv.URL+"/accounts", map[string]string{
		"name": "Bad", "kind": "claude", "secret": secret,
	})
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "lowercase") {
		t.Fatalf("bad account %d %s", resp.StatusCode, body)
	}
	assertNotContains(t, "account error", body, marker)
	if strings.Contains(resp.Header.Get("Location"), marker) {
		t.Fatal("redirect carries the secret")
	}

	resp = postForm(t, client, srv.URL+"/accounts", map[string]string{
		"name": "main", "kind": "claude", "secret": secret,
	})
	body = readClose(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/accounts" {
		t.Fatalf("save account %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	assertNotContains(t, "account redirect", body+" "+resp.Header.Get("Location"), marker)

	resp = postForm(t, client, srv.URL+"/bots", map[string]string{
		"name": "alpha", "node": "worker-1", "backend": "claude", "account": "main",
	})
	body = readClose(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create %d %s", resp.StatusCode, body)
	}
	assertNotContains(t, "create redirect", body, marker)
	token := string(h.spec("alpha").Secret[host.KeyBotToken])
	if token == "" || string(h.spec("alpha").Secret[host.KeyClaudeToken]) != secret {
		t.Fatal("account secret was not injected into the host")
	}

	paths := []string{
		"/", "/bots/new", "/bots/alpha", "/bots/alpha/logs", "/accounts",
		"/api/accounts", "/api/bots", "/api/bots/alpha", "/api/bots/alpha/logs", "/api/bots/alpha/messages",
		"/static/app.css", "/static/chat.js",
	}
	for _, path := range paths {
		code, page := getResp(t, srv.URL+path)
		if code != http.StatusOK {
			t.Fatalf("%s %d %s", path, code, page)
		}
		assertNotContains(t, path, page, marker)
		assertNotContains(t, path, page, token)
	}
	accounts := getBody(t, srv.URL+"/accounts")
	if !strings.Contains(accounts, "<strong>main</strong>") || !strings.Contains(accounts, "claude") {
		t.Fatalf("accounts page %s", accounts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/bots/alpha/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	raw := nextData(t, bufio.NewReader(stream.Body))
	assertNotContains(t, "events", raw, marker)
	assertNotContains(t, "events", raw, token)
	missing, _ := getResp(t, srv.URL+"/bots/missing/events")
	if missing != http.StatusNotFound {
		t.Fatalf("missing events %d", missing)
	}
}

func newTestServer(t *testing.T, h *fakeHost) (*store.Store, *httptest.Server, *Service, *fakeHost) {
	t.Helper()
	if h == nil {
		h = newFake()
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "pvebot.db"), bytes32())
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: st, Host: h}
	srv := httptest.NewServer(NewHandler(svc))
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return st, srv, svc, h
}

func newFake(nodes ...string) *fakeHost {
	if len(nodes) == 0 {
		nodes = []string{"worker-1"}
	}
	return &fakeHost{nodes: nodes, logs: "log-line"}
}

func mustBot(t *testing.T, svc *Service, name, node, backend, account string) {
	t.Helper()
	if _, err := svc.CreateBot(context.Background(), name, node, backend, account); err != nil {
		t.Fatal(err)
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func noFollow() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func postForm(t *testing.T, client *http.Client, rawURL string, form map[string]string) *http.Response {
	t.Helper()
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	resp, err := client.PostForm(rawURL, vals)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getResp(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	body := readClose(t, resp)
	return resp.StatusCode, body
}

func getBody(t *testing.T, rawURL string) string {
	t.Helper()
	_, body := getResp(t, rawURL)
	return body
}

func readClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("missing %q", start)
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		t.Fatalf("missing %q", end)
	}
	return s[:j]
}

func nextData(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	for {
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if strings.HasSuffix(b.String(), "\n\n") || err != nil {
				if err != nil {
					t.Fatalf("event read: %v %q", err, b.String())
				}
				break
			}
		}
		ev := b.String()
		if strings.HasPrefix(strings.TrimSpace(ev), "data:") {
			return ev
		}
	}
}

func parseSnap(t *testing.T, event string) snapshot {
	t.Helper()
	line := strings.TrimSpace(event)
	line = strings.TrimPrefix(line, "data:")
	line = strings.TrimSpace(line)
	var snap snapshot
	if err := json.Unmarshal([]byte(line), &snap); err != nil {
		t.Fatalf("snapshot: %v %s", err, event)
	}
	return snap
}

func assertNotContains(t *testing.T, where, body, needle string) {
	t.Helper()
	if needle != "" && strings.Contains(body, needle) {
		t.Fatalf("%s contains secret material", where)
	}
}

type fakeHost struct {
	mu      sync.Mutex
	specs   map[string]host.Spec
	nodes   []string
	logs    string
	phase   string
	started []string
	stopped []string
	deleted []string
}

func (m *fakeHost) Create(_ context.Context, spec host.Spec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs == nil {
		m.specs = map[string]host.Spec{}
	}
	if _, ok := m.specs[spec.Name]; ok {
		return host.ErrExists
	}
	cp := spec
	cp.Secret = map[string][]byte{}
	for k, v := range spec.Secret {
		cp.Secret[k] = append([]byte(nil), v...)
	}
	m.specs[spec.Name] = cp
	return nil
}

func (m *fakeHost) Start(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, name)
	return nil
}

func (m *fakeHost) Stop(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, name)
	return nil
}

func (m *fakeHost) Status(_ context.Context, name string) (host.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, ok := m.specs[name]
	if !ok {
		return host.Status{}, host.ErrNotFound
	}
	phase := m.phase
	if phase == "" {
		phase = "running"
	}
	return host.Status{Name: name, Phase: phase, Node: spec.Node}, nil
}

func (m *fakeHost) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.specs, name)
	m.deleted = append(m.deleted, name)
	return nil
}

func (m *fakeHost) Logs(context.Context, string, int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logs == "" {
		return "log-line", nil
	}
	return m.logs, nil
}

func (m *fakeHost) ListNodes(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.nodes...), nil
}

func (m *fakeHost) setPhase(phase string) {
	m.mu.Lock()
	m.phase = phase
	m.mu.Unlock()
}

func (m *fakeHost) setLogs(text string) {
	m.mu.Lock()
	m.logs = text
	m.mu.Unlock()
}

func (m *fakeHost) spec(name string) host.Spec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.specs[name]
}

func (m *fakeHost) names() (started, stopped, deleted []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.started...), append([]string(nil), m.stopped...), append([]string(nil), m.deleted...)
}
