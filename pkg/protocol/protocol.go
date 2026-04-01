// Package protocol provides URL-based protocol detection and media stream utilities.
package protocol

import (
	"net"
	"net/url"
	"strings"
)

// Kind classifies a stream URL into a transport category used internally by
// the ingestor to choose the right reader implementation.
type Kind string

// Kind constants classify ingest URLs; see Detect.
const (
	KindUDP     Kind = "udp"  // raw MPEG-TS over UDP (unicast or multicast)
	KindHLS     Kind = "hls"  // HLS playlist pull over HTTP/HTTPS
	KindHTTP    Kind = "http" // raw byte-stream over HTTP/HTTPS (e.g. MPEG-TS over HTTP)
	KindFile    Kind = "file" // local filesystem path
	KindRTMP    Kind = "rtmp" // RTMP / RTMPS (pull or push-listen)
	KindRTSP    Kind = "rtsp" // RTSP pull
	KindSRT     Kind = "srt"  // SRT (pull caller or push listener)
	KindS3      Kind = "s3"   // AWS S3 / S3-compatible object (MinIO, etc.)
	KindUnknown Kind = "unknown"
)

// Detect returns the protocol Kind for the given URL.
// All classification is done purely from the scheme and URL structure — the
// caller never needs to specify the protocol manually.
//
//	rtmp://...                → KindRTMP
//	rtmps://...               → KindRTMP
//	srt://...                 → KindSRT
//	s3://bucket/key           → KindS3
//	udp://...                 → KindUDP
//	rtsp:// or rtsps://...    → KindRTSP
//	http(s)://...*.m3u8       → KindHLS
//	http(s)://...             → KindHTTP
//	file:// or /absolute/path → KindFile
func Detect(rawURL string) Kind {
	u, err := url.Parse(rawURL)
	if err != nil {
		return KindUnknown
	}

	switch strings.ToLower(u.Scheme) {
	case "rtmp", "rtmps":
		return KindRTMP
	case "srt":
		return KindSRT
	case "s3":
		return KindS3
	case "udp":
		return KindUDP
	case "rtsp", "rtsps":
		return KindRTSP
	case "file":
		return KindFile
	case "http", "https":
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") ||
			strings.HasSuffix(strings.ToLower(u.Path), ".m3u") {
			return KindHLS
		}
		return KindHTTP
	case "":
		// Bare path — treat as local file.
		if strings.HasPrefix(rawURL, "/") {
			return KindFile
		}
	}

	return KindUnknown
}

// IsPushListen returns true when the URL describes a local address on which
// the server should listen for incoming encoder connections (push mode).
// This applies to RTMP and SRT when the host resolves to a wildcard or
// loopback address — meaning the URL is "our bind address", not a remote source.
//
// Examples:
//
//	rtmp://0.0.0.0:1935/live  → true  (our RTMP server)
//	srt://0.0.0.0:9999        → true  (our SRT server)
//	rtmp://server.com/live    → false (remote RTMP source to pull from)
func IsPushListen(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "rtmp" && scheme != "rtmps" && scheme != "srt" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return true // bare port like ":1935"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// DNS name → definitely a remote server, not a local bind address.
		return false
	}
	return ip.IsUnspecified() || ip.IsLoopback()
}

// IsMPEGTS returns true when data begins with the MPEG-TS sync byte 0x47.
func IsMPEGTS(data []byte) bool {
	return len(data) >= 188 && data[0] == 0x47
}

// SplitTSPackets splits a raw byte slice into 188-byte MPEG-TS packets.
// Incomplete trailing bytes are discarded.
func SplitTSPackets(data []byte) [][]byte {
	var packets [][]byte
	for i := 0; i+188 <= len(data); i += 188 {
		pkt := make([]byte, 188)
		copy(pkt, data[i:i+188])
		packets = append(packets, pkt)
	}
	return packets
}
