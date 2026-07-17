// hermes-gateway: control plane + user proxy for hermes-as-a-service.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/api"
	"github.com/adityashantanu/ai-agent-service/internal/config"
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.New(cfg, handlers, nil /* proxy lands in M4 */),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("hermes-gateway listening", "addr", cfg.ListenAddr, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
