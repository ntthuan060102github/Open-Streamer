package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startService / stopService are the goroutine-tracker primitives the
// rest of the diff path stands on. Bugs here would either leak goroutines
// (stopService misses the entry) or duplicate them (a stale entry isn't
// torn down before respawn). Both are silent failures in production.

// startService spawns and stopService cleanly cancels.
func TestStartAndStopService(t *testing.T) {
	t.Parallel()
	m := New(t.Context(), Deps{})

	ran := make(chan struct{}, 1)
	m.startService("test", func(ctx context.Context) error {
		ran <- struct{}{}
		<-ctx.Done()
		return nil
	})

	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("startService did not invoke fn")
	}

	m.stopService("test")
	m.mu.Lock()
	_, exists := m.services["test"]
	m.mu.Unlock()
	assert.False(t, exists, "service entry must be removed after stopService")
}

// stopService on an unregistered name is a no-op (idempotent — diff path
// calls it on every reconfigure, including for services that never
// started).
func TestStopServiceNoEntry(t *testing.T) {
	t.Parallel()
	m := New(t.Context(), Deps{})
	m.stopService("never-started") // must not panic
}

// startService called twice on the same name must stop the prior goroutine
// before spawning a new one — otherwise a config change creates a leak.
func TestStartServiceReplacesExisting(t *testing.T) {
	t.Parallel()
	m := New(t.Context(), Deps{})

	first, second := make(chan struct{}, 1), make(chan struct{}, 1)
	firstCancelled := atomic.Bool{}

	m.startService("test", func(ctx context.Context) error {
		first <- struct{}{}
		<-ctx.Done()
		firstCancelled.Store(true)
		return nil
	})
	<-first

	m.startService("test", func(ctx context.Context) error {
		second <- struct{}{}
		<-ctx.Done()
		return nil
	})

	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second startService did not invoke fn")
	}
	assert.True(t, firstCancelled.Load(), "first goroutine must have been cancelled")
	m.stopService("test")
}

// startService logs (but does not panic) when the goroutine returns a
// non-context error before its own ctx is cancelled. We can't easily
// assert on the log; the test guards against the goroutine's stop-channel
// not closing (which would deadlock stopService waiting on done).
func TestStartServiceReturningError(t *testing.T) {
	t.Parallel()
	m := New(t.Context(), Deps{})
	m.startService("erroring", func(_ context.Context) error {
		return errors.New("boom")
	})
	// Wait for the goroutine to finish on its own.
	require.Eventually(t, func() bool {
		m.mu.Lock()
		e, ok := m.services["erroring"]
		m.mu.Unlock()
		if !ok {
			return false
		}
		select {
		case <-e.done:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "errored goroutine must close its done chan")
	// stopService must still complete cleanly even though the goroutine
	// already exited — exercises the "cancel a finished context" path.
	m.stopService("erroring")
}

// diffService is the four-state transition table that drives the entire
// reconcile loop. Each case must dispatch correctly: spawning when nothing
// was running, stopping when the new config drops a service, and
// stop+start (restart) when the config changed.
func TestDiffServiceTransitions(t *testing.T) {
	t.Parallel()

	t.Run("off→on starts", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})
		invoked := make(chan struct{}, 1)
		m.diffService("svc", false, true, false, func(ctx context.Context) error {
			invoked <- struct{}{}
			<-ctx.Done()
			return nil
		})
		<-invoked
		m.stopService("svc")
	})

	t.Run("on→off stops", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})
		ran := make(chan struct{}, 1)
		m.startService("svc", func(ctx context.Context) error {
			ran <- struct{}{}
			<-ctx.Done()
			return nil
		})
		<-ran
		m.diffService("svc", true, false, false, nil) // startFn ignored on off→off
		m.mu.Lock()
		_, exists := m.services["svc"]
		m.mu.Unlock()
		assert.False(t, exists)
	})

	t.Run("on→on with config change restarts", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})

		startCount := atomic.Int32{}
		spawn := func(ctx context.Context) error {
			startCount.Add(1)
			<-ctx.Done()
			return nil
		}
		m.startService("svc", spawn)
		// Wait for the first spawn to actually start before asking diffService
		// to restart it — otherwise stopService might race with a not-yet-
		// scheduled goroutine.
		require.Eventually(t, func() bool { return startCount.Load() == 1 },
			time.Second, 10*time.Millisecond)

		m.diffService("svc", true, true, true, spawn)
		require.Eventually(t, func() bool { return startCount.Load() == 2 },
			time.Second, 10*time.Millisecond, "restart must spawn a second time")
		m.stopService("svc")
	})

	t.Run("on→on without change is no-op", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})
		startCount := atomic.Int32{}
		spawn := func(ctx context.Context) error {
			startCount.Add(1)
			<-ctx.Done()
			return nil
		}
		m.startService("svc", spawn)
		require.Eventually(t, func() bool { return startCount.Load() == 1 },
			time.Second, 10*time.Millisecond)

		m.diffService("svc", true, true, false, spawn)
		// Give it a beat — startCount must NOT increment.
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, int32(1), startCount.Load(), "no-op transition must not respawn")
		m.stopService("svc")
	})
}

// WaitAll blocks until every running service has exited. Empty services
// map returns immediately; populated map waits for each goroutine.
func TestWaitAll(t *testing.T) {
	t.Parallel()

	t.Run("empty returns immediately", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})
		done := make(chan struct{})
		go func() { m.WaitAll(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("WaitAll on empty services blocked unexpectedly")
		}
	})

	t.Run("blocks until services exit", func(t *testing.T) {
		t.Parallel()
		m := New(t.Context(), Deps{})
		m.startService("a", func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		})
		done := make(chan struct{})
		go func() { m.WaitAll(); close(done) }()

		// Should NOT have returned yet.
		select {
		case <-done:
			t.Fatal("WaitAll returned before services exited")
		case <-time.After(50 * time.Millisecond):
		}

		m.stopService("a")
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("WaitAll did not return after stopService")
		}
	})
}
