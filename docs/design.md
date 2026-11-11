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

Cilium is part of the platform, not an optional addon. The FQDN rules are a CiliumNetworkPolicy `toFQDNs` list. A Kubernetes NetworkPolicy cannot name a host. The default list is `api.anthropic.com`, `github.com`, `api.openai.com`, `auth.openai.com`, and `chatgpt.com`. With at least one policy selecting the pod, everything else is denied, including another bot's pod IP. The kind test checks that denial with `wget` from bot A to bot B's port 8081, and checks that bot A can still read the orchestrator `/healthz`. The same test runs once with Cilium's default tunnel and once with `routingMode=native` on a single node. The bot readiness probe is `wget` to `127.0.0.1:8081`. Under native routing, a kubelet HTTP probe is a packet from the node's `cilium_host` address with identity `world`, and an ingress rule for `host` or the node CIDR does not match it.

## Credentials

`PVEBOT_MASTER_KEY` is 32 bytes of standard base64. The store encrypts each account with AES-GCM before sqlite sees it. Creating a bot decrypts the account in the orchestrator process and copies the bytes into the bot Secret.

- Backend `claude` sets `CLAUDE_CODE_OAUTH_TOKEN` from a token produced by `claude setup-token`.
- Backend `codex` mounts `auth.json` read-only at `/var/run/secrets/codex/auth.json`. On startup the daemon copies it to `~/.codex/auth.json` mode `0600`, which is the file `codex login --device-auth` writes and `codex exec` reads.
- Backend `apikey` sets `PVEBOT_API_KEY`. The daemon posts to `{PVEBOT_API_BASE}/v1/chat/completions`, default base `https://api.openai.com`.
- Backend `stub` takes no account. CI uses it.

The bot bearer token is in the same Secret as `PVEBOT_BOT_TOKEN`. The API returns that token once at create time. The HTML pages list account names and do not render secrets.

## Daemon

`botd` long-polls by calling `GET /api/bots/{id}/inbox` with the bearer token. For each message it runs one backend and posts the stdout to the result endpoint.

The commands are the ones the vendor documents for headless use.

- `claude -p <prompt>` with `CLAUDE_CODE_OAUTH_TOKEN` set. Print mode is the `-p` flag described in the Claude Code headless docs.
- `codex exec --skip-git-repo-check <prompt>`. `codex exec` is the non-interactive Codex command. The git flag lets a fresh volume run before anyone runs `git init`.
- The API-key path does not shell out.

The stub command is `botd cli`. A prompt prints `ack:` plus the prompt. A prompt of the form `SEND <agent> <text>` calls `send_to_agent` on the orchestrator. The kind test uses that sentence so the proof does not need a real model.

On startup the daemon writes `/workspace/.mcp.json` with a stdio server whose command is `botd mcp-proxy`. The proxy posts each JSON-RPC line to `/mcp` with the bot token. Claude Code, started in that workspace, can call the same tool the stub calls.

## Bus and MCP

`POST /mcp` implements `initialize`, `tools/list`, and `tools/call` for `send_to_agent`. The tool arguments are `agent` and `text`. The caller is the bot named by the bearer token, not a field in the arguments. The orchestrator enqueues the text on the target bot and waits until that bot's daemon marks the message done. The reply is the tool result.

Two bots therefore talk only through the orchestrator. The network policy is what makes a direct dial fail.

## Web UI

`GET /` lists bots. `GET /bots/new` is the create form, with a node menu and an account menu. `GET /accounts` saves a pasted token or `auth.json`. `GET /bots/{name}` shows phase, a chat form, the transcript, and the pod log. The bot page refreshes every three seconds so a reply shows up after the daemon finishes. There is no login on this process.

## Stages and proof

Stage 1 is pod placement. The criterion is that create, start, stop, status, and delete produce the StatefulSet, the PVC, the node selector, the NetworkPolicy, and the CiliumNetworkPolicy, and that stop then start returns the pod to running. `go test ./internal/host/kubernetes` checks the object bodies against a fake API server. The kind test checks that the same objects exist on a real API server and that the pod reaches Ready.

Stage 2 is the daemon. The criterion is a Claude argv of `claude -p <prompt>`, a Codex argv of `codex exec --skip-git-repo-check <prompt>` with `auth.json` copied mode `0600`, and an API-key request whose bearer token and prompt match the account. `go test ./internal/runtime` runs those commands as stand-in binaries and an httptest API server. The published bot image contains `botd` only. The operator layers the real `claude` and `codex` binaries on top. CI sets `PVEBOT_BACKEND=stub`.

Stage 3 is messaging and placement. The criterion is that bot A, asked to `SEND b hello`, stores the reply `ack:hello`, that bot B's transcript shows sender `a` and body `hello`, and that A's `wget` to B's pod IP fails while A's `wget` to the orchestrator succeeds. The node selector on both pods equals the node chosen at create time. `TestTwoBotsExchangeThroughMCP` covers the bus without a cluster. `test/integration` covers the bus and the policy on kind.

Stage 4 is the UI. The criterion is that the account form stores a secret the HTML never shows, and that the create form places a bot on the selected node with that secret in the host request. The bot page shows the chat reply. `TestWebFormCreatesBotWithoutLeakingSecret` and the bot page assertion in the bus test cover this.

Stage 5 is the image build. The criterion is that `images/orchestrator/Dockerfile` and `images/bot/Dockerfile` are what the kind job runs. `hack/kind-ci.sh` builds them, loads them into kind, and installs the chart. A Kata runtime class and a real Claude or Codex login stay on the operator's cluster, with the commands in `docs/manual-test.md`.

## Open risks

`claude setup-token` issues a long-lived token for a Claude subscription (Pro, Max, Team, or Enterprise in the authentication docs). The daemon does not refresh it. If the token expires, paste a new one and create the bot again. Anthropic's subscription terms may limit unattended automation. The API-key backend is the path that uses a console key instead of a subscription. Bare mode in Claude Code ignores `CLAUDE_CODE_OAUTH_TOKEN`, and this daemon does not pass `--bare`.

Codex can refresh `auth.json` only on the writable copy inside the pod. Deleting the pod throws that copy away and the next start uses the Secret again. When the refresh token itself expires, paste a new `auth.json`. The mount stays read-only so a compromised bot process cannot change the Secret.

The UI and the API do not authenticate the human. Put the Service on a network you trust.

Losing the master key makes every stored account unreadable. The key is a Helm value, so it also lands in the Helm release Secret.

CI does not prove that an FQDN allow rule can open `api.anthropic.com`. It proves the CiliumNetworkPolicy object contains the names, and that bot-to-bot traffic is blocked. The manual test does the live fetch.

CI does not install Kata. An empty runtime class is a passing run, not a Kata run.
