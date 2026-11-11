# Run the bots on your cluster

These commands prove the same path CI proves, plus Kata and a real Claude or Codex login. CI uses kind, Cilium, runc, and the stub CLI. Your cluster is the place that can run Kata and a real credential.

You need `kubectl`, `helm`, `docker` or another builder, a default StorageClass, and Cilium. For VM isolation, the nodes already have a Kata RuntimeClass.

## Build the images

From the repository root:

```bash
docker build -t pvebot-orchestrator:dev -f images/orchestrator/Dockerfile .
docker build -t pvebot-bot:dev -f images/bot/Dockerfile .
```

The bot image runs `botd` and `wget`. It does not contain `claude` or `codex`. To use those CLIs, build a second image that puts both binaries on `PATH`:

```dockerfile
FROM pvebot-bot:dev
USER root
```

Install `claude` and `codex` on `PATH` in that image. Claude authentication is documented at https://code.claude.com/docs/en/authentication . Codex authentication is documented at https://learn.chatgpt.com/docs/auth .

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

The UI shows the same transcript at http://127.0.0.1:8080/bots/a .

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

## Claude and Codex

Create the account in the UI or with curl. The secret is the token from `claude setup-token`, the contents of `~/.codex/auth.json` after `codex login --device-auth`, or an API key.

```bash
curl -s -X POST http://127.0.0.1:8080/api/accounts -H 'content-type: application/json' \
  -d '{"name":"claude-main","kind":"claude","secret":"REPLACE"}'
curl -s -X POST http://127.0.0.1:8080/api/bots -H 'content-type: application/json' \
  -d "{\"name\":\"coder\",\"node\":\"$NODE\",\"backend\":\"claude\",\"account\":\"claude-main\"}"
```

Use the image that contains `claude`. Then post a normal prompt, not a `SEND` line.

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/coder/messages -H 'content-type: application/json' \
  -d '{"body":"Reply with the single word pong."}'
kubectl -n pvebot logs bot-coder-0
curl -s http://127.0.0.1:8080/api/bots/coder/messages
```

Codex is the same flow with `"kind":"codex"` and `"backend":"codex"`. The pod log should show `codex exec` rather than an exec format error. If Codex rejects `--skip-git-repo-check` on your CLI version, that flag is the line to change in `internal/runtime/runtime.go`.

Stop, start, and delete from the bot page, or:

```bash
curl -s -X POST http://127.0.0.1:8080/api/bots/a/stop
curl -s -X POST http://127.0.0.1:8080/api/bots/a/start
curl -s -X DELETE http://127.0.0.1:8080/api/bots/a
curl -s -X DELETE http://127.0.0.1:8080/api/bots/b
```

After stop, `GET /api/bots/a` reports `"phase":"stopped"` and the PVC `workspace-bot-a-0` still exists. After delete, the StatefulSet and the PVC are gone.
