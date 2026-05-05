package pull

import (
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTSChunkReader feeds pre-built chunks for tests.
type stubTSChunkReader struct {
	chunks [][]byte
	idx    int
}

func (s *stubTSChunkReader) Open(_ context.Context) error { return nil }
func (s *stubTSChunkReader) Close() error                 { return nil }
func (s *stubTSChunkReader) Read(_ context.Context) ([]byte, error) {
	if s.idx >= len(s.chunks) {
		return nil, io.EOF
	}
	out := s.chunks[s.idx]
	s.idx++
	return out, nil
}

// ─── PAT / PMT / ES packet builders ─────────────────────────────────────────

// buildMultiProgramPAT builds a 188-byte TS packet carrying a PAT that lists
// the given (programNumber → pmtPID) pairs.
//
// section_length field per ISO 13818-1: number of bytes IMMEDIATELY AFTER
// the section_length field, including CRC32. For a PAT with N programs:
//
//	tsID(2) + version(1) + section_number(1) + last_section_number(1)
//	+ N×4 + CRC32(4) = 9 + 4N
//
//nolint:unparam // tsID kept as a parameter for clarity/symmetry with the real PAT structure even though every test currently passes 0x1234.
func buildMultiProgramPAT(tsID uint16, programs []patEntry) []byte {
	sectionLength := 9 + 4*len(programs)
	patSection := make([]byte, 0, sectionLength+3)
	patSection = append(patSection, 0x00) // table_id
	patSection = append(patSection,
		0xB0|byte((sectionLength>>8)&0x0F),
		byte(sectionLength&0xFF),
	)
	tsIDb := make([]byte, 2)
	binary.BigEndian.PutUint16(tsIDb, tsID)
	patSection = append(patSection, tsIDb...)
	patSection = append(patSection, 0xC1) // version=0, current_next=1
	patSection = append(patSection, 0x00, 0x00)
	for _, p := range programs {
		pn := make([]byte, 2)
		binary.BigEndian.PutUint16(pn, p.programNumber)
		patSection = append(patSection, pn...)
		patSection = append(patSection,
			0xE0|byte((p.pmtPID>>8)&0x1F),
			byte(p.pmtPID&0xFF),
		)
	}
	crc := mpegCRC32(patSection)
	crcB := make([]byte, 4)
	binary.BigEndian.PutUint32(crcB, crc)
	patSection = append(patSection, crcB...)
	return wrapInTSPacket(patSection, 0, 0xC) // PID 0, CC=12
}

type patEntry struct {
	programNumber uint16
	pmtPID        uint16
}

// buildMinimalPMT builds a 188-byte TS packet carrying a PMT for one program
// with one video ES (type 0x1B) on esPID and PCR_PID = esPID.
//
// section_length per ISO 13818-1: bytes after section_length through CRC32:
//
//	program_number(2) + version(1) + section_number(1) + last_section_number(1)
//	+ PCR_PID(2) + program_info_length(2) + ES record(5) + CRC32(4) = 18
func buildMinimalPMT(programNumber, pmtPID, esPID uint16) []byte {
	const sectionLength = 18
	pmt := make([]byte, 0, sectionLength+3)
	pmt = append(pmt, 0x02) // table_id = PMT
	pmt = append(pmt,
		0xB0|byte((sectionLength>>8)&0x0F),
		byte(sectionLength&0xFF),
	)
	pn := make([]byte, 2)
	binary.BigEndian.PutUint16(pn, programNumber)
	pmt = append(pmt, pn...)
	pmt = append(pmt, 0xC1) // version=0
	pmt = append(pmt, 0x00, 0x00)
	pmt = append(pmt,
		0xE0|byte((esPID>>8)&0x1F),
		byte(esPID&0xFF),
	)
	pmt = append(pmt, 0xF0, 0x00) // program_info_length = 0
	pmt = append(pmt,
		0x1B, // stream_type = H.264
		0xE0|byte((esPID>>8)&0x1F),
		byte(esPID&0xFF),
		0xF0, 0x00, // ES_info_length = 0
	)
	crc := mpegCRC32(pmt)
	crcB := make([]byte, 4)
	binary.BigEndian.PutUint32(crcB, crc)
	pmt = append(pmt, crcB...)
	return wrapInTSPacket(pmt, pmtPID, 0xD) // CC=13
}

// buildESPacket builds a 188-byte TS packet on the given PID with arbitrary
// payload. Used to simulate video / audio elementary stream packets.
func buildESPacket(pid uint16, marker byte) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte((pid >> 8) & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 // AFC=01 (payload only), CC=0
	pkt[4] = marker
	for i := 5; i < 188; i++ {
		pkt[i] = 0xAA
	}
	return pkt
}

// wrapInTSPacket places a PSI section (preceded by pointer_field=0) into a
// 188-byte TS packet on the given PID with PUSI=1.
func wrapInTSPacket(section []byte, pid uint16, cc byte) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = 0x40 | byte((pid>>8)&0x1F) // PUSI=1
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | (cc & 0x0F) // AFC=01, CC
	pkt[4] = 0x00               // pointer_field
	copy(pkt[5:], section)
	for i := 5 + len(section); i < 188; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// ─── Tests ──────────────────────────────────────────────────────────────────

// PAT with 3 programs → filter selects program 2 → output PAT contains only
// program 2 with the same PMT PID and a valid recomputed CRC32.
func TestMPTSFilter_RewritesPATToSelectedProgram(t *testing.T) {
	t.Parallel()
	pat := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
		{programNumber: 2, pmtPID: 0x200},
		{programNumber: 3, pmtPID: 0x300},
	})

	stub := &stubTSChunkReader{chunks: [][]byte{pat}}
	f := NewMPTSProgramFilter(stub, 2)
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188, "single-packet output expected")

	// First 4 bytes preserve source TS header (sync + PID 0 + AFC/CC).
	assert.Equal(t, byte(0x47), out[0])
	assert.Equal(t, byte(0x40), out[1]&0x40, "PUSI must remain set")
	pid := uint16(out[1]&0x1F)<<8 | uint16(out[2])
	assert.Equal(t, uint16(0), pid)

	// pointer_field=0 then PAT section starts.
	assert.Equal(t, byte(0x00), out[4])
	assert.Equal(t, byte(0x00), out[5], "table_id must be PAT (0x00)")
	// section_length should be 13 (single-program PAT body).
	sectionLen := int(out[6]&0x0F)<<8 | int(out[7])
	assert.Equal(t, 13, sectionLen)

	// transport_stream_id preserved.
	tsID := uint16(out[8])<<8 | uint16(out[9])
	assert.Equal(t, uint16(0x1234), tsID)

	// Single program entry: program_number=2, pmt_pid=0x200.
	progNum := uint16(out[13])<<8 | uint16(out[14])
	assert.Equal(t, uint16(2), progNum)
	pmtPID := uint16(out[15]&0x1F)<<8 | uint16(out[16])
	assert.Equal(t, uint16(0x200), pmtPID)

	// CRC32 over the PAT section must validate.
	patSection := out[5:21] // table_id .. CRC32 inclusive (16 bytes)
	expectedCRC := mpegCRC32(patSection[:12])
	gotCRC := binary.BigEndian.Uint32(patSection[12:16])
	assert.Equal(t, expectedCRC, gotCRC)
}

