package dash

import "time"

// state.go — packager state machine.
//
//	┌─────────────────────┐
//	│  WaitingForPairing  │  ← packager just started or restarted
//	└─────────┬───────────┘
//	          │ both V+A first packet AND both inits ready
//	          │ OR pairingTimeout elapsed (single-track fallback)
//	          ▼
//	┌─────────────────────┐
//	│        Live         │  ← AST set, segments emitting
//	└─────────┬───────────┘
//	          │ buffer.Packet.SessionStart = true
//	          ▼
//	┌─────────────────────┐
//	│  SessionBoundary    │  ← flush + reset (no AST change)
//	└─────────┬───────────┘
//	          │ packager handled the boundary
//	          ▼
//	(back to Live)

// State is the high-level packager mode.
type State uint8

// State values.
const (
	StateWaitingForPairing State = iota
	StateLive
	StateSessionBoundary
)

// String returns a stable log-friendly label.
func (s State) String() string {
	switch s {
	case StateWaitingForPairing:
		return "waiting_for_pairing"
	case StateLive:
		return "live"
	case StateSessionBoundary:
		return "session_boundary"
	default:
		return "unknown"
	}
}

// StateMachine owns the transitions. Not goroutine-safe — held by the
// packager's run loop on a single goroutine.
type StateMachine struct {
	state           State
	pairingTimeout  time.Duration
	pairingDeadline time.Time
}

// NewStateMachine returns a state machine starting in WaitingForPairing
// with the given pairing-window timeout (3 s is the production default).
func NewStateMachine(pairingTimeout time.Duration) *StateMachine {
	if pairingTimeout <= 0 {
		pairingTimeout = 3 * time.Second
	}
	return &StateMachine{
		state:          StateWaitingForPairing,
		pairingTimeout: pairingTimeout,
	}
}

// State reports the current state.
func (sm *StateMachine) State() State { return sm.state }

// OpenPairingWindow sets the deadline `now + pairingTimeout`. Called by
// the packager when the FIRST init segment of either track first appears
// — gives the LATER track that long to also produce an init before the
// packager proceeds single-track.
//
// Subsequent calls (e.g. when the second init arrives) are no-ops — the
// window has already been opened and the deadline shouldn't move.
func (sm *StateMachine) OpenPairingWindow(now time.Time) {
	if sm.state != StateWaitingForPairing || !sm.pairingDeadline.IsZero() {
		return
	}
	sm.pairingDeadline = now.Add(sm.pairingTimeout)
}

// CanEmitFirstSegment reports whether the packager is allowed to write
// its FIRST segment. Only meaningful in WaitingForPairing; returns true
// in Live (the gate is open) and SessionBoundary (boundary flush always
// proceeds).
//
// Inputs:
//   - now: current wallclock; compared against pairingDeadline.
//   - videoReady: video init built AND queue has at least one IDR.
//   - audioReady: audio init built AND queue has at least one frame.
func (sm *StateMachine) CanEmitFirstSegment(now time.Time, videoReady, audioReady bool) bool {
	if sm.state != StateWaitingForPairing {
		return true
	}
	if videoReady && audioReady {
		return true
	}
	if !sm.pairingDeadline.IsZero() && !now.Before(sm.pairingDeadline) {
		// Timeout: proceed with whatever single track we have.
		return videoReady || audioReady
	}
	return false
}

// OnFirstSegmentFlushed advances WaitingForPairing → Live. Called by
// the packager IMMEDIATELY after the first successful segment write.
func (sm *StateMachine) OnFirstSegmentFlushed() {
	if sm.state == StateWaitingForPairing {
		sm.state = StateLive
	}
}

// OnSessionStart is called when a buffer.Packet with SessionStart=true
// reaches the run loop. Transitions Live → SessionBoundary; no-op from
// any other state (a session boundary in WaitingForPairing is a degenerate
// case the upstream shouldn't produce, but the no-op is defensive).
func (sm *StateMachine) OnSessionStart() {
	if sm.state == StateLive {
		sm.state = StateSessionBoundary
	}
}

// OnSessionBoundaryHandled is called once the packager has processed
// the boundary (flushed remaining queues, reset per-track state). Goes
// back to Live so the next packet starts a fresh sub-session.
func (sm *StateMachine) OnSessionBoundaryHandled() {
	if sm.state == StateSessionBoundary {
		sm.state = StateLive
	}
}
