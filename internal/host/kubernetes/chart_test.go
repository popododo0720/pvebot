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