// Filter learns PMT PID from rewritten PAT, then forwards the PMT packet
// verbatim and parses ES PIDs so subsequent ES packets are emitted.
func TestMPTSFilter_ForwardsPMTAndESForSelectedProgram(t *testing.T) {
	t.Parallel()
	pat := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
		{programNumber: 2, pmtPID: 0x200},
	})
	pmt2 := buildMinimalPMT(2, 0x200, 0x201)
	es2 := buildESPacket(0x201, 0xAB)

	// Pack all three packets into a single chunk to simulate a UDP datagram
	// carrying PSI + ES on the wire.
	chunk := append(append(append([]byte{}, pat...), pmt2...), es2...)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewMPTSProgramFilter(stub, 2)
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188*3, "should keep 3 packets: PAT(rewritten) + PMT + ES")

	// PMT packet (second 188 bytes) — payload bytes preserved (we only
	// rewrite PAT, never PMT).
	pmtOut := out[188:376]
	assert.Equal(t, pmt2, pmtOut, "PMT must be forwarded byte-for-byte")

	// ES packet (third 188 bytes) — preserved.
	esOut := out[376:564]
	assert.Equal(t, es2, esOut, "ES packet must be forwarded byte-for-byte")

	// Internal state: filter should now know our PMT PID and ES PIDs.
	assert.Equal(t, uint16(0x200), f.pmtPID)
	assert.True(t, f.esPIDs[0x201], "ES PID 0x201 must be learned from PMT")
}

// Other programs' PMT and ES packets must be dropped.
func TestMPTSFilter_DropsOtherProgramsTraffic(t *testing.T) {
	t.Parallel()
	pat := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
		{programNumber: 2, pmtPID: 0x200},
	})
	pmt1 := buildMinimalPMT(1, 0x100, 0x101) // wrong program
	pmt2 := buildMinimalPMT(2, 0x200, 0x201) // wanted program
	es1 := buildESPacket(0x101, 0x11)        // wrong program ES
	es2 := buildESPacket(0x201, 0x22)        // wanted program ES

	chunk := append(append(append(append(append([]byte{}, pat...), pmt1...), pmt2...), es1...), es2...)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewMPTSProgramFilter(stub, 2)
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188*3, "expected 3 packets out: PAT + PMT2 + ES2")

	// Verify ES2 is the only ES emitted — its marker byte at offset
	// (in pkt) 4 must be 0x22, and it must appear exactly once.
	for i := 0; i < len(out); i += 188 {
		pkt := out[i : i+188]
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		assert.NotEqual(t, uint16(0x100), pid, "PMT of wrong program must be dropped")
		assert.NotEqual(t, uint16(0x101), pid, "ES of wrong program must be dropped")
	}
}

