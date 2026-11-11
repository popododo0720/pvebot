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
	"time"
)

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
	if err := writeMCP(cfg); err != nil {
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
		reply, runErr := Execute(ctx, cfg, msg.Body)
		failed := runErr != nil
		if failed {
			reply = runErr.Error()
		}
		if err := postResult(ctx, cfg, msg.ID, reply, failed); err != nil && ctx.Err() != nil {
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

func postResult(ctx context.Context, cfg Config, id int64, reply string, failed bool) error {
	payload, _ := json.Marshal(map[string]any{"body": reply, "failed": failed})
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

func Execute(ctx context.Context, cfg Config, prompt string) (string, error) {
	if err := cfg.normalize(); err != nil {
		return "", err
	}
	switch cfg.Backend {
	case "stub":
		return runCommand(ctx, cfg, prompt, stubArgs()...)
	case "claude":
		return runCommand(ctx, cfg, prompt, "claude", "-p", prompt)
	case "codex":
		if err := copyCodexAuth(cfg); err != nil {
			return "", err
		}
		return runCommand(ctx, cfg, prompt, "codex", "exec", "--skip-git-repo-check", prompt)
	case "apikey":
		return callAPI(ctx, cfg, prompt)
	default:
		return "", fmt.Errorf("unknown backend %q", cfg.Backend)
	}
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
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

func commandEnv(cfg Config) []string {
	env := os.Environ()
	env = append(env,
		"PVEBOT_ORCHESTRATOR_URL="+cfg.OrchestratorURL,
		"PVEBOT_BOT_TOKEN="+cfg.Token,
		"PVEBOT_BOT_ID="+cfg.BotID,
		"PVEBOT_WORKDIR="+cfg.Workdir,
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
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/root"
	}
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "auth.json"), b, 0o600)
}

func writeMCP(cfg Config) error {
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
	return os.WriteFile(filepath.Join(cfg.Workdir, ".mcp.json"), append(raw, '\n'), 0o644)
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
