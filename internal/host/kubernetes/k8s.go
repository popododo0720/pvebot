// Package kubernetes implements host.Backend with pods, persistent volumes,
// and network policy.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/popododo0720/pvebot/internal/host"
)

var CiliumNetworkPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

type Config struct {
	Namespace       string
	BotImage        string
	PullPolicy      corev1.PullPolicy
	OrchestratorURL string
	RuntimeClass    string
	StorageClass    string
	StorageSize     string
	EgressFQDNs     []string
}

type Client struct {
	cfg  Config
	size resource.Quantity
	kube kubernetes.Interface
	dyn  dynamic.Interface
}

func New(cfg Config, kube kubernetes.Interface, dyn dynamic.Interface) (*Client, error) {
	if cfg.Namespace == "" || cfg.BotImage == "" || cfg.OrchestratorURL == "" {
		return nil, errors.New("namespace, bot image, and orchestrator URL are required")
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = "1Gi"
	}
	if cfg.PullPolicy == "" {
		cfg.PullPolicy = corev1.PullIfNotPresent
	}
	qty, err := resource.ParseQuantity(cfg.StorageSize)
	if err != nil {
		return nil, fmt.Errorf("storage size: %w", err)
	}
	return &Client{cfg: cfg, size: qty, kube: kube, dyn: dyn}, nil
}

func ClusterOrKubeconfig() (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, nil, err
		}
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return kube, dyn, nil
}

func objectName(bot string) string { return "bot-" + bot }

func (c *Client) Create(ctx context.Context, spec host.Spec) error {
	name := objectName(spec.Name)
	_, err := c.kube.AppsV1().StatefulSets(c.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return host.ErrExists
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := c.apply(ctx, spec); err != nil {
		_ = c.Delete(ctx, spec.Name)
		return err
	}
	return nil
}

func (c *Client) apply(ctx context.Context, spec host.Spec) error {
	ns := c.cfg.Namespace
	name := objectName(spec.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: botLabels(spec.Name)},
		Data:       spec.Secret,
	}
	if _, err := c.kube.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create secret: %w", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: botLabels(spec.Name)},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  botLabels(spec.Name),
			Ports:     []corev1.ServicePort{{Name: "http", Port: 8081}},
		},
	}
	if _, err := c.kube.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	sts := c.statefulSet(spec)
	if _, err := c.kube.AppsV1().StatefulSets(ns).Create(ctx, sts, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create statefulset: %w", err)
	}
	np := c.networkPolicy(spec)
	if _, err := c.kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create networkpolicy: %w", err)
	}
	cnp := c.ciliumPolicy(spec)
	if _, err := c.dyn.Resource(CiliumNetworkPolicyGVR).Namespace(ns).Create(ctx, cnp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create cilium network policy: %w", err)
	}
	return nil
}

func (c *Client) statefulSet(spec host.Spec) *appsv1.StatefulSet {
	name := objectName(spec.Name)
	replicas := int32(1)
	labels := botLabels(spec.Name)
	podSpec := corev1.PodSpec{
		TerminationGracePeriodSeconds: int64ptr(5),
		Containers: []corev1.Container{{
			Name:            "runtime",
			Image:           c.cfg.BotImage,
			ImagePullPolicy: c.cfg.PullPolicy,
			Env:             envVars(spec, c.cfg, name),
			Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 8081}},
			VolumeMounts:    volumeMounts(spec),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(8081)},
				},
				PeriodSeconds: 2,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
		}},
		Volumes: volumes(spec, name),
	}
	if c.cfg.RuntimeClass != "" {
		podSpec.RuntimeClassName = &c.cfg.RuntimeClass
	}
	if spec.Node != "" {
		podSpec.NodeSelector = map[string]string{"kubernetes.io/hostname": spec.Node}
	}
	qty := c.size
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if c.cfg.StorageClass != "" {
		claim.Spec.StorageClassName = &c.cfg.StorageClass
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.cfg.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}

func envVars(spec host.Spec, cfg Config, secretName string) []corev1.EnvVar {
	ref := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  key,
		}}
	}
	env := []corev1.EnvVar{
		{Name: "PVEBOT_BOT_ID", Value: spec.Name},
		{Name: "PVEBOT_ORCHESTRATOR_URL", Value: cfg.OrchestratorURL},
		{Name: "PVEBOT_BACKEND", Value: spec.Backend},
		{Name: "PVEBOT_WORKDIR", Value: "/workspace"},
		{Name: "HOME", Value: "/root"},
		{Name: "PVEBOT_BOT_TOKEN", ValueFrom: ref(host.KeyBotToken)},
	}
	if _, ok := spec.Secret[host.KeyClaudeToken]; ok {
		env = append(env, corev1.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", ValueFrom: ref(host.KeyClaudeToken)})
	}
	if _, ok := spec.Secret[host.KeyAPIKey]; ok {
		env = append(env, corev1.EnvVar{Name: "PVEBOT_API_KEY", ValueFrom: ref(host.KeyAPIKey)})
	}
	return env
}

func volumeMounts(spec host.Spec) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}
	if _, ok := spec.Secret[host.KeyCodexAuth]; ok {
		mounts = append(mounts, corev1.VolumeMount{Name: "codex", MountPath: "/var/run/secrets/codex", ReadOnly: true})
	}
	return mounts
}

