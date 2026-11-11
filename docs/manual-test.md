# Run the bots on your cluster

These commands prove the same path CI proves, plus Kata and a real Claude or Codex login. CI uses kind, Cilium, runc, and the stub CLI. Your cluster is the place that can run Kata and a real credential.

You need `kubectl`, `helm`, `docker` or another builder, a default StorageClass, and Cilium. For VM isolation, the nodes already have a Kata RuntimeClass.

## Build the images

From the repository root:

```bash
docker build -t pvebot-orchestrator:dev -f images/orchestrator/Dockerfile .
docker build -t pvebot-bot:dev -f images/bot/Dockerfile .
```

The bot image runs `botd` and `wget`. It also installs `claude` and `codex`. The versions are the `CLAUDE_CODE_VERSION` and `CODEX_VERSION` build arguments in `images/bot/Dockerfile`.

Push both images somewhere the nodes can pull, or load them the way your cluster expects. The commands below use the tag `pvebot-bot:dev` and `pvebot-orchestrator:dev` with `IfNotPresent`.

## Install

```bash
kubectl get runtimeclass
openssl rand -base64 32
```

Put the runtime class name in the file below. Use `kata` only when `kubectl get runtimeclass` lists `kata`. Leave `runtimeClassName` empty to use the node default, which is what CI does.

```bash
cat > /tmp/pvebot-values.yaml <<'EOF'
masterKey: "REPLACE_WITH_OPENSSL_OUTPUT"
image:
  orchestrator: pvebot-orchestrator:dev
  bot: pvebot-bot:dev
  pullPolicy: IfNotPresent
runtimeClassName: "kata"
storageSize: 1Gi
EOF
helm upgrade --install pvebot charts/pvebot \
  --namespace pvebot --create-namespace \
  -f /tmp/pvebot-values.yaml \
  --wait --timeout 180s
kubectl -n pvebot port-forward svc/pvebot 8080:8080
```

Open http://127.0.0.1:8080 . The Bots page is empty. Accounts is the form for credentials.

## Stub path, no vendor login

This is the CI scenario, repeated by hand.

```bash
curl -s http://127.0.0.1:8080/api/nodes
NODE=$(curl -s http://127.0.0.1:8080/api/nodes | python3 -c 'import json,sys; print(json.load(sys.stdin)["nodes"][0])')
curl -s -X POST http://127.0.0.1:8080/api/bots -H 'content-type: application/json' \
  -d "{\"name\":\"a\",\"node\":\"$NODE\",\"backend\":\"stub\"}"
curl -s -X POST http://127.0.0.1:8080/api/bots -H 'content-type: application/json' \
  -d "{\"name\":\"b\",\"node\":\"$NODE\",\"backend\":\"stub\"}"
```

Wait until both pods are Ready.

```bash
kubectl -n pvebot rollout status statefulset/bot-a --timeout=180s
kubectl -n pvebot rollout status statefulset/bot-b --timeout=180s
kubectl -n pvebot get pod bot-a-0 -o jsonpath='{.spec.runtimeClassName}{"\n"}{.spec.nodeSelector}{"\n"}'
```

With `runtimeClassName: "kata"` in the values file, the first line is `kata`. The second line contains the node you picked.

Send a message and read the reply.

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/a/messages -H 'content-type: application/json' \
  -d '{"body":"SEND b hello"}'
sleep 5
curl -s http://127.0.0.1:8080/api/bots/a/messages
```

The last message has `"reply":"ack:hello"`. Bot B's transcript has sender `a` and body `hello`.

The UI shows the same transcript at http://127.0.0.1:8080/bots/a . The page streams the reply when the daemon posts it. Pod logs are at http://127.0.0.1:8080/bots/a/logs .

## Network policy

```bash
IP=$(kubectl -n pvebot get pod -l pvebot.io/bot=b -o jsonpath='{.items[0].status.podIP}')
kubectl -n pvebot exec bot-a-0 -- wget -q -T 3 -O - "http://${IP}:8081/healthz"
```

That `wget` fails. The next one prints `ok`.

```bash
kubectl -n pvebot exec bot-a-0 -- wget -q -T 5 -O - http://pvebot.pvebot.svc:8080/healthz
kubectl -n pvebot get networkpolicy bot-a
kubectl -n pvebot get ciliumnetworkpolicy bot-a -o yaml
```

The Cilium policy lists the FQDN allow list. From the bot, an allowed host answers and other pods do not.

```bash
kubectl -n pvebot exec bot-a-0 -- wget -q -T 10 -O - https://api.anthropic.com/
```

A TLS or HTTP response means the FQDN rule allowed the dial. A timeout means the policy or the cluster DNS path is wrong.

## Log in to Claude

On a machine with a browser, run `claude setup-token`. The command prints a token. Paste that token as the account secret. The bot image already contains `claude`.

```bash
curl -s -X POST http://127.0.0.1:8080/api/accounts -H 'content-type: application/json' \
  -d '{"name":"claude-main","kind":"claude","secret":"REPLACE"}'
