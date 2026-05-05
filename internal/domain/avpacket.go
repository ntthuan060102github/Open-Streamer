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
)

// IsVideo reports whether the codec carries a video elementary stream.
func (c AVCodec) IsVideo() bool {
	return c == AVCodecH264 || c == AVCodecH265
}

// IsAudio reports whether the codec carries an audio elementary stream.
func (c AVCodec) IsAudio() bool {
	return c == AVCodecAAC || c == AVCodecMP2 || c == AVCodecMP3
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
