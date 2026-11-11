# pvebot

pvebot is a self-hosted orchestrator for coding bots. Each bot is a Kubernetes pod with its own volume and network policy. The orchestrator stores account secrets encrypted, injects them into Secrets, and carries messages between bots.

The design is in [docs/design.md](docs/design.md). Install on a cluster, including Kata and Cilium, is in [docs/install.md](docs/install.md). Commands that prove a running install are in [docs/manual-test.md](docs/manual-test.md).

```bash
go test ./...
```

The cluster test builds the images and runs two bots on kind with Cilium. It needs Docker.

```bash
bash hack/kind-ci.sh
```
