package transcoder

import "strings"

// sarBSFArg returns the `-bsf:v` fragment that stamps the given SAR onto
// the encoded bitstream via the h264_metadata or hevc_metadata bitstream
// filter. Returns ("", false) when the BSF for this encoder isn't
// available in the supplied probe set — caller must fall back to a
// setsar entry in the `-vf` chain.
//
// Why bitstream filter over `-vf setsar`: setsar is CPU-only (no
// setsar_cuda / setsar_vaapi exists). On a GPU pipeline that means
// FFmpeg auto-inserts hwdownload + hwupload around setsar to bridge
// CUDA frames through CPU, costing one full frame round-trip across
// PCIe per access unit. The BSF runs AFTER the encoder on the encoded
// bitstream (a few bytes per SPS NAL, once per GOP), with zero impact
// on frame pipeline.
//
// Inputs:
//   - sar:       value from VideoProfile.SAR, "W:H" syntax (e.g. "1:1",
//     "8:9"). Empty disables both paths.
//   - encoder:   resolved video encoder name (h264_nvenc, libx264,
//     hevc_nvenc, libx265, …). Codecs that aren't H.264/H.265
//     have no metadata BSF and return ("", false).
//   - bsfs:      ProbeResult.BSFs (or nil). Treats nil as "nothing
//     detected" so zero-value Service falls back to setsar
//     without any feature flag plumbing.
//
// The BSF expects rational notation "W/N", so we translate the schema's
// "W:H" form. Setting sample_aspect_ratio writes both
// aspect_ratio_info_present_flag=1 and the value into the SPS VUI block;
// players treat the output identically to a setsar=W:H filtered stream.
func sarBSFArg(sar, encoder string, bsfs map[string]bool) (string, bool) {
	if sar == "" {
		return "", false
	}
	enc := strings.ToLower(encoder)
	var bsfName string
	switch {
	case strings.Contains(enc, "hevc"), strings.Contains(enc, "265"):
		bsfName = "hevc_metadata"
	case strings.Contains(enc, "264"):
		bsfName = "h264_metadata"
	default:
		return "", false
	}
	if !bsfs[bsfName] {
		return "", false
	}
	return bsfName + "=sample_aspect_ratio=" + strings.ReplaceAll(sar, ":", "/"), true
}
