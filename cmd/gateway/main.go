// hermes-gateway: control plane + user proxy for hermes-as-a-service.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/api"
	"github.com/adityashantanu/ai-agent-service/internal/config"
	"github.com/adityashantanu/ai-agent-service/internal/idle"
	"github.com/adityashantanu/ai-agent-service/internal/proxy"
	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
	"github.com/adityashantanu/ai-agent-service/internal/server"
)

func main() {
	defaultKubeconfig := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig,
		"kubeconfig path (dev fallback; in-cluster config wins when available)")
	flag.Parse()

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	clients, err := sandbox.NewClients(*kubeconfig)
	if err != nil {
		slog.Error("kubernetes clients", "err", err)
		os.Exit(1)
	}

	resolver := &sandbox.Resolver{Clients: clients, Namespace: cfg.Namespace}
	provisioner := &sandbox.Provisioner{
		Clients:      clients,
		Namespace:    cfg.Namespace,
		WarmPoolName: cfg.WarmPoolName,
		Resolver:     resolver,
	}
	lifecycle := &sandbox.Lifecycle{Clients: clients, Namespace: cfg.Namespace, Resolver: resolver}

	handlers := &api.Handlers{
		Provisioner:      provisioner,
		Lifecycle:        lifecycle,
		Resolver:         resolver,
		ProvisionTimeout: cfg.ProvisionTimeout,
		WakeTimeout:      cfg.WakeTimeout,
	}

	tracker := idle.NewTracker()
	userProxy := &proxy.Proxy{
		Resolver:      resolver,
		Lifecycle:     lifecycle,
		Tracker:       tracker,
		DashboardPort: cfg.SandboxDashboardPort,
		APIPort:       cfg.SandboxAPIPort,
		APIKey:        cfg.SandboxAPIKey,
		WakeTimeout:   cfg.WakeTimeout,
	}
	suspender := &idle.Suspender{
		Tracker:              tracker,
		Resolver:             resolver,
		Lifecycle:            lifecycle,
		IdleTimeout:          cfg.IdleTimeout,
		SuspendTelegramUsers: cfg.SuspendTelegramUsers,
	}
	go suspender.Run(context.Background())

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.New(cfg, handlers, userProxy),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("hermes-gateway listening", "addr", cfg.ListenAddr, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
