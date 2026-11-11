// Package runtime is the bot daemon. It pulls jobs from the orchestrator and
// runs Claude Code, Codex, an API-key backend, or the CI stub.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	statusDone         = "done"
	statusError        = "error"
	statusNeedsRelogin = "needs_relogin"

	cliUID = 1000
	cliGID = 1000
)

type runFailure struct {
	status string
	detail string
}

func (f *runFailure) Error() string { return f.detail }

func relogin(detail string) error {
	return &runFailure{status: statusNeedsRelogin, detail: "needs re-login: " + detail}
}

type Config struct {
	BotID           string
	Token           string
	OrchestratorURL string
	Backend         string
	Workdir         string
	APIBase         string
	APIModel        string
	CodexAuthFile   string
	Listen          string
	RunTimeout      time.Duration
	OnOutput        func(string)
	Client          *http.Client
}

// Run polls the inbox until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.normalize(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return err
	}
	if err := ensureMCP(cfg); err != nil {
		return err
	}
	if err := copyCodexAuth(cfg); err != nil {
		return err
	}
	if cfg.Listen != "" {
		go serveHealth(ctx, cfg.Listen)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, ok, err := poll(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sleep(ctx, 500*time.Millisecond)
			continue
		}
		if !ok {
			sleep(ctx, 200*time.Millisecond)
			continue
		}
		cfg.OnOutput = func(chunk string) {
			fmt.Fprint(os.Stdout, chunk)
			_ = postLog(ctx, cfg, msg.ID, chunk)
		}
		reply, runErr := Execute(ctx, cfg, msg.Body)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status := statusDone
		if runErr != nil {
			status = statusError
			var fail *runFailure
			if errors.As(runErr, &fail) {
				status = fail.status
			}
			reply = redact(runErr.Error())
		}
		if err := postResult(ctx, cfg, msg.ID, reply, status); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (c *Config) normalize() error {
	if c.BotID == "" || c.Token == "" || c.OrchestratorURL == "" || c.Backend == "" {
		return errors.New("bot id, token, orchestrator URL, and backend are required")
	}
	c.OrchestratorURL = strings.TrimRight(c.OrchestratorURL, "/")
	if c.Workdir == "" {
		c.Workdir = "/workspace"
	}
	if c.APIBase == "" {
		c.APIBase = os.Getenv("PVEBOT_API_BASE")
	}
	if c.APIBase == "" {
		c.APIBase = "https://api.openai.com"
	}
	if c.APIModel == "" {
		c.APIModel = os.Getenv("PVEBOT_API_MODEL")
	}
	if c.APIModel == "" {
		c.APIModel = "gpt-4o-mini"
	}
	if c.CodexAuthFile == "" {
		c.CodexAuthFile = "/var/run/secrets/codex/auth.json"
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = 10 * time.Minute
	}
	return nil
}

type inboxMessage struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

func poll(ctx context.Context, cfg Config) (inboxMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.OrchestratorURL+"/api/bots/"+cfg.BotID+"/inbox", nil)
	if err != nil {
		return inboxMessage{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return inboxMessage{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return inboxMessage{}, false, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return inboxMessage{}, false, fmt.Errorf("inbox status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var msg inboxMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return inboxMessage{}, false, err
	}
	return msg, true, nil
}

func postResult(ctx context.Context, cfg Config, id int64, reply, status string) error {
	payload, _ := json.Marshal(map[string]any{
		"body":   reply,
		"failed": status != statusDone,
		"status": status,
	})
	url := fmt.Sprintf("%s/api/bots/%s/inbox/%d/result", cfg.OrchestratorURL, cfg.BotID, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("result status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

func postLog(ctx context.Context, cfg Config, id int64, chunk string) error {
	payload, _ := json.Marshal(map[string]string{"chunk": chunk})
	url := fmt.Sprintf("%s/api/bots/%s/inbox/%d/log", cfg.OrchestratorURL, cfg.BotID, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("log status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

func Execute(ctx context.Context, cfg Config, prompt string) (string, error) {
	if err := cfg.normalize(); err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.RunTimeout)
	defer cancel()
	text, err := runBackend(runCtx, cfg, prompt)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", &runFailure{status: statusError, detail: "backend timed out"}
	}
	return text, err
}

func runBackend(ctx context.Context, cfg Config, prompt string) (string, error) {
	switch cfg.Backend {
	case "stub":
		return runCommand(ctx, cfg, prompt, stubArgs()...)
	case "claude":
		if strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")) == "" {
			return "", relogin("CLAUDE_CODE_OAUTH_TOKEN is empty")
		}
		if err := ensureMCP(cfg); err != nil {
			return "", err
		}
		return runCommand(ctx, cfg, prompt, "claude", "-p", prompt, "--permission-mode", "bypassPermissions")
	case "codex":
		if err := requireCodexAuth(cfg); err != nil {
			return "", err
		}
		if err := ensureMCP(cfg); err != nil {
			return "", err
		}
		return runCommand(ctx, cfg, prompt, "codex", "exec", "--skip-git-repo-check", prompt)
	case "apikey":
		return callAPI(ctx, cfg, prompt)
	default:
		return "", fmt.Errorf("unknown backend %q", cfg.Backend)
	}
}

func requireCodexAuth(cfg Config) error {
	if _, err := os.Stat(cfg.CodexAuthFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return relogin("codex auth.json is missing")
		}
		return err
	}
	return copyCodexAuth(cfg)
}

func stubArgs() []string {
	exe, err := os.Executable()
	if err != nil {
		return []string{"botd", "cli"}
	}
	return []string{exe, "cli"}
}

func runCommand(ctx context.Context, cfg Config, prompt string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = cfg.Workdir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = commandEnv(cfg)
	cmd.SysProcAttr = cliSysProc()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%s: %s", filepath.Base(args[0]), err.Error())
	}
	var outBuf, errBuf strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go scanOutput(&wg, stdout, &outBuf, cfg)
	go scanOutput(&wg, stderr, &errBuf, cfg)
	wg.Wait()
	err = cmd.Wait()
	out := redact(strings.TrimSpace(outBuf.String()))
	detail := redact(strings.TrimSpace(errBuf.String()))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", &runFailure{status: statusError, detail: "backend timed out"}
	}
	if err != nil {
		combined := out
		if detail != "" {
			if combined != "" {
				combined += "\n"
			}
			combined += detail
		}
		if authFailed(combined) {
			return "", relogin(firstText(detail, out, err.Error()))
		}
		if detail == "" {
			detail = err.Error()
		}
		if out != "" {
			detail = out + ": " + detail
		}
		return "", fmt.Errorf("%s: %s", filepath.Base(args[0]), detail)
	}
	return out, nil
}

func scanOutput(wg *sync.WaitGroup, r io.Reader, buf *strings.Builder, cfg Config) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := redact(sc.Text() + "\n")
		buf.WriteString(line)
		if cfg.OnOutput != nil {
			cfg.OnOutput(line)
		}
	}
}

func firstText(parts ...string) string {
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			return p
		}
	}
	return "authentication failed"
}

