package pull

// hls_pure_test.go covers the parser + transport-error helpers that
// aren't exercised by the integration test (which itself only runs
// when MediaMTX is available locally). These are pure functions; no
// network or goroutines.

import (
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── httpStatusErr.isPermanent ───────────────────────────────────────────────

func TestHTTPStatusErr_IsPermanent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want bool
	}{
		{401, true},
		{403, true},
		{404, true},
		{410, true},
		{500, false},
		{502, false},
		{503, false},
		{200, false},
	}
	for _, tc := range cases {
		err := &httpStatusErr{url: "http://x", code: tc.code}
		assert.Equal(t, tc.want, err.isPermanent(), "code=%d", tc.code)
		assert.Contains(t, err.Error(), "HTTP")
	}
}

// ─── isPermanentTransportError ───────────────────────────────────────────────

func TestIsPermanentTransportError(t *testing.T) {
	t.Parallel()

	assert.False(t, isPermanentTransportError(nil))
	assert.False(t, isPermanentTransportError(errors.New("connection reset")))

	// errors.As-style detection on x509 types.
	assert.True(t, isPermanentTransportError(x509.UnknownAuthorityError{}))
	assert.True(t, isPermanentTransportError(x509.HostnameError{Host: "bad"}))
	assert.True(t, isPermanentTransportError(x509.CertificateInvalidError{}))

	// Substring fallback for wrapped errors.
	assert.True(t, isPermanentTransportError(errors.New("tls: handshake failure")))
	assert.True(t, isPermanentTransportError(errors.New("x509: signed by unknown authority")))
	assert.True(t, isPermanentTransportError(errors.New("dial tcp: lookup foo: no such host")))
}

// ─── parseINFDuration ────────────────────────────────────────────────────────

func TestParseINFDuration(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 4.5, parseINFDuration("4.5,title"))
	assert.Equal(t, 6.0, parseINFDuration("6.0"))
	assert.Equal(t, 0.0, parseINFDuration("garbage"))
	// Whitespace tolerance.
	assert.Equal(t, 3.0, parseINFDuration("  3.0  ,foo"))
}

// ─── parseBandwidthAttr ──────────────────────────────────────────────────────

func TestParseBandwidthAttr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attrs string
		want  uint32
	}{
		{"BANDWIDTH=5000000,RESOLUTION=1920x1080", 5_000_000},
		{"bandwidth=2500000", 2_500_000}, // case-insensitive
		{"RESOLUTION=1280x720", 0},       // missing BANDWIDTH
		{"", 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, parseBandwidthAttr(tc.attrs), "attrs=%q", tc.attrs)
	}
}

// ─── pickBestVariant ─────────────────────────────────────────────────────────

func TestPickBestVariant_HighestBandwidthWins(t *testing.T) {
	t.Parallel()
	got := pickBestVariant([]hlsVariant{
		{uri: "low.m3u8", bandwidth: 800_000},
		{uri: "high.m3u8", bandwidth: 5_000_000},
		{uri: "mid.m3u8", bandwidth: 2_500_000},
	})
	assert.Equal(t, "high.m3u8", got)
}

func TestPickBestVariant_EmptySliceReturnsEmpty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", pickBestVariant(nil))
}

// ─── resolveHLSURL ───────────────────────────────────────────────────────────

func TestResolveHLSURL_NilBaseReturnsAsIs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "seg.ts", resolveHLSURL(nil, "seg.ts"))
}

func TestResolveHLSURL_AbsoluteRefPassesThrough(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://cdn.example.com/live/")
	assert.Equal(t, "https://other.com/seg.ts",
		resolveHLSURL(base, "https://other.com/seg.ts"))
}

func TestResolveHLSURL_RelativeJoinsBase(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://cdn.example.com/live/index.m3u8")
	got := resolveHLSURL(base, "seg_001.ts")
	assert.True(t, strings.HasPrefix(got, "https://cdn.example.com/live/"))
	assert.True(t, strings.HasSuffix(got, "seg_001.ts"))
}

// ─── parseM3U8 (live media playlist) ─────────────────────────────────────────

