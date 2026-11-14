package policy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Watcher hot-reloads the policy directory. Semantics (T1 AC):
//   - startup: StartWatcher loads the directory once; a load error is
//     returned so main refuses to start.
//   - hot reload: a successfully parsed changed directory swaps Current and
//     invokes onUpdate (wired to Store recording); a parse failure keeps the
//     last good policy (degrade, never crash).
//   - onUpdate failures are retried on subsequent ticks until the current
//     version has been delivered, so a transient Store error cannot make a
//     policy version invisible.
type Watcher struct {
	dir      string
	interval time.Duration
	onUpdate func(Policy) error
	log      *slog.Logger

	mu          sync.RWMutex
	current     Policy
	loadedAt    time.Time
	delivered   string // version last accepted by onUpdate
	stop        chan struct{}
	stopDone    chan struct{}
	stopStarted sync.Once
}

// StartWatcher loads the policy directory and starts the reload loop.
// The initial load error propagates: callers must refuse to start.
func StartWatcher(dir string, interval time.Duration, onUpdate func(Policy) error, log *slog.Logger) (*Watcher, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}

	p, err := Load(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to load initial policy from %s: %w", dir, err)
	}

	w := &Watcher{
		dir:       dir,
		interval:  interval,
		onUpdate:  onUpdate,
		log:       log,
		current:   p,
		loadedAt:  time.Now(),
		delivered: p.Version, // initial version needs no delivery
		stop:      make(chan struct{}),
		stopDone:  make(chan struct{}),
	}
	log.Info("policy loaded", "dir", dir, "version", p.Version,
		"repositories", len(p.Repositories), "admission_rules", len(p.Admission), "bots", len(p.Bots))

	go w.loop()
	return w, nil
}

// Current returns the served policy (last good on any failed reload).
func (w *Watcher) Current() Policy {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// LoadedAt reports when the current policy was loaded.
func (w *Watcher) LoadedAt() time.Time {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.loadedAt
}

// Stop terminates the reload loop and waits for it to exit.
func (w *Watcher) Stop() {
	w.stopStarted.Do(func() { close(w.stop) })
	<-w.stopDone
}

func (w *Watcher) loop() {
	defer close(w.stopDone)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.tick(context.Background())
		}
	}
}

// tick performs one reload scan. Parse failures keep the last good policy;
// onUpdate failures are retried on later ticks until delivered.
func (w *Watcher) tick(ctx context.Context) {
	w.mu.RLock()
	served := w.current
	delivered := w.delivered
	w.mu.RUnlock()

	// Retry a pending delivery first so a transient onUpdate error cannot
	// strand a version that is already being served.
	if delivered != served.Version && w.onUpdate != nil {
		if err := w.onUpdate(served); err != nil {
			w.log.Error("failed to record policy version; will retry",
				"version", served.Version, "error", err)
			return
		}
		w.mu.Lock()
		w.delivered = served.Version
		w.mu.Unlock()
		w.log.Info("policy version recorded", "version", served.Version)
		return
	}

	loaded, err := Load(w.dir)
	if err != nil {
		// Degrade: keep serving the last good policy (never crash on reload).
		w.log.Error("policy reload failed; keeping last good version",
			"version", served.Version, "error", err)
		return
	}
	if loaded.Version == served.Version {
		return
	}

	w.mu.Lock()
	w.current = loaded
	w.loadedAt = time.Now()
	w.mu.Unlock()
	w.log.Info("policy reloaded",
		"dir", w.dir,
		"previous_version", served.Version,
		"version", loaded.Version,
		"repositories", len(loaded.Repositories),
		"admission_rules", len(loaded.Admission),
		"bots", len(loaded.Bots))
}
