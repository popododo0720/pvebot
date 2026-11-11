# Architecture

pvebot is a small orchestrator for coding bots. Kubernetes is the platform, on bare metal or on any cluster, including one that happens to run on Proxmox. The orchestrator is a single deployment. It serves the HTTP API, the web UI, an encrypted account store, and a message bus. Each bot is a separate pod created through one Go interface, `host.Backend`. The only implementation is the Kubernetes backend.

## Why Go and Helm

The orchestrator and the bot daemon are Go programs built with `CGO_ENABLED=0`. Each image copies in one static binary. `k8s.io/client-go` talks to the API. The UI is HTML from the same process, so a release does not need a second build tool. Helm installs the orchestrator, its ServiceAccount, and the key Secret. Bot pods are not chart resources. The orchestrator creates them after a person uses the UI or the API, which is when the node and the credential are known.

`host.Backend` stays a narrow interface so a later backend can be added without rewriting the bus or the UI. This repository implements Kubernetes only.

## Placement

`Create` builds five objects in the release namespace.

- A Secret with the bot token and, when the backend needs it, the account material.
- A headless Service, required by the StatefulSet.
- A StatefulSet with one replica, a PVC mounted at `/workspace`, and a container that runs `botd`.
- A NetworkPolicy that denies ingress and allows egress only to the orchestrator on TCP 8080 and to DNS in `kube-system`.
- A CiliumNetworkPolicy with the same orchestrator and DNS paths, plus TCP 443 to an FQDN allow list.

`runtimeClassName` is the chart value of the same name. Set it to the Kata class installed on the cluster, commonly `kata`, when the nodes should run each bot in a VM. Leave it empty to use the cluster default runtime. Kind in CI does that, because Kata cannot run there. The integration test fails if a runtime class name is set.

The node chosen in the UI becomes `nodeSelector` `kubernetes.io/hostname`. An empty choice adds no selector. `ListNodes` skips nodes with `NoSchedule` or `NoExecute` taints, so a kind control plane does not show up next to the worker.

`Stop` scales the StatefulSet to zero and keeps the PVC. `Start` scales it to one. `Delete` removes the objects. A missing object is success, so a retry after a partial failure converges.

## Network

Cilium is part of the platform, not an optional addon. The FQDN rules are a CiliumNetworkPolicy `toFQDNs` list. A Kubernetes NetworkPolicy cannot name a host. The default list is `api.anthropic.com`, `github.com`, `api.openai.com`, `auth.openai.com`, and `chatgpt.com`. With at least one policy selecting the pod, everything else is denied, including another bot's pod IP. The kind test checks that denial with `wget` from bot A to bot B's port 8081, and checks that bot A can still read the orchestrator `/healthz`.

## Credentials

`PVEBOT_MASTER_KEY` is 32 bytes of standard base64. The store encrypts each account with AES-GCM before sqlite sees it. Creating a bot decrypts the account in the orchestrator process and copies the bytes into the bot Secret.

- Backend `claude` sets `CLAUDE_CODE_OAUTH_TOKEN` from a token produced by `claude setup-token`.
- Backend `codex` mounts `auth.json` read-only at `/var/run/secrets/codex/auth.json`. On startup the daemon copies it to `$PVEBOT_WORKDIR/.home/.codex/auth.json` mode `0600` and sets the CLI `HOME` to `$PVEBOT_WORKDIR/.home`. `config.toml` sets `cli_auth_credentials_store = "file"` so Codex reads that file. A pod has no keyring.
- Backend `apikey` sets `PVEBOT_API_KEY`. The daemon posts to `{PVEBOT_API_BASE}/v1/chat/completions`, default base `https://api.openai.com`.
- Backend `stub` takes no account. CI uses it.

The bot bearer token is in the same Secret as `PVEBOT_BOT_TOKEN`. The API returns that token once at create time. The HTML pages list account names and do not render secrets.

## Daemon

`botd` long-polls by calling `GET /api/bots/{id}/inbox` with the bearer token. For each message it runs one backend. Each stdout and stderr line is posted to the log endpoint and appended to the transcript while the message is `running`. The same lines are written to the pod stdout. When the run finishes, the daemon posts the result.

The commands are the ones the vendor documents for headless use.

- `claude -p <prompt> --permission-mode bypassPermissions` with `CLAUDE_CODE_OAUTH_TOKEN` set. Print mode is the `-p` flag. `bypassPermissions` lets the headless process call MCP without a person at the keyboard. The flag `--bare` is not passed, because bare mode ignores `CLAUDE_CODE_OAUTH_TOKEN`.
- `codex exec --skip-git-repo-check <prompt>`. `codex exec` is the non-interactive Codex command. The git flag lets a fresh volume run before anyone runs `git init`.
- The API-key path does not shell out. HTTP 401 is the same needs-re-login outcome as a rejected CLI login.

An empty Claude token, a missing Codex `auth.json`, or CLI output that says the login is dead finishes the message as `needs_relogin`. The bot record `authState` becomes `needs_relogin`. A later successful run sets it back to `ok`. A run that does not exit is killed at `RunTimeout` (default 10 minutes, `PVEBOT_RUN_TIMEOUT`) and the message finishes as `error` with reply `backend timed out`. The timeout kills the CLI process group, including the MCP helper it started. When botd itself is root, the CLI runs as uid 1000. The bot image creates that user. Claude Code exits when uid is 0.

The stub command is `botd cli`. A prompt prints `ack:` plus the prompt. A prompt of the form `SEND <agent> <text>` calls `send_to_agent` on the orchestrator. The kind test uses that sentence so the proof does not need a real model.

