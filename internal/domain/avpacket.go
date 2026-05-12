package domain

// AVCodec identifies elementary stream codec for AVPacket payloads.
type AVCodec uint8

// AVCodec values.
const (
	AVCodecUnknown AVCodec = iota
	AVCodecH264
	AVCodecH265
	AVCodecAAC

	// AVCodecRawTSChunk is a marker codec used to carry raw MPEG-TS bytes
	// through the AVPacket-shaped pipeline without forcing a demux/remux
	// round-trip. The buffer-write step recognises this codec and writes the
	// chunk to `buffer.Packet.TS` instead of `buffer.Packet.AV`. Consumers
	// that prefer raw TS (HLS/DASH segmenters, transcoder ffmpeg-stdin pump)
	// then forward bytes verbatim — preserving PCR continuity and original
	// PIDs that a demux/remux cycle would lose.
	//
	// Used only by sources that ingest pre-muxed MPEG-TS (UDP, HLS, SRT,
	// File). RTSP / RTMP / Copy / Mixer continue to emit decoded AVPackets.
	AVCodecRawTSChunk

	// AVCodecMP2 is MPEG-1 / MPEG-2 Audio Layer I or II. Common in DVB
	// radio channels (e.g. VOH FM) and SD TV audio. TS stream_type 0x03 /
	// 0x04 both land here — the container doesn't encode Layer; we look
	// at the frame header to distinguish Layer III (= MP3, AVCodecMP3).
	AVCodecMP2

	// AVCodecMP3 is MPEG-1 / MPEG-2 Audio Layer III (the familiar "MP3").
	// Same TS stream_type as MP2 (0x03 / 0x04); detected by parsing the
	// MPEG audio frame header so the UI label and downstream codec-aware
	// consumers (mixer, transcoder configs targeting "mp3") see the right
	// codec instead of the generic mp2a fallback.
	AVCodecMP3

	// AVCodecAC3 is Dolby Digital. TS stream_type 0x81 in ATSC headends,
	// or 0x06 + AC-3 descriptor (tag 0x6A) in DVB. Common in HD broadcast
	// audio for premium / international channels.
	AVCodecAC3

	// AVCodecEAC3 is Enhanced AC-3 / Dolby Digital Plus. TS stream_type
	// 0x87 in ATSC, or 0x06 + Enhanced AC-3 descriptor (tag 0x7A) in DVB.
	// Carries 5.1+ surround for premium HD/UHD broadcast.
	AVCodecEAC3

	// AVCodecAV1 is the AOMedia AV1 video codec. TS support added in DVB
	// TS 101 154 v2.7.1 via stream_type 0x06 + registration descriptor
	// format ID "AV01" — modern UHD streaming uses this. Encode via
	// libsvtav1; ingest recognises the descriptor.
	AVCodecAV1

	// AVCodecMPEG2Video is MPEG-2 Part 2 (H.262) video. TS stream_type
	// 0x02 (also 0x01 for legacy MPEG-1 video, lumped here since the
	// downstream pipeline treats them identically). Standard for DVB SD
	// television broadcast — many older IPTV channels still carry video
	// in this codec.
	AVCodecMPEG2Video
)

// IsVideo reports whether the codec carries a video elementary stream.
//
//nolint:exhaustive // unmatched codecs (audio, marker, unknown) intentionally fall to false.
func (c AVCodec) IsVideo() bool {
	switch c {
	case AVCodecH264, AVCodecH265,
		AVCodecAV1, AVCodecMPEG2Video:
		return true
	}
	return false
}

// IsAudio reports whether the codec carries an audio elementary stream.
//
//nolint:exhaustive // unmatched codecs (video, marker, unknown) intentionally fall to false.
func (c AVCodec) IsAudio() bool {
	switch c {
	case AVCodecAAC, AVCodecMP2, AVCodecMP3,
		AVCodecAC3, AVCodecEAC3:
		return true
	}
	return false
}

// AVPacket is one decoded video access unit (Annex B H.264/H.265) or one AAC frame (ADTS in Data).
// PTSms and DTSms are presentation / decode timestamps in milliseconds (MPEG-TS / gomedia convention).
//
// No Discontinuity flag — that field was the original per-packet boundary
// cue, ambiguously meaning "first-after-reconnect" / "rebaser re-anchor" /
// "mixer cycle restart" depending on writer. Phase 3 of the refactor
// moved session-boundary signalling onto buffer.Packet.SessionStart;
// Phase 5 deleted this field. Rebaser-internal re-anchor events surface
// via Normaliser.LastDiagnostic().HardReanchored, scoped to telemetry
// (not propagated to consumers).
type AVPacket struct {
	Codec    AVCodec
	Data     []byte
	PTSms    uint64
	DTSms    uint64
	KeyFrame bool
}

// Clone returns a deep copy suitable for buffer fan-out.
func (p *AVPacket) Clone() *AVPacket {
	if p == nil {
		return nil
	}
	c := *p
	if len(p.Data) > 0 {
		c.Data = append([]byte(nil), p.Data...)
	}
	return &c
}