func TestParseM3U8_LiveMediaPlaylist(t *testing.T) {
	t.Parallel()
	body := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:42
#EXTINF:5.987,
seg_42.ts
#EXTINF:6.000,
seg_43.ts
#EXT-X-DISCONTINUITY
#EXTINF:6.000,
seg_44.ts
`
	base, _ := url.Parse("https://cdn.example.com/live/playlist.m3u8")
	res, err := parseM3U8(strings.NewReader(body), base)
	require.NoError(t, err)
	require.NotNil(t, res.media)
	pl := res.media

	assert.Equal(t, uint64(42), pl.seqBase)
	assert.Equal(t, 6.0, pl.targetDuration)
	assert.False(t, pl.ended)
	require.Len(t, pl.segments, 3)
	assert.Equal(t, uint64(42), pl.segments[0].seq)
	assert.Equal(t, 5.987, pl.segments[0].duration)
	// Discontinuity flag attaches to the segment that FOLLOWS the tag.
	assert.False(t, pl.segments[1].discontinuity)
	assert.True(t, pl.segments[2].discontinuity)
	// URI resolved relative to base.
	assert.Contains(t, pl.segments[0].uri, "/live/seg_42.ts")
}

func TestParseM3U8_EndedVODPlaylist(t *testing.T) {
	t.Parallel()
	body := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:4,
seg0.ts
#EXTINF:4,
seg1.ts
#EXT-X-ENDLIST
`
	base, _ := url.Parse("https://cdn.example.com/vod/index.m3u8")
	res, err := parseM3U8(strings.NewReader(body), base)
	require.NoError(t, err)
	require.NotNil(t, res.media)
	assert.True(t, res.media.ended)
	assert.Len(t, res.media.segments, 2)
}

func TestParseM3U8_MasterPlaylistResolvesVariants(t *testing.T) {
	t.Parallel()
	body := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
high.m3u8
`
	base, _ := url.Parse("https://cdn.example.com/abr/master.m3u8")
	res, err := parseM3U8(strings.NewReader(body), base)
	require.NoError(t, err)
	require.NotNil(t, res.variants)
	require.Nil(t, res.media)
	require.Len(t, res.variants, 2)
	assert.Equal(t, uint32(800_000), res.variants[0].bandwidth)
	assert.Equal(t, uint32(5_000_000), res.variants[1].bandwidth)

	// pickBestVariant should pick the high-bw URI, resolved to absolute.
	best := pickBestVariant(res.variants)
	assert.Contains(t, best, "/abr/high.m3u8")
}

func TestParseM3U8_BlankAndCommentsAreIgnored(t *testing.T) {
	t.Parallel()
	body := `#EXTM3U

#EXT-X-VERSION:3

# this is not a tag we recognise
#EXT-X-TARGETDURATION:2
#EXTINF:2,
a.ts
`
	base, _ := url.Parse("https://x/")
	res, err := parseM3U8(strings.NewReader(body), base)
	require.NoError(t, err)
	require.NotNil(t, res.media)
	require.Len(t, res.media.segments, 1)
}

// ─── fetchResult.isMaster ────────────────────────────────────────────────────

func TestFetchResult_IsMaster(t *testing.T) {
	t.Parallel()
	master := &fetchResult{variants: []hlsVariant{{uri: "v.m3u8", bandwidth: 1}}}
	media := &fetchResult{media: &hlsMediaPlaylist{}}
	assert.True(t, master.isMaster())
	assert.False(t, media.isMaster())
}

// ─── isMediaTag ──────────────────────────────────────────────────────────────

func TestIsMediaTag(t *testing.T) {
	t.Parallel()
	for _, l := range []string{
		"#EXT-X-TARGETDURATION:6",
		"#EXT-X-MEDIA-SEQUENCE:42",
		"#EXTINF:5.5,",
		"#EXT-X-ENDLIST",
	} {
		assert.True(t, isMediaTag(l), "%q should be a media tag", l)
	}
	for _, l := range []string{
		"#EXT-X-STREAM-INF:BANDWIDTH=1000",
		"#EXTM3U",
		"some.ts",
	} {
		assert.False(t, isMediaTag(l), "%q should not be a media tag", l)
	}
}
