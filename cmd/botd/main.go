package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/popododo0720/pvebot/internal/runtime"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "cli":
			os.Exit(runtime.CLI(os.Stdin, os.Stdout))
		case "mcp-proxy":
			os.Exit(runtime.Proxy())
		}
	}
	cfg := runtime.Config{
		BotID:           os.Getenv("PVEBOT_BOT_ID"),
		Token:           os.Getenv("PVEBOT_BOT_TOKEN"),
		OrchestratorURL: os.Getenv("PVEBOT_ORCHESTRATOR_URL"),
		Backend:         os.Getenv("PVEBOT_BACKEND"),
		Workdir:         os.Getenv("PVEBOT_WORKDIR"),
		APIBase:         os.Getenv("PVEBOT_API_BASE"),
		APIModel:        os.Getenv("PVEBOT_API_MODEL"),
		Listen:          envOr("PVEBOT_LISTEN", ":8081"),
		RunTimeout:      durationEnv("PVEBOT_RUN_TIMEOUT", 10*time.Minute),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := runtime.Run(ctx, cfg); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
