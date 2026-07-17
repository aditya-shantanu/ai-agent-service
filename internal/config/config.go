// Package config carries all runtime configuration for the hermes-gateway
// binary. Everything is env-driven so the same image runs on kind and GKE.
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	// ListenAddr is the HTTP bind address, e.g. ":8080".
	ListenAddr string
	// Namespace all user claims/sandboxes live in.
	Namespace string
	// WarmPoolName is the SandboxWarmPool user claims reference.
	WarmPoolName string
	// AdminToken guards /api/v1/* mutations.
	AdminToken string

	// SandboxDashboardPort and SandboxAPIPort are the two Hermes ports proxied to.
	SandboxDashboardPort int
	SandboxAPIPort       int

	// SandboxAPIKey is the shared API_SERVER_KEY injected upstream by the proxy.
	SandboxAPIKey string

	// ProvisionTimeout bounds claim-adoption + first Ready wait.
	ProvisionTimeout time.Duration
	// WakeTimeout bounds the wake-on-connect hold.
	WakeTimeout time.Duration
	// IdleTimeout after which a user with no activity is suspended.
	IdleTimeout time.Duration
	// SuspendTelegramUsers, when false (default), exempts users with a
	// Telegram token from idle suspension (their bot long-polls in-pod).
	SuspendTelegramUsers bool
}

func FromEnv() (*Config, error) {
	c := &Config{
		ListenAddr:           envOr("LISTEN_ADDR", ":8080"),
		Namespace:            envOr("HERMES_NAMESPACE", "hermes-users"),
		WarmPoolName:         envOr("WARM_POOL_NAME", "hermes-pool"),
		AdminToken:           os.Getenv("ADMIN_TOKEN"),
		SandboxDashboardPort: 9119,
		SandboxAPIPort:       8642,
		SandboxAPIKey:        os.Getenv("SANDBOX_API_SERVER_KEY"),
		ProvisionTimeout:     durOr("PROVISION_TIMEOUT", 120*time.Second),
		WakeTimeout:          durOr("WAKE_TIMEOUT", 60*time.Second),
		IdleTimeout:          durOr("IDLE_TIMEOUT", time.Minute),
		SuspendTelegramUsers: os.Getenv("SUSPEND_TELEGRAM_USERS") == "true",
	}
	if c.AdminToken == "" {
		return nil, fmt.Errorf("ADMIN_TOKEN must be set")
	}
	if len(c.AdminToken) < 16 {
		return nil, fmt.Errorf("ADMIN_TOKEN must be at least 16 characters")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
