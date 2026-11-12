#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

CLUSTER="${CLUSTER:-pvebot}"
KIND_VERSION="${KIND_VERSION:-v0.27.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.4}"
HELM_VERSION="${HELM_VERSION:-v3.16.4}"
CILIUM_VERSION="${CILIUM_VERSION:-1.16.5}"
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.31.4}"
ROUTING_MODE="${ROUTING_MODE:-tunnel}"
FQDN_EGRESS="${FQDN_EGRESS:-true}"

install_tool() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1
}

if ! install_tool kind; then
  curl -fsSL -o /tmp/kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
  chmod +x /tmp/kind
  sudo mv /tmp/kind /usr/local/bin/kind
fi
if ! install_tool kubectl; then
  curl -fsSL -o /tmp/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
  chmod +x /tmp/kubectl
  sudo mv /tmp/kubectl /usr/local/bin/kubectl
fi
if ! install_tool helm; then
  curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" | tar -xz -C /tmp
  sudo mv /tmp/linux-amd64/helm /usr/local/bin/helm
fi

kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
if [[ "$ROUTING_MODE" == native ]]; then
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
  podSubnet: 10.244.0.0/16
nodes:
  - role: control-plane
EOF
else
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOF
fi

kubectl config use-context "kind-${CLUSTER}"
if [[ "$ROUTING_MODE" == native ]]; then
  kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true
fi
CP="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep control-plane | head -n1)"
HOST="$(kubectl get node "$CP" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
helm repo add cilium https://helm.cilium.io/
helm repo update cilium
CILIUM_ROUTING=()
if [[ "$ROUTING_MODE" == native ]]; then
  CILIUM_ROUTING=(
    --set routingMode=native
    --set autoDirectNodeRoutes=true
    --set ipv4NativeRoutingCIDR=10.244.0.0/16
  )
fi
helm upgrade --install cilium cilium/cilium --version "$CILIUM_VERSION" \
  --namespace kube-system \
  --set operator.replicas=1 \
  --set kubeProxyReplacement=true \
  --set k8sServiceHost="$HOST" \
  --set k8sServicePort=6443 \
  "${CILIUM_ROUTING[@]}" \
  --wait --timeout 6m

kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml
kubectl -n local-path-storage rollout status deploy/local-path-provisioner --timeout=180s
for _ in $(seq 1 30); do
  if kubectl get storageclass local-path >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl get storageclass local-path

docker build -t pvebot-orchestrator:dev -f images/orchestrator/Dockerfile .
docker build -t pvebot-bot:dev -f images/bot/Dockerfile .
kind load docker-image pvebot-orchestrator:dev --name "$CLUSTER"
kind load docker-image pvebot-bot:dev --name "$CLUSTER"

KEY="$(openssl rand -base64 32)"
cat > /tmp/pvebot-values.yaml <<EOF
masterKey: "${KEY}"
image:
  orchestrator: pvebot-orchestrator:dev
  bot: pvebot-bot:dev
  pullPolicy: IfNotPresent
runtimeClassName: ""
EOF

helm upgrade --install pvebot charts/pvebot \
  --namespace pvebot --create-namespace \
  -f /tmp/pvebot-values.yaml \
  --set networkPolicy.fqdnEgress.enabled="${FQDN_EGRESS}" \
  --wait --timeout 180s

kubectl -n pvebot port-forward svc/pvebot 30080:8080 >/tmp/pvebot-pf.log 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" >/dev/null 2>&1 || true' EXIT
for _ in $(seq 1 40); do
  if curl -sf http://127.0.0.1:30080/healthz >/dev/null; then
    break
  fi
  sleep 1
done
curl -sf http://127.0.0.1:30080/healthz >/dev/null

PVEBOT_URL=http://127.0.0.1:30080 PVEBOT_FQDN_EGRESS="${FQDN_EGRESS}" go test -tags=integration -count=1 -timeout 20m ./test/integration
