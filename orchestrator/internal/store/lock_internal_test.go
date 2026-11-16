package store

// The instance-lock watchdog (PR #11 review, Required #2) split into two test
// layers: the probe-decision seam (watchInstanceLock) is covered here without
// PostgreSQL — held probes never fire onLoss, an error or a not-held probe
// fires it exactly once and ends the watch — and the SQL probe itself runs
// against the real lock connection in TestInstanceLockProbe.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchInstanceLockDecision(t *testing.T) {
	t.Run("Given a probe that always reports the lock held, when the watchdog runs several cycles and is stopped, assert that onLoss never fires", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var probes atomic.Int64
		lost := make(chan string, 1)
		go watchInstanceLock(ctx, time.Millisecond, func(context.Context) (bool, error) {
			probes.Add(1)
			return true, nil
		}, func(string) { lost <- "unexpected loss" })

		time.Sleep(20 * time.Millisecond)
		cancel()
		time.Sleep(20 * time.Millisecond)
		if probes.Load() < 2 {
			t.Fatalf("watchdog ran %d probes, want several cycles", probes.Load())
		}
		select {
		case why := <-lost:
			t.Fatalf("onLoss fired: %s", why)
		default:
		}
	})

	t.Run("Given a probe that reports the lock no longer held, when the watchdog probes, assert that onLoss fires exactly once with the not-held reason and the watch ends", func(t *testing.T) {
		lost := make(chan string, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchInstanceLock(context.Background(), time.Millisecond,
				func(context.Context) (bool, error) { return false, nil },
				func(why string) { lost <- why })
		}()

		select {
		case why := <-lost:
			if !strings.Contains(why, "no longer held") {
				t.Fatalf("loss reason = %q, want the not-held reason", why)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("onLoss never fired for a not-held probe")
		}
		<-done
	})

	t.Run("Given a probe whose connection errors, when the watchdog probes, assert that onLoss fires exactly once with the error as the reason and the watch ends", func(t *testing.T) {
		lost := make(chan string, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchInstanceLock(context.Background(), time.Millisecond,
				func(context.Context) (bool, error) { return false, errors.New("conn closed") },
				func(why string) { lost <- why })
		}()

		select {
		case why := <-lost:
			if !strings.Contains(why, "conn closed") {
				t.Fatalf("loss reason = %q, want the probe error", why)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("onLoss never fired for a broken probe")
		}
		<-done
	})
}

func TestInstanceLockProbe(t *testing.T) {
	t.Run("Given a store holding the instance lock, when the watchdog's reentrant probe runs on the same lock connection, assert that it reports held and leaves the lock undisturbed", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		if err := s.TryAcquireInstanceLock(ctx); err != nil {
			t.Fatalf("lock acquisition failed: %v", err)
		}

		// The probe runs on the SAME dedicated connection and is reentrant
		// per session: while the store holds the lock it must report held,
		// never steal or release anything.
		held, err := s.probeInstanceLock(ctx)
		if err != nil {
			t.Fatalf("instance lock probe failed: %v", err)
		}
		if !held {
			t.Fatal("the reentrant probe must report the lock held while the store holds it")
		}

		// The probe must not have disturbed the lock: a second store is
		// still refused.
		second, err := Open(ctx, Config{
			DatabaseURL: testDatabaseURL(t),
			Schema:      s.Schema(),
		})
		if err != nil {
			t.Fatalf("failed to open second store: %v", err)
		}
		defer second.Close()
		if err := second.TryAcquireInstanceLock(ctx); !errors.Is(err, ErrInstanceLocked) {
			t.Fatalf("want ErrInstanceLocked after the probe, got: %v", err)
		}
	})
}
