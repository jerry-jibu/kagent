// Command orchestrator is the superagent-on-kagent P0 control plane service
// skeleton (T1): it connects to PostgreSQL, applies schema migrations, takes
// the single-instance lock, hot-reloads gating policy from a ConfigMap
// mount, and serves healthz/admin endpoints. From T2 it also runs the
// GitHub source poller (trigger ingestion chain) when --github-source is
// set. From T4 it also runs the eval loop (pending triggers → Gate →
// evaluator → accept creates Ticket/Mandate/Assignment in one transaction)
// and the outbox deliverer (confirmation comments to GitHub with
// marker-idempotent receipts) when --evaluator-agent and --a2a-gateway-url
// are set.
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

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/eval"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
	"github.com/jerry-jibu/kagent/orchestrator/internal/server"
	"github.com/jerry-jibu/kagent/orchestrator/internal/source"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DefaultEvalInterval is the eval/deliver cadence. The eval loop must run
// noticeably faster than the source poll so a trigger is judged within one
// poll cycle of its ingestion.
const DefaultEvalInterval = 5 * time.Second

// Config groups the service settings (CLI flags per STYLE.md).
type Config struct {
	DatabaseURL        string
	Schema             string
	PolicyDir          string
	ListenAddr         string
	PolicyPollInterval time.Duration

	GitHubSource       string
	GitHubToken        string
	GitHubAPIURL       string
	GitHubPollInterval time.Duration
	GitHubStartCursor  string

	A2AGatewayURL  string
	EvaluatorAgent string
	EvalInterval   time.Duration
}

func parseConfig() Config {
	var cfg Config
	flag.StringVar(&cfg.DatabaseURL, "database-url", "", "PostgreSQL connection string (required)")
	flag.StringVar(&cfg.Schema, "schema", "orchestrator", "orchestrator-owned PostgreSQL schema")
	flag.StringVar(&cfg.PolicyDir, "policy-dir", "/etc/orchestrator/policy", "directory mounted from the policy ConfigMap")
	flag.StringVar(&cfg.ListenAddr, "listen-addr", ":8080", "healthz/admin HTTP listen address")
	flag.DurationVar(&cfg.PolicyPollInterval, "policy-poll-interval", 5*time.Second, "policy reload poll interval")
	flag.StringVar(&cfg.GitHubSource, "github-source", "", "GitHub repository (owner/name) to poll for issue comments; empty disables the GitHub source")
	flag.StringVar(&cfg.GitHubToken, "github-token", "", "GitHub bearer token (mounted from a Secret)")
	flag.StringVar(&cfg.GitHubAPIURL, "github-api-url", "https://api.github.com", "GitHub REST API base URL")
	flag.DurationVar(&cfg.GitHubPollInterval, "github-poll-interval", source.DefaultPollInterval, "GitHub source poll interval")
	flag.StringVar(&cfg.GitHubStartCursor, "github-start-cursor", "", "RFC3339 position a fresh GitHub source starts reading from (default: the repository origin)")
	flag.StringVar(&cfg.A2AGatewayURL, "a2a-gateway-url", "", "kagent A2A gateway origin; required for the eval loop")
	flag.StringVar(&cfg.EvaluatorAgent, "evaluator-agent", "", "evaluator agent route (name or namespace/name) invoked for assessments; required for the eval loop")
	flag.DurationVar(&cfg.EvalInterval, "eval-interval", DefaultEvalInterval, "eval + outbox delivery cycle interval")
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

	// GitHub source (T2): one repository inbox per process, poll cadence
	// with a 5s cursor rewind; ingestion is atomic in the store. The
	// transport is shared with the outbox deliverer — the orchestrator holds
	// the only GitHub credentials (docs/designs/0001 §7).
	pollerCtx, cancelPoller := context.WithCancel(ctx)
	var poller *source.Poller
	if cfg.GitHubSource != "" {
		var startCursor time.Time
		if cfg.GitHubStartCursor != "" {
			startCursor, err = time.Parse(time.RFC3339, cfg.GitHubStartCursor)
			if err != nil {
				log.Error("--github-start-cursor must be RFC3339", "error", err)
				cancelPoller()
				watcher.Stop()
				st.Close()
				os.Exit(1)
			}
		}
		ghTransport := github.NewTransport(github.TransportConfig{
			BaseURL: cfg.GitHubAPIURL,
			Token:   cfg.GitHubToken,
		})
		poller, err = source.New(ghTransport, intake.New(st), source.Source{
			Repo:         cfg.GitHubSource,
			PollInterval: cfg.GitHubPollInterval,
			StartCursor:  startCursor,
		})
		if err != nil {
			log.Error("failed to configure GitHub source; refusing to start", "error", err)
			cancelPoller()
			watcher.Stop()
			st.Close()
			os.Exit(1)
		}
		go poller.Run(pollerCtx, log)
		log.Info("github source started",
			"repo", cfg.GitHubSource,
			"poll_interval", cfg.GitHubPollInterval.String(),
			"start_cursor", cfg.GitHubStartCursor)

		// Eval loop + outbox deliverer (T4): accept decisions become
		// tickets in one transaction, committed effects are delivered to
		// GitHub with marker-idempotent receipts. The first cycle runs
		// immediately, so a restart replays effects committed by a previous
		// life before the first poll lands.
		if cfg.EvaluatorAgent == "" || cfg.A2AGatewayURL == "" {
			log.Info("eval loop disabled: --evaluator-agent and --a2a-gateway-url are required with a source")
		} else {
			engine := eval.NewEngine(st, eval.NewEvaluator(
				a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: cfg.A2AGatewayURL}),
				cfg.EvaluatorAgent))
			deliverer := outbox.New(st, ghTransport)
			go runEvalLoop(pollerCtx, cfg.GitHubSource, engine, deliverer, watcher, cfg.EvalInterval, log)
			log.Info("eval loop started",
				"agent", cfg.EvaluatorAgent,
				"gateway", cfg.A2AGatewayURL,
				"eval_interval", cfg.EvalInterval.String())
		}
	} else {
		log.Info("github source disabled (--github-source not set)")
	}

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
	cancelPoller()
	watcher.Stop()
	st.Close()
}

// runEvalLoop judges pending triggers on the eval cadence and drains the
// outbox afterwards. Errors are logged per cycle and never stop the loop:
// a failing evaluator or GitHub is retried on the next tick, and the store's
// transactional guarantees keep every retry safe (triggers are judged once,
// effects replay marker-idempotently).
func runEvalLoop(ctx context.Context, repo string, engine *eval.Engine, deliverer *outbox.Deliverer,
	watcher *policy.Watcher, interval time.Duration, log *slog.Logger) {

	source := github.SourceName(repo)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	cycle := func() {
		stats, err := engine.EvaluatePending(ctx, source, watcher.Current(), eval.DefaultEvaluateBatch)
		if err != nil {
			log.Error("eval cycle failed", "source", source, "error", err)
		}
		if stats.Evaluated > 0 {
			log.Info("eval cycle judged", "source", source,
				"evaluated", stats.Evaluated, "tickets", stats.Tickets, "outcomes", stats.Outcomes)
		}

		delivered, err := deliverer.DeliverPending(ctx)
		if err != nil {
			log.Error("outbox delivery failed", "source", source, "error", err)
		}
		if delivered.Delivered+delivered.Confirmed > 0 {
			log.Info("outbox delivered", "source", source,
				"posted", delivered.Delivered, "confirmed", delivered.Confirmed)
		}
	}

	cycle()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle()
		}
	}
}
