# Install pvebot on a cluster

Install the orchestrator with Helm, then create bots from the web UI. The chart installs the orchestrator only. Bot pods are created later, when you use the UI or the API.

This guide is for a cluster you administer. That includes Kubernetes on Proxmox VMs. CI uses kind, Cilium, and an empty runtime class. A Kata runtime class belongs on the cluster you run, not in CI.

You need `kubectl`, `helm`, a container builder, a default StorageClass, and Cilium. pvebot creates a `CiliumNetworkPolicy` for each bot. Install fails when the Cilium CRDs are missing.

The UI has no login. Reach it with `kubectl port-forward` or another private path. Do not put a public `LoadBalancer` in front of it unless something else authenticates the caller.

## Create node VMs on Proxmox

Kata starts a virtual machine for each bot pod. The Kubernetes node has to be a VM with nested virtualization. An LXC container cannot provide that hypervisor.

On the Proxmox host, check nested virtualization. On AMD, read `/sys/module/kvm_amd/parameters/nested` instead.

```bash
cat /sys/module/kvm_intel/parameters/nested
```

The file contains `Y` or `1` when nesting is already on. Leave the host alone in that case. When it contains `N` or `0`, write the module option and reboot the host.

```bash
echo 'options kvm_intel nested=1' | sudo tee /etc/modprobe.d/kvm-intel.conf
sudo reboot
```

On AMD the option is `options kvm_amd nested=1` in `/etc/modprobe.d/kvm-amd.conf`. Read the parameter again after reboot.

Create one VM per Kubernetes node. Set the VM CPU type to `host`. Use a virtio disk and a virtio NIC. Size the worker for the cluster pods plus one Kata guest for each bot that runs on it.

Install the Kubernetes distribution you already operate. kubeadm, k3s, and RKE2 are fine when they let you replace the CNI with Cilium and add a RuntimeClass.

## Install Cilium

Each bot has two policy objects.

The Kubernetes `NetworkPolicy` allows egress to the orchestrator pod on TCP 8080 and to DNS in `kube-system` on port 53. Ingress is denied.

The `CiliumNetworkPolicy` allows those same paths and, by default, TCP 443 to the names in `egressFQDNs`. The orchestrator match uses the label `app.kubernetes.io/name=pvebot`. The chart sets that label on the orchestrator pod. DNS is matched as `k8s-app=kube-dns` in `kube-system`. The default FQDN rules use `toFQDNs`, which needs Cilium's DNS proxy. Cilium turns that proxy on for a policy that uses `toFQDNs`.

CI installs Cilium 1.16.5 with `kubeProxyReplacement=true` because kind in `hack/kind-ci.sh` has no kube-proxy. On a cluster that still runs kube-proxy, install Cilium without copying the kind `k8sServiceHost` settings.

```bash
helm repo add cilium https://helm.cilium.io/
helm repo update cilium
helm upgrade --install cilium cilium/cilium \
  --namespace kube-system \
  --set kubeProxyReplacement=false \
  --wait --timeout 6m
```

If the cluster has no kube-proxy, set `kubeProxyReplacement=true` and set `k8sServiceHost` and `k8sServicePort` to the API server address, the way `hack/kind-ci.sh` does.

Confirm the CRD and the DNS label before you install pvebot.

```bash
kubectl get crd ciliumnetworkpolicies.cilium.io
kubectl -n kube-system get pods -l k8s-app=kube-dns
```

The second command must list the DNS pods. If it lists nothing, the bot policy cannot name your DNS and name lookup from a bot fails.

## Disable FQDN egress

Some node kernels are built without the netfilter targets Cilium's DNS proxy needs. cilium-agent then logs `cannot install static proxy rules` and `-j CT --notrack`, or `Extension CT revision 0 not supported`. Bots cannot resolve the orchestrator Service, and a message stays pending. A direct IP connection to the orchestrator still works.