On startup the daemon writes `/workspace/.mcp.json`, `/workspace/.claude/settings.local.json`, and `$PVEBOT_WORKDIR/.home/.codex/config.toml`. Both MCP configs name a stdio server whose command is `botd mcp-proxy`. The Codex config lists `PVEBOT_ORCHESTRATOR_URL` and `PVEBOT_BOT_TOKEN` in `env_vars`, because Codex starts that process from an allowlist. The Claude settings file turns on the project MCP server. Headless `claude -p` does not show the trust prompt. The proxy posts each JSON-RPC request to `/mcp` with the bot token. An accepted notification (HTTP 202) produces no stdout line. `ping` returns an empty result. Claude Code and Codex can then call the same `send_to_agent` tool the stub calls.

## Bus and MCP

`POST /mcp` implements `initialize`, `tools/list`, and `tools/call` for `send_to_agent`. The tool arguments are `agent` and `text`. The caller is the bot named by the bearer token, not a field in the arguments. The orchestrator enqueues the text on the target bot and waits until that bot's daemon marks the message done. The reply is the tool result.

Two bots therefore talk only through the orchestrator. The network policy is what makes a direct dial fail.

## Web UI

`GET /` lists bots and reloads every three seconds so the phase stays current. `GET /bots/new` is the create form. The node menu is the schedulable nodes from the cluster, and the backend menu is `stub`, `claude`, `codex`, or `apikey`. `GET /accounts` saves a pasted token or `auth.json`. The HTML lists the account name and kind and does not contain the secret. `GET /bots/{name}` shows phase, start, stop, delete, the chat, and bot-to-bot messages. `GET /bots/{name}/events` streams that transcript as server-sent events. Reply text appears as the daemon appends it, and a dead login shows needs re-login on the list and on the bot page. `GET /bots/{name}/logs` is the pod log and reloads on the same three-second timer. There is no login on this process.

## Stages and proof

Stage 1 is pod placement. The criterion is that create, start, stop, status, and delete produce the StatefulSet, the PVC, the node selector, the NetworkPolicy, and the CiliumNetworkPolicy, and that stop then start returns the pod to running. `go test ./internal/host/kubernetes` checks the object bodies against a fake API server. The kind test checks that the same objects exist on a real API server and that the pod reaches Ready.

Stage 2 is the daemon. The criterion is a Claude argv of `claude -p <prompt> --permission-mode bypassPermissions` with `CLAUDE_CODE_OAUTH_TOKEN` in the environment, a Codex argv of `codex exec --skip-git-repo-check <prompt>` with `auth.json` copied mode `0600` into the CLI home, and an API-key request whose bearer token and prompt match the account. A rejected login finishes as `needs_relogin` on the message and on the bot `authState`. A CLI that does not exit finishes as `error` with reply `backend timed out`. Lines from the CLI show up in the transcript before the process exits. `go test ./internal/runtime` runs those commands as stand-in binaries and an httptest API server. The bot image installs the pinned CLIs. CI sets `PVEBOT_BACKEND=stub`.

Stage 3 is messaging and placement. The criterion is that bot A, asked to `SEND b hello`, stores the reply `ack:hello`, that bot B's transcript shows sender `a` and body `hello`, and that A's `wget` to B's pod IP fails while A's `wget` to the orchestrator succeeds. The node selector on both pods equals the node chosen at create time. `TestTwoBotsExchangeThroughMCP` covers the bus without a cluster. `test/integration` covers the bus and the policy on kind.

Stage 4 is the UI. The criterion is that the account form stores a secret the HTML never shows, and that the create form places a bot on the selected node with that secret in the host request. The bot page shows the chat reply. `TestWebFormCreatesBotWithoutLeakingSecret` and the bot page assertion in the bus test cover this.

Stage 5 is the image build. The criterion is that `images/orchestrator/Dockerfile` and `images/bot/Dockerfile` build, and that the bot image contains `botd`, `wget`, and the pinned `claude` and `codex` binaries. CI builds both images. `hack/kind-ci.sh` builds them, loads them into kind, and installs the chart. A Kata runtime class and a real Claude or Codex login stay on the operator's cluster, with the commands in `docs/manual-test.md`.

## Open risks

`claude setup-token` issues a long-lived token for a Claude subscription (Pro, Max, Team, or Enterprise in the authentication docs). The daemon does not refresh it. When the CLI rejects the token, the bot `authState` becomes `needs_relogin` and the message ends. Paste a new token into a new account and create the bot again. The secret is copied into the pod Secret only at create time. Anthropic's subscription terms may limit unattended automation. The API-key backend is the path that uses a console key instead of a subscription. Bare mode in Claude Code ignores `CLAUDE_CODE_OAUTH_TOKEN`, and this daemon does not pass `--bare`.

Codex can refresh `auth.json` only on the writable copy inside the pod. Deleting the pod throws that copy away and the next start uses the Secret again. When the refresh token itself expires, the same `needs_relogin` state is set. Paste a new `auth.json`. The mount stays read-only so a compromised bot process cannot change the Secret.

The UI and the API do not authenticate the human. Put the Service on a network you trust.

Losing the master key makes every stored account unreadable. The key is a Helm value, so it also lands in the Helm release Secret.

CI does not prove that an FQDN allow rule can open `api.anthropic.com`. It proves the CiliumNetworkPolicy object contains the names, and that bot-to-bot traffic is blocked. The manual test does the live fetch.

CI does not install Kata. An empty runtime class is a passing run, not a Kata run.