func authFailed(text string) bool {
	low := strings.ToLower(text)
	for _, phrase := range []string{
		"not logged in",
		"please log in",
		"please run /login",
		"setup-token",
		"codex login",
		"invalid api key",
		"invalid authentication",
		"authentication_error",
		"authentication failed",
		"token has expired",
		"token expired",
		"refresh token",
		"oauth token",
		"login required",
		"re-login",
		"relogin",
	} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}

func redact(s string) string {
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "PVEBOT_BOT_TOKEN", "PVEBOT_API_KEY"} {
		if v := os.Getenv(key); len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func commandEnv(cfg Config) []string {
	env := os.Environ()
	home, err := cliHome(cfg)
	if err != nil {
		home = cfg.Workdir
	}
	env = append(env,
		"PVEBOT_ORCHESTRATOR_URL="+cfg.OrchestratorURL,
		"PVEBOT_BOT_TOKEN="+cfg.Token,
		"PVEBOT_BOT_ID="+cfg.BotID,
		"PVEBOT_WORKDIR="+cfg.Workdir,
		"HOME="+home,
	)
	return env
}

func callAPI(ctx context.Context, cfg Config, prompt string) (string, error) {
	key := os.Getenv("PVEBOT_API_KEY")
	if key == "" {
		return "", errors.New("PVEBOT_API_KEY is empty")
	}
	payload, _ := json.Marshal(map[string]any{
		"model": cfg.APIModel,
		"messages": []map[string]string{{
			"role":    "user",
			"content": prompt,
		}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.APIBase, "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", relogin("api rejected the key")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("api returned no choices")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

func copyCodexAuth(cfg Config) error {
	b, err := os.ReadFile(cfg.CodexAuthFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	home, err := cliHome(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := chownCLI(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return chownCLI(path)
}

func cliHome(cfg Config) (string, error) {
	dir := filepath.Join(cfg.Workdir, ".home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if os.Getuid() == 0 {
		if err := os.Chmod(cfg.Workdir, 0o755); err != nil {
			return "", err
		}
		if err := os.Chown(dir, cliUID, cliGID); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func chownCLI(path string) error {
	if os.Getuid() != 0 {
		return nil
	}
	return os.Chown(path, cliUID, cliGID)
}

// cliSysProc drops the CLI to uid 1000 when botd is root. Claude Code exits on uid 0.
// Setpgid lets the run timeout kill the MCP helper the CLI spawned.
func cliSysProc() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Getuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: cliUID, Gid: cliGID}
	}
	return attr
}

func ensureMCP(cfg Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	doc := map[string]any{
		"mcpServers": map[string]any{
			"pvebot": map[string]any{
				"command": exe,
				"args":    []string{"mcp-proxy"},
			},
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Workdir, ".mcp.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	home, err := cliHome(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := chownCLI(dir); err != nil {
		return err
	}
	// "auto" uses the keyring first. A pod has none, so Codex would ignore auth.json.
	body := fmt.Sprintf("cli_auth_credentials_store = \"file\"\n\n[mcp_servers.pvebot]\ncommand = %q\nargs = [\"mcp-proxy\"]\n", exe)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	return chownCLI(path)
}

func serveHealth(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func CLI(in io.Reader, out io.Writer) int {
	body, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		fmt.Fprintln(out, err.Error())
		return 1
	}
	prompt := strings.TrimSpace(string(body))
	const prefix = "SEND "
	if strings.HasPrefix(prompt, prefix) {
		rest := strings.TrimPrefix(prompt, prefix)
		agent, text, ok := strings.Cut(rest, " ")
		if !ok || agent == "" || strings.TrimSpace(text) == "" {
			fmt.Fprintln(out, "usage: SEND <agent> <text>")
			return 1
		}
		reply, err := callSend(os.Getenv("PVEBOT_ORCHESTRATOR_URL"), os.Getenv("PVEBOT_BOT_TOKEN"), agent, text)
		if err != nil {
			fmt.Fprintln(out, err.Error())
			return 1
		}
		fmt.Fprint(out, reply)
		return 0
	}
	fmt.Fprintf(out, "ack:%s", prompt)
	return 0
}

func callSend(orch, token, agent, text string) (string, error) {
	if orch == "" || token == "" {
		return "", errors.New("missing orchestrator URL or bot token")
	}
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "send_to_agent",
			"arguments": map[string]string{"agent": agent, "text": text},
		},
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(orch, "/")+"/mcp", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mcp status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", errors.New(parsed.Error.Message)
	}
	if parsed.Result.IsError {
		if len(parsed.Result.Content) > 0 {
			return "", errors.New(parsed.Result.Content[0].Text)
		}
		return "", errors.New("send_to_agent failed")
	}
	if len(parsed.Result.Content) == 0 {
		return "", errors.New("empty mcp result")
	}
	return parsed.Result.Content[0].Text, nil
}

// Proxy forwards newline-delimited MCP JSON-RPC to the orchestrator.
func Proxy() int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, err := forwardMCP(line)
		if err != nil {
			fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"error\":{\"code\":-32000,\"message\":%q}}\n", err.Error())
			continue
		}
		if len(resp) == 0 || resp[len(resp)-1] != '\n' {
			resp = append(resp, '\n')
		}
		_, _ = os.Stdout.Write(resp)
	}
	return 0
}

func forwardMCP(line []byte) ([]byte, error) {
	orch := strings.TrimRight(os.Getenv("PVEBOT_ORCHESTRATOR_URL"), "/")
	token := os.Getenv("PVEBOT_BOT_TOKEN")
	if orch == "" || token == "" {
		return nil, errors.New("missing orchestrator URL or bot token")
	}
	req, err := http.NewRequest(http.MethodPost, orch+"/mcp", bytes.NewReader(line))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}
