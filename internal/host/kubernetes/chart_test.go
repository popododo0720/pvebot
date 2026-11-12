package kubernetes

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/popododo0720/pvebot/internal/host"
)

func TestChartUsesOrchestratorLabel(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	want := host.OrchestratorAppLabel + ": " + host.OrchestratorAppValue
	for _, rel := range []string{
		"charts/pvebot/templates/deployment.yaml",
		"charts/pvebot/templates/service.yaml",
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s does not contain %s", rel, want)
		}
	}
}

func TestChartPlumbsFQDNEgress(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	deploy, err := os.ReadFile(filepath.Join(root, "charts/pvebot/templates/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deploy), "--fqdn-egress={{ .Values.networkPolicy.fqdnEgress.enabled }}") {
		t.Fatal("deployment does not pass networkPolicy.fqdnEgress.enabled")
	}
	values, err := os.ReadFile(filepath.Join(root, "charts/pvebot/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "fqdnEgress:") || !strings.Contains(string(values), "enabled: true") {
		t.Fatalf("values.yaml default is not fqdn egress enabled:\n%s", values)
	}
}
