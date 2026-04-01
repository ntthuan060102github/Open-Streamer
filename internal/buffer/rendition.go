package buffer

import (
	"fmt"

	"github.com/open-streamer/open-streamer/internal/domain"
)

// RenditionBufferID is the Buffer Hub id for one transcoded video profile (ABR ladder).
// Slug is always VideoTrackSlug(i) for the profile index i in the ladder.
func RenditionBufferID(code domain.StreamCode, slug string) domain.StreamCode {
	return domain.StreamCode("$r$" + string(code) + "$" + slug)
}

// VideoTrackSlug returns the stable path segment for the profile at index (0-based): track_1, track_2, ….
func VideoTrackSlug(index int) string {
	return fmt.Sprintf("track_%d", index+1)
}

// RenditionPlayout describes one ladder rung for publisher routing.
type RenditionPlayout struct {
	Slug        string
	BufferID    domain.StreamCode
	Width       int
	Height      int
	BitrateKbps int
}

// RenditionsForTranscoder returns ladder entries when transcoding is active (non-external).
func RenditionsForTranscoder(code domain.StreamCode, tc *domain.TranscoderConfig) []RenditionPlayout {
	if tc == nil || tc.Global.External {
		return nil
	}
	profiles := tc.Video.Profiles
	if len(profiles) == 0 {
		profiles = defaultVideoProfilesForABR()
	}
	out := make([]RenditionPlayout, 0, len(profiles))
	for i, p := range profiles {
		slug := VideoTrackSlug(i)
		br := p.Bitrate
		if br <= 0 {
			br = 2500
		}
		out = append(out, RenditionPlayout{
			Slug:        slug,
			BufferID:    RenditionBufferID(code, slug),
			Width:       p.Width,
			Height:      p.Height,
			BitrateKbps: br,
		})
	}
	return out
}

func defaultVideoProfilesForABR() []domain.VideoProfile {
	return []domain.VideoProfile{
		{Width: 1920, Height: 1080, Bitrate: 5000},
		{Width: 1280, Height: 720, Bitrate: 2500},
		{Width: 854, Height: 480, Bitrate: 1000},
	}
}

// BestRenditionIndex picks the highest resolution (then bitrate) for single-bitrate protocols.
func BestRenditionIndex(rends []RenditionPlayout) int {
	if len(rends) == 0 {
		return 0
	}
	best := 0
	bestScore := rends[0].Width*rends[0].Height*1000 + rends[0].BitrateKbps
	for i := 1; i < len(rends); i++ {
		sc := rends[i].Width*rends[i].Height*1000 + rends[i].BitrateKbps
		if sc > bestScore {
			bestScore = sc
			best = i
		}
	}
	return best
}

// PlaybackBufferID returns the buffer subscribers should use for single-rendition outputs
// (RTSP, RTMP play, SRT, push, DVR) — best ladder rung when transcoding, else the main stream code.
func PlaybackBufferID(code domain.StreamCode, tc *domain.TranscoderConfig) domain.StreamCode {
	rends := RenditionsForTranscoder(code, tc)
	if len(rends) == 0 {
		return code
	}
	return rends[BestRenditionIndex(rends)].BufferID
}

// BandwidthBps returns EXT-X-STREAM-INF BANDWIDTH (bits/s).
func (r RenditionPlayout) BandwidthBps() int {
	if r.BitrateKbps <= 0 {
		return 2_500_000
	}
	return r.BitrateKbps * 1000
}