The `-j CT --notrack` rule needs `CONFIG_NETFILTER_XT_TARGET_CT`. The proxy also needs `CONFIG_NETFILTER_XT_TARGET_TPROXY` and `CONFIG_NETFILTER_XT_MATCH_SOCKET`. `--notrack` is an option of the CT target. It is not a separate `CONFIG_NETFILTER_XT_TARGET_NOTRACK` option.

On a node, read the kernel config.

```bash
zcat /proc/config.gz | grep -E 'CONFIG_NETFILTER_XT_TARGET_CT|CONFIG_NETFILTER_XT_TARGET_TPROXY|CONFIG_NETFILTER_XT_MATCH_SOCKET'
```

If `/proc/config.gz` is missing, read `/boot/config-$(uname -r)` with the same grep. Each line should end in `=y` or `=m`. A line that says `is not set`, or a name that the grep does not print, means the proxy cannot install.

Read the agent log from a machine with `kubectl`.

```bash
kubectl -n kube-system logs -l k8s-app=cilium --tail=200 | grep -E 'CT --notrack|Extension CT revision 0 not supported'
```

Any match means the same thing. Upgrade the release and keep the existing values, including `masterKey`.

```bash
helm upgrade pvebot charts/pvebot \
  --namespace pvebot \
  --reuse-values \
  --set networkPolicy.fqdnEgress.enabled=false \
  --wait --timeout 180s
```

You can also set the key in the values file.

```yaml
networkPolicy:
  fqdnEgress:
    enabled: false
```

The generated CiliumNetworkPolicy then allows the orchestrator on TCP 8080 and kube-dns on UDP 53 and TCP 53. It has no `rules.dns` entry and no `toFQDNs` entry. Names in `egressFQDNs` are not installed. A bot cannot dial the public internet in this setting. The chart does not add a CIDR allow list to replace those names. Bot-to-bot traffic stays denied. The Kubernetes NetworkPolicy is unchanged.

Bots created before the upgrade keep the old policy. Delete those bots and create them again.

## Install Kata and choose a RuntimeClass