func volumes(spec host.Spec, secretName string) []corev1.Volume {
	if _, ok := spec.Secret[host.KeyCodexAuth]; !ok {
		return nil
	}
	mode := int32(0600)
	return []corev1.Volume{{
		Name: "codex",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items:      []corev1.KeyToPath{{Key: host.KeyCodexAuth, Path: "auth.json", Mode: &mode}},
			},
		},
	}}
}

func (c *Client) networkPolicy(spec host.Spec) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port8080 := intstr.FromInt(8080)
	port53 := intstr.FromInt(53)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectName(spec.Name),
			Namespace: c.cfg.Namespace,
			Labels:    botLabels(spec.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{host.BotLabel: spec.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							host.OrchestratorAppLabel: host.OrchestratorAppValue,
						}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port8080}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": "kube-system",
						}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &port53},
						{Protocol: &tcp, Port: &port53},
					},
				},
			},
		},
	}
}

func (c *Client) ciliumPolicy(spec host.Spec) *unstructured.Unstructured {
	fqdns := make([]any, 0, len(c.cfg.EgressFQDNs))
	for _, name := range c.cfg.EgressFQDNs {
		fqdns = append(fqdns, map[string]any{"matchName": name})
	}
	egress := []any{
		map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
				host.OrchestratorAppLabel:         host.OrchestratorAppValue,
				"k8s:io.kubernetes.pod.namespace": c.cfg.Namespace,
			}}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "8080", "protocol": "TCP"}}}},
		},
		map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
				"k8s:io.kubernetes.pod.namespace": "kube-system",
				"k8s-app":                         "kube-dns",
			}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
				"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
			}},
		},
	}
	if len(fqdns) > 0 {
		egress = append(egress, map[string]any{
			"toFQDNs": fqdns,
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name":      objectName(spec.Name),
			"namespace": c.cfg.Namespace,
			"labels":    map[string]any{host.BotLabel: spec.Name},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{host.BotLabel: spec.Name}},
			"ingress":          []any{},
			"egress":           egress,
		},
	}}
}

func botLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": "pvebot-bot",
		host.BotLabel:            name,
	}
}

func (c *Client) Start(ctx context.Context, name string) error { return c.scale(ctx, name, 1) }

func (c *Client) Stop(ctx context.Context, name string) error { return c.scale(ctx, name, 0) }

func (c *Client) scale(ctx context.Context, name string, replicas int32) error {
	sts, err := c.kube.AppsV1().StatefulSets(c.cfg.Namespace).Get(ctx, objectName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return host.ErrNotFound
	}
	if err != nil {
		return err
	}
	sts.Spec.Replicas = &replicas
	_, err = c.kube.AppsV1().StatefulSets(c.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

func (c *Client) Status(ctx context.Context, name string) (host.Status, error) {
	sts, err := c.kube.AppsV1().StatefulSets(c.cfg.Namespace).Get(ctx, objectName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return host.Status{}, host.ErrNotFound
	}
	if err != nil {
		return host.Status{}, err
	}
	st := host.Status{Name: name, Phase: "pending"}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
		st.Phase = "stopped"
		return st, nil
	}
	pods, err := c.kube.CoreV1().Pods(c.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: host.BotLabel + "=" + name,
	})
	if err != nil {
		return host.Status{}, err
	}
	for _, pod := range pods.Items {
		st.Node = pod.Spec.NodeName
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				st.Phase = "running"
				return st, nil
			}
		}
		if pod.Status.Phase == corev1.PodFailed {
			st.Phase = "failed"
		}
	}
	return st, nil
}

func (c *Client) Delete(ctx context.Context, name string) error {
	ns := c.cfg.Namespace
	obj := objectName(name)
	var errs []error
	errs = append(errs, ignoreNotFound(c.kube.AppsV1().StatefulSets(ns).Delete(ctx, obj, metav1.DeleteOptions{})))
	errs = append(errs, ignoreNotFound(c.kube.CoreV1().Services(ns).Delete(ctx, obj, metav1.DeleteOptions{})))
	errs = append(errs, ignoreNotFound(c.kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, obj, metav1.DeleteOptions{})))
	errs = append(errs, ignoreNotFound(c.dyn.Resource(CiliumNetworkPolicyGVR).Namespace(ns).Delete(ctx, obj, metav1.DeleteOptions{})))
	errs = append(errs, ignoreNotFound(c.kube.CoreV1().Secrets(ns).Delete(ctx, obj, metav1.DeleteOptions{})))
	errs = append(errs, ignoreNotFound(c.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, "workspace-"+obj+"-0", metav1.DeleteOptions{})))
	return errors.Join(errs...)
}

func (c *Client) Logs(ctx context.Context, name string, tail int64) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	req := c.kube.CoreV1().Pods(c.cfg.Namespace).GetLogs(objectName(name)+"-0", &corev1.PodLogOptions{
		Container: "runtime",
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if apierrors.IsNotFound(err) {
		return "", host.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	defer stream.Close()
	buf, err := io.ReadAll(io.LimitReader(stream, 64<<10))
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func (c *Client) ListNodes(ctx context.Context) ([]string, error) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var schedulable []string
	var all []string
	for _, node := range list.Items {
		all = append(all, node.Name)
		if nodeSchedulable(node) {
			schedulable = append(schedulable, node.Name)
		}
	}
	if len(schedulable) == 0 {
		return all, nil
	}
	return schedulable, nil
}

func nodeSchedulable(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			return false
		}
	}
	return true
}

func ignoreNotFound(err error) error {
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func int64ptr(v int64) *int64 { return &v }
