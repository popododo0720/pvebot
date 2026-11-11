//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTwoBotsMessageAndNetworkPolicy(t *testing.T) {
	base := env("PVEBOT_URL", "http://127.0.0.1:30080")
	ns := env("PVEBOT_NAMESPACE", "pvebot")
	svc := env("PVEBOT_SERVICE", "pvebot")

	health := getText(t, base+"/healthz")
	if !strings.Contains(health, "ok") {
		t.Fatalf("healthz = %q", health)
	}
	var nodes struct {
		Nodes []string `json:"nodes"`
	}
	getJSON(t, base+"/api/nodes", &nodes)
	if len(nodes.Nodes) == 0 {
		t.Fatal("cluster reported no schedulable nodes")
	}
	node := nodes.Nodes[0]

	t.Cleanup(func() {
		deleteBot(base, "a")
		deleteBot(base, "b")
	})
	createBot(t, base, "a", node)
	createBot(t, base, "b", node)
	waitPhase(t, base, "a", "running")
	waitPhase(t, base, "b", "running")

	for _, name := range []string{"a", "b"} {
		pod := kubectlJSON(t, "get", "pod", "-n", ns, "bot-"+name+"-0")
		spec, _ := pod["spec"].(map[string]any)
		if spec["runtimeClassName"] != nil && spec["runtimeClassName"] != "" {
			t.Fatalf("bot %s runtimeClassName = %v, CI must use the cluster default", name, spec["runtimeClassName"])
		}
		selector, _ := spec["nodeSelector"].(map[string]any)
		if selector["kubernetes.io/hostname"] != node {
			t.Fatalf("bot %s nodeSelector = %v, want %s", name, selector, node)
		}
		mustKubectl(t, "get", "pvc", "-n", ns, "workspace-bot-"+name+"-0")
		mustKubectl(t, "get", "networkpolicy", "-n", ns, "bot-"+name)
		mustKubectl(t, "get", "ciliumnetworkpolicy", "-n", ns, "bot-"+name)
	}

	postJSON(t, base+"/api/bots/a/messages", map[string]string{"body": "SEND b hello"}, http.StatusAccepted)
	reply := waitReply(t, base, "a")
	if reply != "ack:hello" {
		t.Fatalf("reply = %q", reply)
	}

	peerIP := podIP(t, ns, "b")
	if !eventuallyDenied(t, ns, "bot-a-0", "http://"+peerIP+":8081/healthz") {
		t.Fatal("bot A reached bot B directly, network policy did not block it")
	}
	orchURL := fmt.Sprintf("http://%s.%s.svc:8080/healthz", svc, ns)
	if err := eventuallyAllowed(t, ns, "bot-a-0", orchURL); err != nil {
		t.Fatalf("bot A could not reach the orchestrator: %v", err)
	}
}

func createBot(t *testing.T, base, name, node string) {
	t.Helper()
	postJSON(t, base+"/api/bots", map[string]string{"name": name, "node": node, "backend": "stub"}, http.StatusCreated)
}

func deleteBot(base, name string) {
	req, err := http.NewRequest(http.MethodDelete, base+"/api/bots/"+name, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func waitPhase(t *testing.T, base, name, phase string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		var view struct {
			Phase string `json:"phase"`
		}
		getJSON(t, base+"/api/bots/"+name, &view)
		last = view.Phase
		if last == phase {
			return
		}
		if last == "failed" {
			t.Fatalf("bot %s failed\n%s", name, output(t, "describe", "pod", "-n", env("PVEBOT_NAMESPACE", "pvebot"), "bot-"+name+"-0"))
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("bot %s phase %q, want %s\n%s", name, last, phase, output(t, "describe", "pod", "-n", env("PVEBOT_NAMESPACE", "pvebot"), "bot-"+name+"-0"))
}

func waitReply(t *testing.T, base, name string) string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var msgs []struct {
			Status string `json:"status"`
			Reply  string `json:"reply"`
		}
		getJSON(t, base+"/api/bots/"+name+"/messages", &msgs)
		if len(msgs) > 0 && msgs[len(msgs)-1].Status == "done" {
			return msgs[len(msgs)-1].Reply
		}
		if len(msgs) > 0 && msgs[len(msgs)-1].Status == "error" {
			t.Fatalf("message failed: %s\n%s", msgs[len(msgs)-1].Reply, output(t, "logs", "-n", env("PVEBOT_NAMESPACE", "pvebot"), "bot-"+name+"-0"))
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no reply\n%s", output(t, "logs", "-n", env("PVEBOT_NAMESPACE", "pvebot"), "bot-a-0"))
	return ""
}

func eventuallyDenied(t *testing.T, ns, pod, url string) bool {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		_, err := kubectl(t, "exec", "-n", ns, pod, "--", "wget", "-q", "-T", "3", "-O", "-", url)
		if err != nil {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

func eventuallyAllowed(t *testing.T, ns, pod, url string) error {
	t.Helper()
	var last error
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		out, err := kubectl(t, "exec", "-n", ns, pod, "--", "wget", "-q", "-T", "5", "-O", "-", url)
		if err == nil && strings.Contains(out, "ok") {
			return nil
		}
		last = fmt.Errorf("%w: %s", err, out)
		time.Sleep(2 * time.Second)
	}
	return last
}

func podIP(t *testing.T, ns, bot string) string {
	t.Helper()
	pod := kubectlJSON(t, "get", "pod", "-n", ns, "bot-"+bot+"-0")
	status, _ := pod["status"].(map[string]any)
	ip, _ := status["podIP"].(string)
	if ip == "" {
		t.Fatalf("bot %s has no pod IP", bot)
	}
	return ip
}

func getJSON(t *testing.T, url string, dest any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d: %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, dest); err != nil {
		t.Fatalf("GET %s json: %v body %s", url, err, body)
	}
}

func getText(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func postJSON(t *testing.T, url string, payload any, want int) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s status %d want %d: %s", url, resp.StatusCode, want, body)
	}
}

func kubectlJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out := mustKubectl(t, append(args, "-o", "json")...)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("kubectl json: %v\n%s", err, out)
	}
	return m
}

func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func output(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := kubectl(t, args...)
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
