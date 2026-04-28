package sessions

import (
	"context"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// runReaper closes sessions whose UpdatedAt is older than idleDur. For
// connection-bound sessions (RTMP/SRT/RTSP) the transport layer is the
// authoritative lifecycle source — but we still reap them in case the
// publisher's close path was missed (panic, ctx race). HTTP-based
// (HLS/DASH) sessions rely on the reaper as their primary close trigger
// because there's no TCP "the viewer left" signal.
//
// Tick cadence is min(5 s, idleDur/3) — small enough that recent dead
// sessions don't linger long, large enough to keep the lock churn low
// when there are 10 k+ active sessions.
func (s *service) runReaper(ctx context.Context) {
	tick := s.idleDur / 3
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.shutdownActiveSessions(ctx)
			return
		case <-t.C:
			s.reapOnce(ctx)
		}
	}
}

// reapOnce closes every session whose UpdatedAt is older than idleDur or
// whose total lifetime exceeds maxAlive (when configured). The ctx is
// forwarded to the close path so the EventSessionClosed publication carries
// the same trace as the reaper goroutine.
func (s *service) reapOnce(ctx context.Context) {
	now := s.now()
	idleCutoff := now.Add(-s.idleDur)
	var maxLifeCutoff time.Time
	if s.maxAlive > 0 {
		maxLifeCutoff = now.Add(-s.maxAlive)
	}

	// Two-phase: snapshot ids under RLock, close them under per-id lock.
	// Avoids holding the write lock across the (potentially many) close paths.
	s.mu.RLock()
	type victim struct {
		id     string
		reason domain.SessionCloseReason
	}
	victims := make([]victim, 0)
	for id, sess := range s.sessions {
		if sess.ClosedAt != nil {
			continue
		}
		if sess.UpdatedAt.Before(idleCutoff) {
			victims = append(victims, victim{id, domain.SessionCloseIdle})
			continue
		}
		if !maxLifeCutoff.IsZero() && sess.OpenedAt.Before(maxLifeCutoff) {
			victims = append(victims, victim{id, domain.SessionCloseIdle})
		}
	}
	s.mu.RUnlock()

	for _, v := range victims {
		s.closeByIDCtx(ctx, v.id, v.reason, 0)
	}
}

// shutdownActiveSessions closes every still-active session with reason=shutdown.
// Called from the reaper when the parent context is cancelled — uses a fresh
// context.Background internally so EventSessionClosed publishes complete even
// though the parent ctx is already done.
func (s *service) shutdownActiveSessions(_ context.Context) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		// Intentionally background, not the parent ctx: the parent is
		// already cancelled (that's what triggered shutdown) and we still
		// want EventSessionClosed to publish so persistence hooks complete.
		s.closeByID(id, domain.SessionCloseShutdown, 0) //nolint:contextcheck // see comment above
	}
}
