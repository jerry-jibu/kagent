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
//   - hot reload: a successfully parsed changed directory first invokes
//     onUpdate (wired to Store recording) and only swaps Current after that
//     succeeds — a served version always carries a persisted hash (PR #11
//     review: record-then-publish could serve a version nobody recorded). A
//     parse failure keeps the last good policy (degrade, never crash).
//   - onUpdate failures keep the previous version served and are retried on
//     subsequent ticks, so a transient Store error cannot strand the new
//     version: it is re-read and re-attempted until it publishes.
type Watcher struct {
	dir      string
	interval time.Duration
	onUpdate func(Policy) error
	log      *slog.Logger

	mu          sync.RWMutex
	current     Policy
	loadedAt    time.Time
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
		dir:      dir,
		interval: interval,
		onUpdate: onUpdate,
		log:      log,
		current:  p,
		loadedAt: time.Now(),
		stop:     make(chan struct{}),
		stopDone: make(chan struct{}),
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
// onUpdate failures keep serving the previous version and retry on later
// ticks — Current only ever moves to a version whose onUpdate succeeded
// (record-before-publish, PR #11 review).
func (w *Watcher) tick(ctx context.Context) {
	w.mu.RLock()
	served := w.current
	w.mu.RUnlock()

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

	if w.onUpdate != nil {
		if err := w.onUpdate(loaded); err != nil {
			w.log.Error("failed to record policy version; keeping the previous version and retrying on the next tick",
				"version", loaded.Version, "previous_version", served.Version, "error", err)
			return
		}
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
