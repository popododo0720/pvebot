# Agent notes

pvebot runs coding bots as Kubernetes pods. One orchestrator deployment owns the API, the UI, the encrypted account store, and the message bus. Each bot is a StatefulSet created through `host.Backend`. The only implementation is `internal/host/kubernetes`.

## Working style

Ship one vertical slice that can be run before adding the next surface. A change is done when a test or a command in `docs/manual-test.md` shows the behavior. Prefer deleting a layer over adding one. Keep commits small enough that each one still builds.

## Layout

- `cmd/orchestrator` is the API and the UI.
- `cmd/botd` is the pod process. `botd cli` is the CI stub. `botd mcp-proxy` forwards MCP to the orchestrator.
- `internal/host` is the placement interface. Kubernetes object construction stays in `internal/host/kubernetes`.
- `internal/store` is sqlite. Account secrets are AES-GCM ciphertext.
- `internal/orch` is HTTP, MCP, and HTML.
- `internal/runtime` runs `claude -p`, `codex exec`, the API-key call, or the stub.
- `charts/pvebot` installs the orchestrator. Bot pods are not in the chart. The orchestrator creates them.
- `images/bot` and `images/orchestrator` are the container images.

## Conventions

Build with `CGO_ENABLED=0`. The orchestrator pod label `app.kubernetes.io/name=pvebot` is also the label in the bot network policy. An empty `runtimeClassName` means the cluster default runtime. CI uses that path because Kata does not run in kind.

Do not log tokens, `auth.json`, or API keys. Do not put secret values in pod env as literals. Use Secret references.

`go test ./...` is the fast check. `bash hack/kind-ci.sh` is the cluster check. It needs Docker.

Comments are for a constraint the next reader cannot see in the code.