curl -s -X POST http://127.0.0.1:8080/api/bots -H 'content-type: application/json' \
  -d "{\"name\":\"coder\",\"node\":\"$NODE\",\"backend\":\"claude\",\"account\":\"claude-main\"}"
kubectl -n pvebot rollout status statefulset/bot-coder --timeout=180s
```

Post a normal prompt, not a `SEND` line.

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/coder/messages -H 'content-type: application/json' \
  -d '{"body":"Reply with the single word pong."}'
```

Poll the transcript until `"status"` is `done`. The `"reply"` is the model text. While `"status"` is `running`, `"reply"` grows with each line from the CLI. The same lines are in the pod log.

```bash
kubectl -n pvebot logs bot-coder-0
curl -s http://127.0.0.1:8080/api/bots/coder/messages
```

The bot page at http://127.0.0.1:8080/bots/coder shows the same transcript.

## Log in to Codex

On a machine with a browser, run `codex login --device-auth`. Paste the contents of `~/.codex/auth.json` as the account secret. The bot image already contains `codex`.

Use the Claude commands above with `"kind":"codex"`, `"backend":"codex"`, and a different bot name. The daemon runs `codex exec --skip-git-repo-check`. The pod log shows the CLI output, not an exec format error.

## See a dead login

Create a Claude account whose secret is not a token from `claude setup-token`. Create a bot on that account and post a prompt.

```bash
curl -s -X POST http://127.0.0.1:8080/api/accounts -H 'content-type: application/json' \
  -d '{"name":"claude-bad","kind":"claude","secret":"not-a-token"}'
curl -s -X POST http://127.0.0.1:8080/api/bots -H 'content-type: application/json' \
  -d "{\"name\":\"stale\",\"node\":\"$NODE\",\"backend\":\"claude\",\"account\":\"claude-bad\"}"
kubectl -n pvebot rollout status statefulset/bot-stale --timeout=180s
curl -s -X POST http://127.0.0.1:8080/api/bots/stale/messages -H 'content-type: application/json' \
  -d '{"body":"Reply with the single word pong."}'
sleep 15
curl -s http://127.0.0.1:8080/api/bots/stale
curl -s http://127.0.0.1:8080/api/bots/stale/messages
```

`GET /api/bots/stale` has `"authState":"needs_relogin"`. The message `"status"` is `needs_relogin`, not `running`. The bot page and the bot list show **needs re-login**.

A run that never exits becomes `"status":"error"` with `"reply":"backend timed out"`. The daemon waits 10 minutes (`PVEBOT_RUN_TIMEOUT`).

The account secret is copied into the pod when the bot is created. To replace a dead token, create a new account with a fresh `claude setup-token` or `auth.json`, delete the bot, and create it again.

## Ask Claude to message another bot

Create the stub bot `b` from the stub section, and a Claude bot `a` with a real token. Post this prompt to `a`:

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/a/messages -H 'content-type: application/json' \
  -d '{"body":"Use the pvebot MCP tool send_to_agent. The agent is b. The text is hello from claude."}'
```

Bot `b` transcript has sender `a` and body `hello from claude`. Bot `a` reply includes `b`'s `ack:` answer. The CLI sees that tool because the daemon writes `/workspace/.mcp.json` for Claude and `config.toml` for Codex. The tool command is `botd mcp-proxy`.

Stop, start, and delete from the bot page, or:

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/a/stop
curl -s -X POST http://127.0.0.1:8080/api/bots/a/start
curl -s -X DELETE http://127.0.0.1:8080/api/bots/a
curl -s -X DELETE http://127.0.0.1:8080/api/bots/b
```

After stop, `GET /api/bots/a` reports `"phase":"stopped"` and the PVC `workspace-bot-a-0` still exists. After delete, the StatefulSet and the PVC are gone.
