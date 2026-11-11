package runtime

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "cli":
			os.Exit(CLI(os.Stdin, os.Stdout))
		case "mcp-proxy":
			os.Exit(Proxy())
		}
	}
	os.Exit(m.Run())
}
