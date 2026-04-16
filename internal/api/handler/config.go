package handler

import (
	"net/http"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/hwdetect"
)

// configResponse is the payload returned by GET /config.
type configResponse struct {
	HWAccels           []domain.HWAccel           `json:"hwAccels"`
	VideoCodecs        []domain.VideoCodec        `json:"videoCodecs"`
	AudioCodecs        []domain.AudioCodec        `json:"audioCodecs"`
	OutputProtocols    []string                   `json:"outputProtocols"`
	StreamStatuses     []domain.StreamStatus      `json:"streamStatuses"`
	WatermarkTypes     []domain.WatermarkType     `json:"watermarkTypes"`
	WatermarkPositions []domain.WatermarkPosition `json:"watermarkPositions"`
}

// GetConfig returns static enum values and host-detected hardware capabilities.
//
// @Summary     Get server configuration.
// @Description Returns available hardware accelerators (OS-detected) and static enum lists for building configuration forms.
// @Tags        system
// @Produce     json
// @Success     200 {object} configResponse
// @Router      /config [get].
func GetConfig(w http.ResponseWriter, _ *http.Request) {
	resp := configResponse{
		HWAccels: hwdetect.Available(),
		VideoCodecs: []domain.VideoCodec{
			domain.VideoCodecH264,
			domain.VideoCodecH265,
			domain.VideoCodecAV1,
			domain.VideoCodecVP9,
			domain.VideoCodecCopy,
		},
		AudioCodecs: []domain.AudioCodec{
			domain.AudioCodecAAC,
			domain.AudioCodecMP3,
			domain.AudioCodecOpus,
			domain.AudioCodecAC3,
			domain.AudioCodecCopy,
		},
		OutputProtocols: []string{"hls", "dash", "rtmp", "rtsp", "srt"},
		StreamStatuses: []domain.StreamStatus{
			domain.StatusIdle,
			domain.StatusActive,
			domain.StatusDegraded,
			domain.StatusStopped,
		},
		WatermarkTypes: []domain.WatermarkType{
			domain.WatermarkTypeText,
			domain.WatermarkTypeImage,
		},
		WatermarkPositions: []domain.WatermarkPosition{
			domain.WatermarkTopLeft,
			domain.WatermarkTopRight,
			domain.WatermarkBottomLeft,
			domain.WatermarkBottomRight,
			domain.WatermarkCenter,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}
