package domain

import "time"

// StreamSession is the upstream-side counterpart of PlaySession: it describes
// one continuous lifetime of media flowing from a source into the buffer hub.
// A new session is minted whenever the source identity, codec config, or
// timeline anchor changes — every fresh Open of a PacketReader, every
// Stream Manager failover, every mixer cycle restart. Within one session
// every packet carries the same SessionID; the first packet carries
// SessionStart=true so downstream consumers can latch onto the new track
// configuration without having to re-parse it.
//
// This is the wire-level analogue of `<PERIOD>` boundaries in DASH or
// `#EXT-X-DISCONTINUITY` markers in HLS — it replaces the four overloaded
// meanings of the previous `Discontinuity` flag (input switch, rebaser
// re-anchor, codec-config delta, AAC sample-rate change) with one explicit
// concept that the packager and segmenter can dispatch on.
type StreamSession struct {
	// ID monotonically increases for the lifetime of the buffer hub. Zero is
	// the sentinel "no session yet" — readers that observe SessionID==0 should
	// treat the packet as if it had no session metadata (legacy path).
	ID uint64

	// StartedAt is the wall-clock instant the session began. Downstream
	// timeline math anchors everything against this — the packager's
	// `availabilityStartTime` (DASH) or the HLS segmenter's first-segment
	// `EXT-X-PROGRAM-DATE-TIME` derives from here.
	StartedAt time.Time

	// Reason records why the session was minted. Useful for diagnostics
	// (operator dashboards, error reports) and for any future consumer that
	// wants different segmenting behaviour by reason (e.g. emit a fresh
	// `<Period>` only on Failover, keep one Period across Reconnect).
	Reason SessionStartReason

	// Video and Audio are snapshots of the track configuration observed at
	// session start. Either may be nil for video-only or audio-only streams.
	// The buffer hub does NOT populate codec init bytes on its own — the
	// ingestor's reader fills them when it has SPS/PPS / ASC on hand,
	// otherwise the consumer falls back to in-band parsing (the current
	// behaviour).
	//
	// SessionVideoConfig / SessionAudioConfig are deliberately distinct from
	// the transcoder's `VideoConfig` / `AudioConfig` (output encoder knobs).
	// These describe the inbound elementary stream as observed at session
	// start, not what we want to produce downstream.
	Video *SessionVideoConfig
	Audio *SessionAudioConfig
}

// SessionStartReason explains why a StreamSession was minted. Used for
// telemetry and for consumers that want reason-dependent segmenting
// behaviour. Stable string forms are exposed via String() for log fields.
type SessionStartReason uint8

// SessionStartReason values.
const (
	// SessionStartUnknown is the zero value. Used in tests / synthetic packets.
	SessionStartUnknown SessionStartReason = iota

	// SessionStartFresh is the very first session for this stream after the
	// pipeline starts. Equivalent to a cold boot.
	SessionStartFresh

	// SessionStartReconnect is a same-input reopen — the source URL is
	// identical, the upstream just dropped the TCP/UDP/RTP connection and
	// the ingestor reconnected. Codec config typically survives.
	SessionStartReconnect

	// SessionStartFailover is a Stream Manager input switch. The downstream
	// is now reading from a different priority-N input; codec config, FPS,
	// resolution may all be different.
	SessionStartFailover

	// SessionStartMixerCycle is a mixer rebuild (pull/mixer or coordinator/
	// abr_mixer) — one of the two upstreams reconnected and the mirror has
	// re-anchored its rebaser onto a fresh wall-clock origin.
	SessionStartMixerCycle

	// SessionStartConfigChange is an in-source codec or sample-rate change
	// reported by the reader (RTMP onMetaData delta, RTSP SDP re-OPTIONS,
	// AAC ADTS header sample-rate flip). The source stayed up, but the
	// elementary stream's track config changed.
	SessionStartConfigChange

	// SessionStartStallRecovery is fired when the worker's stall watchdog
	// observed >stallThreshold seconds of no writes (source went silent
	// while the connection / context was still alive) and a write just
	// resumed. The underlying TCP / UDP / HTTP session never dropped, so
	// the ingestor never reconnected; but downstream consumers (DASH
	// packager, HLS segmenter, RTSP / RTMP re-stream) need to treat the
	// gap as a session boundary so their accumulated state (segmenter
	// queues, normaliser anchors, mp4 timeline) resets cleanly instead of
	// emitting a giant per-sample dur or an audio-vs-video skew.
	SessionStartStallRecovery
)

// String returns the stable text form of the reason. Used in log fields
// and in event payloads so external dashboards can categorise sessions.
func (r SessionStartReason) String() string {
	switch r {
	case SessionStartUnknown:
		return "unknown"
	case SessionStartFresh:
		return "fresh"
	case SessionStartReconnect:
		return "reconnect"
	case SessionStartFailover:
		return "failover"
	case SessionStartMixerCycle:
		return "mixer_cycle"
	case SessionStartConfigChange:
		return "config_change"
	case SessionStartStallRecovery:
		return "stall_recovery"
	default:
		return "unknown"
	}
}

// SessionVideoConfig is the per-session video track configuration. Populated
// by the reader when init params are known at session start; otherwise
// zero-valued and downstream consumers fall back to in-band parsing.
type SessionVideoConfig struct {
	Codec  AVCodec
	Width  int
	Height int
	FPS    float64

	// SPS / PPS / VPS are the codec init parameter sets in Annex-B form
	// WITHOUT start codes. H.264 uses SPS+PPS; HEVC adds VPS. Empty when
	// the reader didn't see them upfront (some RTMP / mixer paths only
	// learn them on the first IDR).
	SPS []byte
	PPS []byte
	VPS []byte
}

// SessionAudioConfig is the per-session audio track configuration.
type SessionAudioConfig struct {
	Codec      AVCodec
	SampleRate int
	Channels   int

	// Profile is the AAC profile / object type (2 = LC, 5 = HE-AAC v1,
	// 29 = HE-AAC v2). Zero for non-AAC codecs.
	Profile int
}
