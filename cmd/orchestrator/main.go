package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/popododo0720/pvebot/internal/host/kubernetes"
	"github.com/popododo0720/pvebot/internal/orch"
	"github.com/popododo0720/pvebot/internal/store"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	data := flag.String("data", "/data", "directory for the sqlite database")
	namespace := flag.String("namespace", "pvebot", "namespace for bot pods")
	botImage := flag.String("bot-image", "", "container image for bot pods")
	orchURL := flag.String("orch-url", "", "orchestrator URL reachable from bot pods")
	runtimeClass := flag.String("runtime-class", "", "RuntimeClass name, empty for the cluster default")
	storageClass := flag.String("storage-class", "", "StorageClass for bot volumes, empty for the default")
	storageSize := flag.String("storage-size", "1Gi", "bot workspace volume size")
	fqdn := flag.String("egress-fqdns", "api.anthropic.com,github.com,api.openai.com", "comma-separated FQDN egress allow list")
	flag.Parse()

	key, err := parseKey(os.Getenv("PVEBOT_MASTER_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	if *botImage == "" || *orchURL == "" {
		log.Fatal("bot-image and orch-url are required")
	}
	st, err := store.Open(filepath.Join(*data, "pvebot.db"), key)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	kube, dyn, err := kubernetes.ClusterOrKubeconfig()
	if err != nil {
		log.Fatal(err)
	}
	var fqdns []string
	for _, name := range strings.Split(*fqdn, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			fqdns = append(fqdns, name)
		}
	}
	client, err := kubernetes.New(kubernetes.Config{
		Namespace:       *namespace,
		BotImage:        *botImage,
		OrchestratorURL: *orchURL,
		RuntimeClass:    *runtimeClass,
		StorageClass:    *storageClass,
		StorageSize:     *storageSize,
		EgressFQDNs:     fqdns,
	}, kube, dyn)
	if err != nil {
		log.Fatal(err)
	}
	handler := orch.NewHandler(&orch.Service{Store: st, Host: client})
	log.Printf("listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, handler))
}

func parseKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("PVEBOT_MASTER_KEY is required, 32 bytes of standard base64")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("PVEBOT_MASTER_KEY must be 32 bytes of standard base64")
	}
	return key, nil
}
