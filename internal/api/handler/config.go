package handler

import (
	"net/http"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/hwdetect"
	"github.com/samber/do/v2"
)

// publisherPorts exposes listener port configuration so the UI can build output URLs.
type publisherPorts struct {
	HTTPAddr string `json:"http_addr"` // e.g. ":8080"
	RTSPPort int    `json:"rtsp_port"` // e.g. 18554; 0 = disabled
	RTMPPort int    `json:"rtmp_port"` // e.g. 1936; 0 = disabled
	SRTPort  int    `json:"srt_port"`  // e.g. 10000; 0 = disabled
}

// configResponse is the payload returned by GET /config.
type configResponse struct {
	HWAccels           []domain.HWAccel           `json:"hwAccels"`
	VideoCodecs        []domain.VideoCodec        `json:"videoCodecs"`
	AudioCodecs        []domain.AudioCodec        `json:"audioCodecs"`
	OutputProtocols    []string                   `json:"outputProtocols"`
	StreamStatuses     []domain.StreamStatus      `json:"streamStatuses"`
	WatermarkTypes     []domain.WatermarkType     `json:"watermarkTypes"`
	WatermarkPositions []domain.WatermarkPosition `json:"watermarkPositions"`
	Ports              publisherPorts             `json:"ports"`
}

// ConfigHandler serves the GET /config endpoint.
type ConfigHandler struct {
	ports publisherPorts
}

// NewConfigHandler creates a ConfigHandler from the DI injector.
func NewConfigHandler(i do.Injector) (*ConfigHandler, error) {
	cfg := do.MustInvoke[*config.Config](i)
	return &ConfigHandler{
		ports: publisherPorts{
			HTTPAddr: cfg.Server.HTTPAddr,
			RTSPPort: cfg.Publisher.RTSP.PortMin,
			RTMPPort: cfg.Publisher.RTMP.Port,
			SRTPort:  cfg.Publisher.SRT.Port,
		},
	}, nil
}

// GetConfig returns static enum values, host-detected hardware capabilities, and publisher ports.
//
// @Summary     Get server configuration.
// @Description Returns available hardware accelerators (OS-detected), static enum lists, and publisher listener ports.
// @Tags        system
// @Produce     json
// @Success     200 {object} apidocs.ConfigData
// @Router      /config [get].
func (h *ConfigHandler) GetConfig(w http.ResponseWriter, _ *http.Request) {
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
		Ports: h.ports,
	}
	writeJSON(w, http.StatusOK, resp)
}
