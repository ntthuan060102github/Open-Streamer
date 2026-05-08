package pull

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindH264SPSPPSInNALUs_FindsBoth(t *testing.T) {
	t.Parallel()
	sps := []byte{0x67, 0x4d, 0x40, 0x28} // NAL type 7
	pps := []byte{0x68, 0xee, 0x3c, 0x80} // NAL type 8
	idr := []byte{0x65, 0x88, 0x80, 0x40} // NAL type 5

	gotSPS, gotPPS := findH264SPSPPSInNALUs([][]byte{sps, pps, idr})
	require.Equal(t, sps, gotSPS, "must locate SPS by NAL type 7")
	require.Equal(t, pps, gotPPS, "must locate PPS by NAL type 8")
}

func TestFindH264SPSPPSInNALUs_AbsentReturnsNil(t *testing.T) {
	t.Parallel()
	// Slice-only IDR — no SPS / PPS NALUs.
	idr := []byte{0x65, 0x88, 0x80, 0x40}
	sps, pps := findH264SPSPPSInNALUs([][]byte{idr})
	require.Nil(t, sps)
	require.Nil(t, pps)
}

func TestFindH264SPSPPSInNALUs_SkipsEmptyAndIgnoresOtherTypes(t *testing.T) {
	t.Parallel()
	sps := []byte{0x67, 0x4d}
	au := [][]byte{
		{},                 // empty — skip without panic
		{0x06, 0x00},       // SEI (type 6) — ignored
		sps,                // SPS
		{0x41, 0x9b, 0x40}, // P-slice (type 1) — ignored
	}
	gotSPS, gotPPS := findH264SPSPPSInNALUs(au)
	require.Equal(t, sps, gotSPS)
	require.Nil(t, gotPPS)
}

func TestFindH264SPSPPSInNALUs_TakesFirstWhenDuplicates(t *testing.T) {
	t.Parallel()
	// If the source includes two SPS NALUs (rare, but legal), the first
	// one wins — matches the cache-update-once-then-skip semantic the
	// caller relies on.
	first := []byte{0x67, 0x4d, 0x40, 0x28}
	second := []byte{0x67, 0x42, 0xc0, 0x1e}
	gotSPS, _ := findH264SPSPPSInNALUs([][]byte{first, second})
	require.Equal(t, first, gotSPS)
}

func TestFindH265VPSSPSPPSInNALUs_FindsAll(t *testing.T) {
	t.Parallel()
	// HEVC NAL header: bits 1..6 of first byte = type. Construct one of
	// each with type 32 (VPS), 33 (SPS), 34 (PPS) and confirm the
	// finder picks them out without confusion. Layer + temporal-id
	// bits are zero so the first byte is just (type<<1).
	vps := []byte{32 << 1, 0x00, 0x01}
	sps := []byte{33 << 1, 0x00, 0x02}
	pps := []byte{34 << 1, 0x00, 0x03}
	idr := []byte{19 << 1, 0x00, 0x04} // IDR_W_RADL = type 19

	gotV, gotS, gotP := findH265VPSSPSPPSInNALUs([][]byte{vps, sps, pps, idr})
	require.Equal(t, vps, gotV)
	require.Equal(t, sps, gotS)
	require.Equal(t, pps, gotP)
}

func TestFindH265VPSSPSPPSInNALUs_PartialReturnsRest(t *testing.T) {
	t.Parallel()
	// Source emits only VPS + IDR (encoder bug or muxer pruning).
	// The finder must still return what's there without confusing types.
	vps := []byte{32 << 1, 0x00}
	idr := []byte{19 << 1, 0x00}

	gotV, gotS, gotP := findH265VPSSPSPPSInNALUs([][]byte{vps, idr})
	require.Equal(t, vps, gotV)
	require.Nil(t, gotS)
	require.Nil(t, gotP)
}
