// Command orchestrator is the superagent-on-kagent P0 control plane service
// skeleton (T1): it connects to PostgreSQL, applies schema migrations, takes
// the single-instance lock, hot-reloads gating policy from a ConfigMap
// mount, and serves healthz/admin endpoints.
//
// Startup failure semantics (T1 AC): an unreachable database, a held
// instance lock, failed migrations, or a broken initial policy all refuse
// the start with a logged reason. A broken policy detected later degrades
// to the last good version without crashing (policy.Watcher).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
	"github.com/jerry-jibu/kagent/orchestrator/internal/server"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// Config groups the service settings (CLI flags per STYLE.md).
type Config struct {
	DatabaseURL        string
	Schema             string
	PolicyDir          string
	ListenAddr         string
	PolicyPollInterval time.Duration
}

func parseConfig() Config {
	var cfg Config
	flag.StringVar(&cfg.DatabaseURL, "database-url", "", "PostgreSQL connection string (required)")
	flag.StringVar(&cfg.Schema, "schema", "orchestrator", "orchestrator-owned PostgreSQL schema")
	flag.StringVar(&cfg.PolicyDir, "policy-dir", "/etc/orchestrator/policy", "directory mounted from the policy ConfigMap")
	flag.StringVar(&cfg.ListenAddr, "listen-addr", ":8080", "healthz/admin HTTP listen address")
	flag.DurationVar(&cfg.PolicyPollInterval, "policy-poll-interval", 5*time.Second, "policy reload poll interval")
	flag.Parse()
	return cfg
}

// startupState feeds the healthz readiness probe: it reports exactly which
// startup steps completed, so a partially-started process probes 503.
type startupState struct {
	lockHeld     bool
	migrated     bool
	policyLoaded bool
}

func (s *startupState) ready() error {
	switch {
	case !s.lockHeld:
		return fmt.Errorf("instance lock not held")
	case !s.migrated:
		return fmt.Errorf("migrations not applied")
	case !s.policyLoaded:
		return fmt.Errorf("policy not loaded")
	default:
		return nil
	}
}

func main() {
	cfg := parseConfig()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if cfg.DatabaseURL == "" {
		log.Error("--database-url is required")
		os.Exit(2)
	}

	ctx := context.Background()
	state := &startupState{}

	st, err := store.Open(ctx, store.Config{DatabaseURL: cfg.DatabaseURL, Schema: cfg.Schema})
	if err != nil {
		log.Error("failed to open store", "error", err)
		os.Exit(1)
	}

	// Lock first, then migrate: a second instance must never race migrations.
	if err := st.TryAcquireInstanceLock(ctx); err != nil {
		log.Error("failed to acquire instance lock; refusing to start",
			"error", err, "schema", cfg.Schema)
		st.Close()
		os.Exit(1)
	}
	state.lockHeld = true
	log.Info("instance lock acquired", "schema", cfg.Schema)

	if err := st.Migrate(ctx); err != nil {
		log.Error("failed to apply migrations", "error", err)
		st.Close()
		os.Exit(1)
	}
	state.migrated = true
	log.Info("migrations applied", "schema", cfg.Schema)

	// Policy: initial load failure refuses the start (AC: 拒绝启动 vs 降级).
	watcher, err := policy.StartWatcher(cfg.PolicyDir, cfg.PolicyPollInterval,
		func(p policy.Policy) error {
			if err := st.RecordPolicyVersion(ctx, p.Version, p.Canonical()); err != nil {
				return fmt.Errorf("failed to record policy version %s: %w", p.Version, err)
			}
			return nil
		}, log)
	if err != nil {
		log.Error("failed to load initial policy; refusing to start", "error", err)
		st.Close()
		os.Exit(1)
	}
	current := watcher.Current()
	if err := st.RecordPolicyVersion(ctx, current.Version, current.Canonical()); err != nil {
		log.Error("failed to record initial policy version; refusing to start", "error", err)
		watcher.Stop()
		st.Close()
		os.Exit(1)
	}
	state.policyLoaded = true
	log.Info("policy version recorded", "version", current.Version, "dir", cfg.PolicyDir)

	handler := server.NewHandler(state.ready, func() (string, time.Time, bool) {
		return watcher.Current().Version, watcher.LoadedAt(), true
	})
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("orchestrator serving", "addr", cfg.ListenAddr, "schema", cfg.Schema)
		serveErr <- httpSrv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-serveErr:
		log.Error("http server failed", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	watcher.Stop()
	st.Close()
}
