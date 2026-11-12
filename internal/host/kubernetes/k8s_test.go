package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/popododo0720/pvebot/internal/host"
)

func testClient(t *testing.T, cfg Config) (*Client, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	kube := fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		CiliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	c, err := New(cfg, kube, dyn)
	if err != nil {
		t.Fatal(err)
	}
	return c, kube, dyn
}

func baseCfg() Config {
	return Config{
		Namespace:       "pvebot",
		BotImage:        "pvebot-bot:dev",
		OrchestratorURL: "http://pvebot.pvebot.svc:8080",
		RuntimeClass:    "kata",
		StorageSize:     "1Gi",
		EgressFQDNs:     []string{"api.anthropic.com", "github.com"},
	}
}

func TestCreateKataPodPolicyAndSecretRef(t *testing.T) {
	c, kube, dyn := testClient(t, baseCfg())
	ctx := context.Background()
	spec := host.Spec{
		Name:    "alpha",
		Node:    "worker-1",
		Backend: "claude",
		Secret: map[string][]byte{
			host.KeyBotToken:    []byte("bot-token"),
			host.KeyClaudeToken: []byte("oauth-secret"),
		},
	}
	if err := c.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, spec); !errors.Is(err, host.ErrExists) {
		t.Fatalf("duplicate create err = %v", err)
	}

	sts, err := kube.AppsV1().StatefulSets("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := sts.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata" {
		t.Fatalf("runtime class = %v", pod.RuntimeClassName)
	}
	probe := pod.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet != nil || probe.Exec == nil {
		t.Fatalf("readiness = %#v, want loopback exec", probe)
	}
	if got := strings.Join(probe.Exec.Command, " "); !strings.Contains(got, "http://127.0.0.1:8081/healthz") {
		t.Fatalf("readiness command = %q", got)
	}
	if pod.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Fatalf("nodeSelector = %v", pod.NodeSelector)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("claims = %d", len(sts.Spec.VolumeClaimTemplates))
	}
	q := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if q.String() != "1Gi" {
		t.Fatalf("storage = %s", q.String())
	}
	raw, _ := json.Marshal(sts)
	if strings.Contains(string(raw), "oauth-secret") || strings.Contains(string(raw), "bot-token") {
		t.Fatal("pod spec contains secret material")
	}
	var tokenRef, claudeRef bool
	for _, env := range pod.Containers[0].Env {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := env.ValueFrom.SecretKeyRef
		if env.Name == "PVEBOT_BOT_TOKEN" && ref.Name == "bot-alpha" && ref.Key == host.KeyBotToken {
			tokenRef = true
		}
		if env.Name == "CLAUDE_CODE_OAUTH_TOKEN" && ref.Key == host.KeyClaudeToken {
			claudeRef = true
		}
	}
	if !tokenRef || !claudeRef {
		t.Fatalf("env = %#v", pod.Containers[0].Env)
	}

	np, err := kube.NetworkingV1().NetworkPolicies("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Fatalf("ingress rules = %d, want none", len(np.Spec.Ingress))
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[0].Ports[0].Port.IntVal != 8080 {
		t.Fatalf("egress = %#v", np.Spec.Egress)
	}

	cnp, err := dyn.Resource(CiliumNetworkPolicyGVR).Namespace("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(cnp.Object)
	if !strings.Contains(string(body), `"matchName":"api.anthropic.com"`) || !strings.Contains(string(body), `"matchName":"github.com"`) {
		t.Fatalf("cilium policy = %s", body)
	}

	sec, err := kube.CoreV1().Secrets("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[host.KeyClaudeToken]) != "oauth-secret" {
		t.Fatal("secret missing claude token")
	}
}

func TestCiliumPolicyEgressModes(t *testing.T) {
	fqdn, _ := loadCiliumPolicy(t, baseCfg())
	if len(fqdn.Spec.Ingress) != 0 {
		t.Fatalf("ingress = %d, want none", len(fqdn.Spec.Ingress))
	}
	if len(fqdn.Spec.Egress) != 3 {
		t.Fatalf("fqdn egress rules = %d, want orchestrator, dns proxy, toFQDNs", len(fqdn.Spec.Egress))
	}
	if !hasPort(fqdn, "8080", "TCP") {
		t.Fatalf("fqdn policy missing orchestrator: %#v", fqdn.Spec.Egress)
	}
	if !hasDNSProxy(fqdn) {
		t.Fatalf("fqdn policy missing dns proxy rule: %#v", fqdn.Spec.Egress)
	}
	names := fqdnNames(fqdn)
	if strings.Join(names, ",") != "api.anthropic.com,github.com" {
		t.Fatalf("toFQDNs = %v", names)
	}

	empty := baseCfg()
	empty.EgressFQDNs = nil
	noNames, _ := loadCiliumPolicy(t, empty)
	if len(noNames.Spec.Egress) != 2 || !hasDNSProxy(noNames) || len(fqdnNames(noNames)) != 0 {
		t.Fatalf("empty allow list = %#v", noNames.Spec.Egress)
	}

	l4cfg := baseCfg()
	l4cfg.DNSEgress = DNSEgressL4
	l4, raw := loadCiliumPolicy(t, l4cfg)
	body := string(raw)
	if strings.Contains(body, "toFQDNs") || strings.Contains(body, `"dns"`) || strings.Contains(body, "matchPattern") {
		t.Fatalf("L4 policy still has L7 DNS or toFQDNs: %s", body)
	}
	if len(l4.Spec.Ingress) != 0 {
		t.Fatalf("L4 ingress = %d, want none", len(l4.Spec.Ingress))
	}
	if len(l4.Spec.Egress) != 2 {
		t.Fatalf("L4 egress rules = %d, want orchestrator and kube-dns", len(l4.Spec.Egress))
	}
	if !hasPort(l4, "8080", "TCP") || !hasPort(l4, "53", "UDP") || !hasPort(l4, "53", "TCP") {
		t.Fatalf("L4 ports = %#v", l4.Spec.Egress)
	}
	if !bytes.Equal(networkPolicySpec(t, baseCfg()), networkPolicySpec(t, l4cfg)) {
		t.Fatal("L4 mode changed the Kubernetes NetworkPolicy")
	}
}

func loadCiliumPolicy(t *testing.T, cfg Config) (ciliumPolicyDoc, []byte) {
	t.Helper()
	c, _, dyn := testClient(t, cfg)
	ctx := context.Background()
	if err := c.Create(ctx, host.Spec{
		Name: "alpha", Backend: "stub", Secret: map[string][]byte{host.KeyBotToken: []byte("t")},
	}); err != nil {
		t.Fatal(err)
	}
	obj, err := dyn.Resource(CiliumNetworkPolicyGVR).Namespace("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		t.Fatal(err)
	}
	var doc ciliumPolicyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, raw
}

func networkPolicySpec(t *testing.T, cfg Config) []byte {
	t.Helper()
	c, kube, _ := testClient(t, cfg)
	ctx := context.Background()
	if err := c.Create(ctx, host.Spec{
		Name: "alpha", Backend: "stub", Secret: map[string][]byte{host.KeyBotToken: []byte("t")},
	}); err != nil {
		t.Fatal(err)
	}
	np, err := kube.NetworkingV1().NetworkPolicies("pvebot").Get(ctx, "bot-alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(np.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type ciliumPolicyDoc struct {
	Spec struct {
		Ingress []json.RawMessage `json:"ingress"`
		Egress  []ciliumEgress    `json:"egress"`
	} `json:"spec"`
}

type ciliumEgress struct {
	ToFQDNs []struct {
		MatchName string `json:"matchName"`
	} `json:"toFQDNs"`
	ToPorts []struct {
		Ports []struct {
			Port     string `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"ports"`
		Rules struct {
			DNS []struct {
				MatchPattern string `json:"matchPattern"`
			} `json:"dns"`
		} `json:"rules"`
	} `json:"toPorts"`
}

func hasPort(doc ciliumPolicyDoc, port, protocol string) bool {
	for _, rule := range doc.Spec.Egress {
		for _, tp := range rule.ToPorts {
			for _, p := range tp.Ports {
				if p.Port == port && p.Protocol == protocol {
					return true
				}
			}
		}
	}
	return false
}

func hasDNSProxy(doc ciliumPolicyDoc) bool {
	for _, rule := range doc.Spec.Egress {
		for _, tp := range rule.ToPorts {
			for _, dns := range tp.Rules.DNS {
				if dns.MatchPattern == "*" {
					return true
				}
			}
		}
	}
	return false
}

func fqdnNames(doc ciliumPolicyDoc) []string {
	var names []string
	for _, rule := range doc.Spec.Egress {
		for _, name := range rule.ToFQDNs {
			names = append(names, name.MatchName)
		}
	}
	return names
}

func TestRuntimeClassOmittedAndCodexMount(t *testing.T) {
	cfg := baseCfg()
	cfg.RuntimeClass = ""
	c, kube, _ := testClient(t, cfg)
	ctx := context.Background()
	err := c.Create(ctx, host.Spec{
		Name:    "beta",
		Backend: "codex",
		Secret: map[string][]byte{
			host.KeyBotToken:  []byte("t"),
			host.KeyCodexAuth: []byte(`{"tokens":{}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sts, err := kube.AppsV1().StatefulSets("pvebot").Get(ctx, "bot-beta", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatal("expected the cluster default runtime")
	}
	if len(sts.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatal("empty node must not set a selector")
	}
	found := false
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "codex" && m.MountPath == "/var/run/secrets/codex" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("codex auth was not mounted read-only")
	}
}

func TestStopStartDeleteAndNodes(t *testing.T) {
	c, kube, _ := testClient(t, baseCfg())
	ctx := context.Background()
	if err := c.Create(ctx, host.Spec{Name: "a", Node: "n", Backend: "stub", Secret: map[string][]byte{host.KeyBotToken: []byte("t")}}); err != nil {
		t.Fatal(err)
	}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-a-0", Namespace: "pvebot", Labels: map[string]string{host.BotLabel: "a"}},
		Spec:       corev1.PodSpec{NodeName: "n"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
	if _, err := kube.CoreV1().Pods("pvebot").Create(ctx, ready, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx, "a")
	if err != nil || st.Phase != "running" || st.Node != "n" {
		t.Fatalf("status = %+v %v", st, err)
	}
	if err := c.Stop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	st, err = c.Status(ctx, "a")
	if err != nil || st.Phase != "stopped" {
		t.Fatalf("after stop = %+v %v", st, err)
	}
	if err := c.Start(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	sts, _ := kube.AppsV1().StatefulSets("pvebot").Get(ctx, "bot-a", metav1.GetOptions{})
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Fatal("start did not scale to 1")
	}
	if err := c.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.AppsV1().StatefulSets("pvebot").Get(ctx, "bot-a", metav1.GetOptions{}); err == nil {
		t.Fatal("statefulset still present")
	}
	if _, err := c.Status(ctx, "missing"); !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("missing status err = %v", err)
	}

	if _, err := kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != "worker-1" {
		t.Fatalf("nodes = %v", nodes)
	}
}
