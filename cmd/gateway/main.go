// hermes-gateway: control plane + user proxy for hermes-as-a-service.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aditya-shantanu/ai-agent-service/internal/api"
	"github.com/aditya-shantanu/ai-agent-service/internal/config"
	"github.com/aditya-shantanu/ai-agent-service/internal/idle"
	"github.com/aditya-shantanu/ai-agent-service/internal/proxy"
	"github.com/aditya-shantanu/ai-agent-service/internal/sandbox"
	"github.com/aditya-shantanu/ai-agent-service/internal/server"
	"github.com/aditya-shantanu/ai-agent-service/internal/telegram"
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
	execRunner := &sandbox.SPDYExecRunner{Clients: clients}
	lifecycle := &sandbox.Lifecycle{
		Clients:   clients,
		Namespace: cfg.Namespace,
		Resolver:  resolver,
		Exec:      execRunner,
	}

	injector := &telegram.Injector{
		Clients:     clients,
		Namespace:   cfg.Namespace,
		Resolver:    resolver,
		Lifecycle:   lifecycle,
		Exec:        execRunner,
		WakeTimeout: cfg.WakeTimeout,
	}

	tracker := idle.NewTracker()

	handlers := &api.Handlers{
		Provisioner:      provisioner,
		Lifecycle:        lifecycle,
		Resolver:         resolver,
		Telegram:         injector,
		Activity:         tracker,
		ProvisionTimeout: cfg.ProvisionTimeout,
		WakeTimeout:      cfg.WakeTimeout,
	}

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
		ActiveTimeout:        cfg.IdleActiveTimeout,
		SuspendTelegramUsers: cfg.SuspendTelegramUsers,
	}
	// Background loops and in-flight requests all stop on SIGINT/SIGTERM —
	// Kubernetes sends SIGTERM on every rollout, and dropping held
	// wake-on-connect requests there would surface as user-visible errors.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go suspender.Run(ctx)

	cronWaker := &idle.CronWaker{
		Resolver:    resolver,
		Lifecycle:   lifecycle,
		Grace:       cfg.CronGrace,
		WakeTimeout: cfg.WakeTimeout,
	}
	go cronWaker.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.New(cfg, handlers, userProxy),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("hermes-gateway listening", "addr", cfg.ListenAddr, "namespace", cfg.Namespace)

	select {
	case err := <-errCh:
		slog.Error("server", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		// Give held wake-on-connect requests a chance to finish; the
		// drain window must cover WakeTimeout to avoid dropping them.
		slog.Info("shutting down", "drain", cfg.WakeTimeout+5*time.Second)
		shCtx, cancel := context.WithTimeout(context.Background(), cfg.WakeTimeout+5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			slog.Warn("shutdown", "err", err)
		}
	}
}