Install Kata with the kata-deploy chart. The install steps and the class names live in the [Kata installation guide](https://kata-containers.github.io/kata-containers/installation/) and the [RuntimeClass guide](https://kata-containers.github.io/kata-containers/kata-deploy-runtimeclasses/). Pin the chart version to the Kata release you want.

```bash
export VERSION=$(curl -sSL https://api.github.com/repos/kata-containers/kata-containers/releases/latest | jq -r .tag_name)
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --version "${VERSION}"
kubectl get runtimeclasses
```

Copy a name from that list into the pvebot value `runtimeClassName`. A default kata-deploy install includes `kata-qemu-runtime-rs` for general use. Use that name when the list shows it. Use another name from the list when you need a different hypervisor. The class `kata` exists only when the Kata chart was installed with the default class enabled. Do not type `kata` unless `kubectl get runtimeclasses` prints it.

`runtimeClassName` is applied to bot pods. The orchestrator deployment stays on the cluster default runtime.

Confirm Kata with a pod before you point pvebot at the class. In the manifest below, replace `kata-qemu-runtime-rs` with the class you put in the pvebot values. The kernel in the log differs from `uname -r` on the node when the pod ran under Kata.

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: kata-check
spec:
  runtimeClassName: kata-qemu-runtime-rs
  containers:
    - name: test
      image: quay.io/libpod/ubuntu:latest
      command: ["uname", "-r"]
EOF
kubectl logs kata-check
kubectl delete pod kata-check
```

If the pod stays `ContainerCreating` and the event says the runtime cannot start a VM, nested virtualization is off or the VM CPU type is not `host`.

Leave `runtimeClassName` empty to use the node default runtime. That is a valid install. It is the CI setting, and it does not run Kata.

## Install the chart

Build the images from the repository root and make them pullable by the nodes. The bot image contains `botd`. It does not contain `claude` or `codex`. Layer those binaries into a second image when you want those backends. The stub backend works with the image below.

```bash
docker build -t pvebot-orchestrator:dev -f images/orchestrator/Dockerfile .
docker build -t pvebot-bot:dev -f images/bot/Dockerfile .
openssl rand -base64 32
```

The `openssl` line prints `masterKey`. It is 32 bytes of standard base64. The orchestrator refuses to start without it. Losing the key makes every stored account unreadable. Helm keeps the value in the release Secret as well as in the chart Secret.

Write the values file. Delete the file after install. It contains the key.

```bash
cat > /tmp/pvebot-values.yaml <<'EOF'
masterKey: "REPLACE_WITH_OPENSSL_OUTPUT"
image:
  orchestrator: pvebot-orchestrator:dev
  bot: pvebot-bot:dev
  pullPolicy: IfNotPresent
runtimeClassName: "kata-qemu-runtime-rs"
storageSize: 1Gi
EOF
helm upgrade --install pvebot charts/pvebot \
  --namespace pvebot --create-namespace \
  -f /tmp/pvebot-values.yaml \
  --wait --timeout 180s
rm /tmp/pvebot-values.yaml
```

Use the RuntimeClass name you confirmed above. Use `runtimeClassName: ""` when you are not running Kata.

The chart creates a namespaced Role for pods, Services, Secrets, PVCs, StatefulSets, NetworkPolicies, and CiliumNetworkPolicies. It also creates a ClusterRole that can list nodes. The node menu in the UI is empty when that list fails.

```bash
kubectl -n pvebot port-forward svc/pvebot 8080:8080
```

Open http://127.0.0.1:8080 .

The Bots page lists bots and reloads so the phase stays current. **New bot** asks for a name, a schedulable node, and a backend. **Accounts** stores a Claude setup token, a Codex `auth.json`, or an API key. The page shows the name and the kind. It does not show the secret. Open a bot for the chat, the bot-to-bot messages, start, stop, and delete. **Logs** on that bot is the pod log.

After you create a bot, confirm the runtime class.

```bash
kubectl -n pvebot get pod -l pvebot.io/bot=NAME -o jsonpath='{.items[0].spec.runtimeClassName}{"\n"}'
```

An empty line means the pod uses the cluster default runtime.

The curl drill for two stub bots, the network check, and a real Claude or Codex login is in [docs/manual-test.md](manual-test.md).

## Helm values

These are the keys in `charts/pvebot/values.yaml`. Defaults are the file contents.

`image.orchestrator` is the orchestrator image. Default `pvebot-orchestrator:dev`.

`image.bot` is the image for bot pods. Default `pvebot-bot:dev`.

`image.pullPolicy` is the pull policy for both images. Default `IfNotPresent`.

`service.type` is the Service type. Default `ClusterIP`.

`service.port` is the Service port. Default `8080`. The container listens on 8080. Bot network policy allows TCP 8080 to the orchestrator pod.

`runtimeClassName` is the RuntimeClass for bot pods. Default `""`, which omits the field and uses the cluster default runtime.

`storageClass` is the StorageClass for the orchestrator data volume and for bot workspace volumes. Default `""`, which uses the cluster default StorageClass.

`storageSize` is the bot workspace volume size. Default `1Gi`. The orchestrator data volume is 1Gi in the chart template and does not follow this key.

`masterKey` is required. It is 32 bytes of standard base64, passed to the orchestrator as `PVEBOT_MASTER_KEY`.

`egressFQDNs` is the list of names a bot may dial on TCP 443 when `networkPolicy.fqdnEgress.enabled` is true. The default is `api.anthropic.com`, `github.com`, `api.openai.com`, `auth.openai.com`, and `chatgpt.com`. An empty list omits that rule. Bots can still reach the orchestrator and DNS. When FQDN egress is disabled, the list is ignored.

`networkPolicy.fqdnEgress.enabled` defaults to true. True installs `toFQDNs` and the Cilium DNS proxy rule. False allows only L3/L4 egress to kube-dns and the orchestrator. The kernel check is under Disable FQDN egress.
