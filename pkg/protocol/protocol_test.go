package protocol_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/open-streamer/open-streamer/pkg/protocol"
)

func TestDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want protocol.Kind
	}{
		// RTMP
		{name: "rtmp pull", url: "rtmp://server.com/live/key", want: protocol.KindRTMP},
		{name: "rtmps pull", url: "rtmps://server.com/live/key", want: protocol.KindRTMP},
		{name: "rtmp push-listen", url: "rtmp://0.0.0.0:1935/live/key", want: protocol.KindRTMP},
		// SRT
		{name: "srt pull", url: "srt://relay.example.com:9999", want: protocol.KindSRT},
		{name: "srt push-listen", url: "srt://0.0.0.0:9999?streamid=key", want: protocol.KindSRT},
		// UDP
		{name: "udp unicast", url: "udp://192.168.1.100:5000", want: protocol.KindUDP},
		{name: "udp multicast", url: "udp://239.1.1.1:5000", want: protocol.KindUDP},
		// RTSP
		{name: "rtsp", url: "rtsp://camera.local:554/stream", want: protocol.KindRTSP},
		{name: "rtsps", url: "rtsps://camera.local:554/stream", want: protocol.KindRTSP},
		// HLS
		{name: "http m3u8", url: "http://cdn.example.com/live/playlist.m3u8", want: protocol.KindHLS},
		{name: "https m3u8", url: "https://cdn.example.com/live/playlist.m3u8", want: protocol.KindHLS},
		{name: "http m3u", url: "http://cdn.example.com/live/playlist.m3u", want: protocol.KindHLS},
		{name: "uppercase M3U8 extension", url: "http://cdn.example.com/LIVE.M3U8", want: protocol.KindHLS},
		// HTTP raw stream
		{name: "http ts stream", url: "http://cdn.example.com/live.ts", want: protocol.KindHTTP},
		{name: "https stream", url: "https://cdn.example.com/live", want: protocol.KindHTTP},
		// File
		{name: "file scheme", url: "file:///recordings/source.ts", want: protocol.KindFile},
		{name: "absolute path", url: "/recordings/source.ts", want: protocol.KindFile},
		// S3
		{name: "s3 bucket key", url: "s3://my-bucket/streams/live.ts", want: protocol.KindS3},
		{name: "s3 with query", url: "s3://my-bucket/file.ts?region=ap-southeast-1", want: protocol.KindS3},
		// Unknown
		{name: "unknown scheme", url: "ftp://server/file", want: protocol.KindUnknown},
		{name: "empty url", url: "", want: protocol.KindUnknown},
		{name: "relative path no slash", url: "relative/path", want: protocol.KindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := protocol.Detect(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsPushListen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want bool
	}{
		// Push-listen addresses
		{name: "rtmp wildcard", url: "rtmp://0.0.0.0:1935/live/key", want: true},
		{name: "rtmp ipv6 wildcard", url: "rtmp://[::]:1935/live/key", want: true},
		{name: "rtmp loopback", url: "rtmp://127.0.0.1:1935/live/key", want: true},
		{name: "srt wildcard", url: "srt://0.0.0.0:9999?streamid=key", want: true},
		{name: "srt loopback", url: "srt://127.0.0.1:9999", want: true},
		// Pull addresses
		{name: "rtmp remote dns", url: "rtmp://server.example.com/live/key", want: false},
		{name: "rtmp remote ip", url: "rtmp://203.0.113.10:1935/live/key", want: false},
		{name: "srt remote", url: "srt://relay.example.com:9999", want: false},
		// Other protocols always false
		{name: "rtsp", url: "rtsp://camera/stream", want: false},
		{name: "udp", url: "udp://0.0.0.0:5000", want: false},
		{name: "http", url: "http://0.0.0.0/stream", want: false},
		{name: "bad url", url: "://bad", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := protocol.IsPushListen(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsMPEGTS(t *testing.T) {
	t.Parallel()

	syncByte := byte(0x47)
	pkt188 := make([]byte, 188) //nolint:prealloc // fixed 188-byte TS packet for table-driven cases
	pkt188[0] = syncByte

	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "valid 188-byte TS packet", data: pkt188, want: true},
		{name: "larger buffer starting with 0x47", data: append(pkt188, make([]byte, 100)...), want: true},
		{name: "wrong sync byte", data: func() []byte { b := make([]byte, 188); b[0] = 0x00; return b }(), want: false},
		{name: "too short (187 bytes)", data: make([]byte, 187), want: false},
		{name: "empty", data: []byte{}, want: false},
		{name: "nil", data: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, protocol.IsMPEGTS(tt.data))
		})
	}
}

func TestSplitTSPackets(t *testing.T) {
	t.Parallel()

	makePkt := func(id byte) []byte {
		p := make([]byte, 188)
		p[0] = 0x47
		p[1] = id
		return p
	}

	tests := []struct {
		name      string
		data      []byte
		wantCount int
	}{
		{name: "empty", data: []byte{}, wantCount: 0},
		{name: "one packet exactly", data: makePkt(1), wantCount: 1},
		{name: "two packets", data: append(makePkt(1), makePkt(2)...), wantCount: 2},
		{name: "one full + partial trailing", data: append(makePkt(1), make([]byte, 100)...), wantCount: 1},
		{name: "187 bytes (less than one packet)", data: make([]byte, 187), wantCount: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pkts := protocol.SplitTSPackets(tt.data)
			assert.Len(t, pkts, tt.wantCount)
			for _, pkt := range pkts {
				assert.Len(t, pkt, 188, "each packet must be exactly 188 bytes")
			}
		})
	}
}
