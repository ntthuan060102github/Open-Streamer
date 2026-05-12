package dash

import (
	"testing"
	"time"
)

// TestStateMachine_StartsInWaitingForPairing — initial state.
func TestStateMachine_StartsInWaitingForPairing(t *testing.T) {
	sm := NewStateMachine(3 * time.Second)
	if sm.State() != StateWaitingForPairing {
		t.Errorf("initial state = %v, want WaitingForPairing", sm.State())
	}
}

// TestStateMachine_FlushAdvancesToLive — first segment write transitions.
func TestStateMachine_FlushAdvancesToLive(t *testing.T) {
	sm := NewStateMachine(3 * time.Second)
	sm.OnFirstSegmentFlushed()
	if sm.State() != StateLive {
		t.Errorf("state after first flush = %v, want Live", sm.State())
	}
}

// TestStateMachine_CanEmitOnlyWhenPaired — within the pairing window,
// both tracks must be ready to allow flush. Single-track during the
// window must wait.
func TestStateMachine_CanEmitOnlyWhenPaired(t *testing.T) {
	now := time.Now()
	sm := NewStateMachine(3 * time.Second)
	sm.OpenPairingWindow(now)

	cases := []struct {
		name string
		v, a bool
		want bool
	}{
		{"neither ready", false, false, false},
		{"video only inside window", true, false, false},
		{"audio only inside window", false, true, false},
		{"both ready", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sm.CanEmitFirstSegment(now.Add(time.Second), tc.v, tc.a); got != tc.want {
				t.Errorf("CanEmitFirstSegment(v=%v, a=%v) = %v, want %v", tc.v, tc.a, got, tc.want)
			}
		})
	}
}

// TestStateMachine_TimeoutAllowsSingleTrack — past the pairing deadline,
// proceed with whichever single track we have.
func TestStateMachine_TimeoutAllowsSingleTrack(t *testing.T) {
	now := time.Now()
	sm := NewStateMachine(3 * time.Second)
	sm.OpenPairingWindow(now)

	past := now.Add(4 * time.Second) // past 3s deadline
	cases := []struct {
		name string
		v, a bool
		want bool
	}{
		{"neither: still no", false, false, false},
		{"video only past deadline", true, false, true},
		{"audio only past deadline", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sm.CanEmitFirstSegment(past, tc.v, tc.a); got != tc.want {
				t.Errorf("post-deadline CanEmit(v=%v, a=%v) = %v, want %v", tc.v, tc.a, got, tc.want)
			}
		})
	}
}

// TestStateMachine_OpenPairingWindowIdempotent — subsequent calls don't
// move the deadline.
func TestStateMachine_OpenPairingWindowIdempotent(t *testing.T) {
	t0 := time.Now()
	sm := NewStateMachine(3 * time.Second)
	sm.OpenPairingWindow(t0)
	firstDeadline := sm.pairingDeadline

	// Second call 1 second later: should NOT shift the deadline.
	sm.OpenPairingWindow(t0.Add(time.Second))
	if !sm.pairingDeadline.Equal(firstDeadline) {
		t.Errorf("pairingDeadline shifted: was %v, now %v", firstDeadline, sm.pairingDeadline)
	}
}

// TestStateMachine_CanEmitTrueInLive — once Live, the gate is always open.
func TestStateMachine_CanEmitTrueInLive(t *testing.T) {
	sm := NewStateMachine(3 * time.Second)
	sm.OnFirstSegmentFlushed()
	if !sm.CanEmitFirstSegment(time.Now(), false, false) {
		t.Error("Live state should always allow emit")
	}
}

// TestStateMachine_SessionBoundaryTransition — Live → SessionBoundary
// on OnSessionStart, then SessionBoundary → Live on
// OnSessionBoundaryHandled.
func TestStateMachine_SessionBoundaryTransition(t *testing.T) {
	sm := NewStateMachine(3 * time.Second)
	sm.OnFirstSegmentFlushed()

	sm.OnSessionStart()
	if sm.State() != StateSessionBoundary {
		t.Errorf("after OnSessionStart: %v, want SessionBoundary", sm.State())
	}
	sm.OnSessionBoundaryHandled()
	if sm.State() != StateLive {
		t.Errorf("after OnSessionBoundaryHandled: %v, want Live", sm.State())
	}
}

// TestStateMachine_SessionStartIgnoredInPairing — defensive: a session
// boundary in WaitingForPairing is degenerate (upstream shouldn't emit
// SessionStart=true before the first session even has data). No-op.
func TestStateMachine_SessionStartIgnoredInPairing(t *testing.T) {
	sm := NewStateMachine(3 * time.Second)
	sm.OnSessionStart()
	if sm.State() != StateWaitingForPairing {
		t.Errorf("OnSessionStart in WaitingForPairing should be no-op, got %v", sm.State())
	}
}

// TestState_String — sanity for log fields.
func TestState_String(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{StateWaitingForPairing, "waiting_for_pairing"},
		{StateLive, "live"},
		{StateSessionBoundary, "session_boundary"},
		{State(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}
