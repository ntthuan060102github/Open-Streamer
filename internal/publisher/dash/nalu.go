package dash

import (
	"bytes"

	"github.com/Eyevinn/mp4ff/avc"
)

// nalu.go — Annex-B ↔ AVCC byte-stream helpers.
//
// fMP4 fragments require samples in AVCC format (each NAL unit prefixed
// with a 4-byte big-endian length), while the buffer hub delivers
// Annex-B byte streams (NAL units separated by 0x00 0x00 0x00 0x01 or
// 0x00 0x00 0x01 start codes). These helpers do the conversion in both
// directions for both H.264 and H.265.

// h264AnnexBToAVCC converts an H.264 Annex-B access unit to AVCC.
// Returns nil on empty input or unparseable byte stream.
func h264AnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	avcc := avc.ConvertByteStreamToNaluSample(annexB)
	if len(avcc) > 0 {
		return avcc
	}
	// Fallback for byte streams the avc library can't parse (some
	// upstream sources produce non-canonical Annex-B): manually split
	// NALUs and prefix each with a 4-byte big-endian length.
	nalus := avc.ExtractNalusFromByteStream(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(nalus)*5)
	for _, nalu := range nalus {
		out = appendLengthPrefixedNALU(out, nalu)
	}
	return out
}

// hevcAnnexBToAVCC converts an H.265 Annex-B access unit to AVCC.
// Returns nil on empty input or no NAL units found.
func hevcAnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	nalus := splitAnnexBNALUs(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(annexB))
	for _, nalu := range nalus {
		out = appendLengthPrefixedNALU(out, nalu)
	}
	return out
}

// appendLengthPrefixedNALU writes one length-prefixed NALU into out.
func appendLengthPrefixedNALU(out, nalu []byte) []byte {
	n := len(nalu)
	out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(out, nalu...)
}

// splitAnnexBNALUs splits an Annex-B byte stream on 3- or 4-byte start
// codes. Used by hevcAnnexBToAVCC (the avc package's helpers are
// H.264-specific).
//
// The trailing-zero trim handles the case where a NAL unit's trailing
// bytes happen to be zeros that look like the start of the next NALU's
// start code prefix; we move the boundary back to exclude them.
func splitAnnexBNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1
	n := len(data)
	for i := 0; i < n-3; i++ {
		if !isAnnexBStartCodePrefix(data, i) {
			continue
		}
		// Determine start-code length (3 or 4 bytes).
		scLen := 3
		if data[i+2] == 0 {
			scLen = 4
		}
		if start >= 0 {
			end := i
			for end > start && data[end-1] == 0 {
				end--
			}
			if end > start {
				nalus = append(nalus, data[start:end])
			}
		}
		start = i + scLen
		i += scLen - 1
	}
	if start >= 0 && start < n {
		nalus = append(nalus, data[start:])
	}
	return nalus
}

// isAnnexBStartCodePrefix reports whether data[i:] begins with an
// Annex-B start code (0x00 0x00 0x01 or 0x00 0x00 0x00 0x01).
func isAnnexBStartCodePrefix(data []byte, i int) bool {
	if i+2 >= len(data) || data[i] != 0 || data[i+1] != 0 {
		return false
	}
	if data[i+2] == 1 {
		return true
	}
	return data[i+2] == 0 && i+3 < len(data) && data[i+3] == 1
}

// annexB4To3 replaces 4-byte Annex-B start codes (0x00 0x00 0x00 0x01)
// with 3-byte start codes (0x00 0x00 0x01) so that mp4ff's parameter-set
// scanners (which use the 3-byte form) work correctly on inputs that
// arrive with 4-byte codes.
func annexB4To3(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0, 0, 0, 1}, []byte{0, 0, 1})
}
