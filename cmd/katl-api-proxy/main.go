package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/katl-dev/katl/internal/apiproxy"
)

func main() {
	configPath := flag.String("config", apiproxy.ConfigPath, "API proxy configuration path")
	statusPath := flag.String("status", apiproxy.StatusPath, "API proxy status path")
	flag.Parse()
	if err := run(*configPath, *statusPath); err != nil {
		fmt.Fprintln(os.Stderr, "katl-api-proxy:", err)
		os.Exit(1)
	}
}

func run(configPath, statusPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var config apiproxy.Config
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return (&apiproxy.Server{
		Config: config, StatusPath: statusPath, Logf: log.Printf,
	}).Run(ctx)
}