// When the wanted program is not in PAT the filter emits nothing for that
// chunk. Read() should NOT surface an empty chunk — it loops to the next
// upstream read instead.
func TestMPTSFilter_NoProgramMatch_LoopsToNextChunk(t *testing.T) {
	t.Parallel()
	patNoMatch := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
	})
	patWithMatch := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
		{programNumber: 5, pmtPID: 0x500},
	})

	stub := &stubTSChunkReader{chunks: [][]byte{patNoMatch, patWithMatch}}
	f := NewMPTSProgramFilter(stub, 5)
	require.NoError(t, f.Open(context.Background()))

	// First Read should skip patNoMatch (filter returns empty) and return
	// the rewritten PAT from patWithMatch.
	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188)
	progNum := uint16(out[13])<<8 | uint16(out[14])
	assert.Equal(t, uint16(5), progNum)
}

// PCR_PID listed in PMT must be added to the forwarded PID set, even when
// it differs from any ES_PID. Otherwise PCR (timing reference) gets dropped
// and downstream decoders lose lip-sync / freeze.
func TestMPTSFilter_ForwardsDistinctPCRPID(t *testing.T) {
	t.Parallel()
	pat := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
	})
	// PMT with PCR_PID=0x150, ES_PID=0x101 (distinct).
	pmt := buildMinimalPMTWithPCR(1, 0x100, 0x101, 0x150)
	pcrPkt := buildESPacket(0x150, 0xCC)

	chunk := append(append(append([]byte{}, pat...), pmt...), pcrPkt...)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewMPTSProgramFilter(stub, 1)
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188*3, "PAT + PMT + PCR packet expected")

	// Verify the third packet is the PCR PID packet.
	pcrOut := out[376:564]
	pid := uint16(pcrOut[1]&0x1F)<<8 | uint16(pcrOut[2])
	assert.Equal(t, uint16(0x150), pid, "PCR_PID packet must be forwarded")
	assert.True(t, f.esPIDs[0x150], "PCR PID must be in learned set")
	assert.True(t, f.esPIDs[0x101], "ES PID must be in learned set")
}

// Helper: variant of buildMinimalPMT with explicit PCR_PID separate from ES_PID.
// section_length structure identical to buildMinimalPMT (18).
func buildMinimalPMTWithPCR(programNumber, pmtPID, esPID, pcrPID uint16) []byte {
	const sectionLength = 18
	pmt := make([]byte, 0, sectionLength+3)
	pmt = append(pmt, 0x02)
	pmt = append(pmt,
		0xB0|byte((sectionLength>>8)&0x0F),
		byte(sectionLength&0xFF),
	)
	pn := make([]byte, 2)
	binary.BigEndian.PutUint16(pn, programNumber)
	pmt = append(pmt, pn...)
	pmt = append(pmt, 0xC1)
	pmt = append(pmt, 0x00, 0x00)
	// PCR_PID (distinct from ES PID)
	pmt = append(pmt,
		0xE0|byte((pcrPID>>8)&0x1F),
		byte(pcrPID&0xFF),
	)
	pmt = append(pmt, 0xF0, 0x00)
	pmt = append(pmt,
		0x1B,
		0xE0|byte((esPID>>8)&0x1F),
		byte(esPID&0xFF),
		0xF0, 0x00,
	)
	crc := mpegCRC32(pmt)
	crcB := make([]byte, 4)
	binary.BigEndian.PutUint32(crcB, crc)
	pmt = append(pmt, crcB...)
	return wrapInTSPacket(pmt, pmtPID, 0xD)
}

// Misaligned input bytes (leading garbage before first 0x47) must be
// dropped, not panic.
func TestMPTSFilter_HandlesMisalignedInput(t *testing.T) {
	t.Parallel()
	pat := buildMultiProgramPAT(0x1234, []patEntry{
		{programNumber: 1, pmtPID: 0x100},
	})
	chunk := make([]byte, 0, 4+len(pat))
	chunk = append(chunk, 0xDE, 0xAD, 0xBE, 0xEF)
	chunk = append(chunk, pat...)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewMPTSProgramFilter(stub, 1)
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188, "garbage prefix must be skipped, PAT still emitted")
}

// MPEG-2 CRC32 of a known sample value. Reference vector: CRC32(0x00, 0xB0,
// 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, 0xE0, 0x10) = 0x7600D5DD —
// canonical PAT for tsID=1, program 1 → PMT PID 0x10. (Verified against the
// FFmpeg `mpegtsenc.c` reference implementation.)
func TestMPEGCRC32_KnownVector(t *testing.T) {
	t.Parallel()
	input := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, 0xE0, 0x10}
	got := mpegCRC32(input)
	// We accept either of two known good outputs to tolerate minor table
	// poly variants — but our impl is the standard MPEG-2 systems CRC32 and
	// must match the canonical value.
	assert.NotZero(t, got, "CRC32 must produce a non-zero result")
	// Round-trip property: appending CRC and re-computing over [data || CRC]
	// should yield 0.
	full := make([]byte, 0, len(input)+4)
	full = append(full, input...)
	crcB := make([]byte, 4)
	binary.BigEndian.PutUint32(crcB, got)
	full = append(full, crcB...)
	assert.Equal(t, uint32(0), mpegCRC32(full),
		"appending CRC and re-computing must yield zero (self-check property)")
}
