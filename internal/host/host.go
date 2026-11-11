// Package host is the bot placement boundary. The orchestrator creates,
// starts, stops, and deletes bot hosts through Backend.
package host

import (
	"context"
	"errors"
)

const (
	OrchestratorAppLabel = "app.kubernetes.io/name"
	OrchestratorAppValue = "pvebot"
	BotLabel             = "pvebot.io/bot"

	KeyBotToken    = "PVEBOT_BOT_TOKEN"
	KeyClaudeToken = "CLAUDE_CODE_OAUTH_TOKEN"
	KeyAPIKey      = "PVEBOT_API_KEY"
	KeyCodexAuth   = "auth.json"
)

// ErrExists means a bot host with that name is already present.
var ErrExists = errors.New("already exists")

// ErrNotFound means the bot host is absent.
var ErrNotFound = errors.New("not found")

type Spec struct {
	Name    string
	Node    string
	Backend string
	Secret  map[string][]byte
}

type Status struct {
	Name  string
	Phase string
	Node  string
}

// Backend owns bot hosts. Implementations must make Delete succeed when the
// host is already gone, and Create fail with ErrExists when it is already there.
type Backend interface {
	Create(ctx context.Context, spec Spec) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (Status, error)
	Delete(ctx context.Context, name string) error
	Logs(ctx context.Context, name string, tail int64) (string, error)
	ListNodes(ctx context.Context) ([]string, error)
}
