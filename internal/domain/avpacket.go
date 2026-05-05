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

	// AVCodecMP2 is MPEG-1 / MPEG-2 audio (Layer I, II, or III). The TS
	// stream-type values 0x03 (MPEG-1 audio) and 0x04 (MPEG-2 audio) both
	// land here — the wire format is identical at the frame level so the
	// downstream muxer doesn't need to distinguish. Common in DVB radio
	// channels relayed over IP multicast (e.g. VOH FM, BBC World Service)
	// and in legacy IPTV headends; needed so mixer:// sources pulling such
	// audio are correctly recognised instead of silently dropped.
	AVCodecMP2
)

// IsVideo reports whether the codec carries a video elementary stream.
func (c AVCodec) IsVideo() bool {
	return c == AVCodecH264 || c == AVCodecH265
}

// IsAudio reports whether the codec carries an audio elementary stream.
func (c AVCodec) IsAudio() bool {
	return c == AVCodecAAC || c == AVCodecMP2
}

// AVPacket is one decoded video access unit (Annex B H.264/H.265) or one AAC frame (ADTS in Data).
// PTSms and DTSms are presentation / decode timestamps in milliseconds (MPEG-TS / gomedia convention).
type AVPacket struct {
	Codec         AVCodec
	Data          []byte
	PTSms         uint64
	DTSms         uint64
	KeyFrame      bool
	Discontinuity bool
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
