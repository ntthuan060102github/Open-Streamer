package handler

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const (
	maxYAMLBodyBytes = 1 << 20 // 1 MiB — generous for a config file, blocks accidental huge uploads
	yamlContentType  = "application/yaml"
)

// fieldError describes a single validation failure.
// It is JSON-serialised so the editor can highlight the offending key.
type fieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// GetConfigYAML returns the current GlobalConfig serialised as YAML.
// The frontend editor uses this to populate its initial buffer.
//
// @Summary     Get full server configuration as YAML
// @Description Returns the entire persisted GlobalConfig as a YAML document. Pair with PUT /config/yaml to round-trip an editor.
// @Tags        system
// @Produce     application/yaml
// @Success     200 {string} string "YAML document"
// @Router      /config/yaml [get].
func (h *ConfigHandler) GetConfigYAML(w http.ResponseWriter, _ *http.Request) {
	gcfg := h.rtm.CurrentConfig()
	out, err := yaml.Marshal(gcfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MARSHAL_FAILED", err.Error())
		return
	}
	w.Header().Set("Content-Type", yamlContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ReplaceConfigYAML replaces the entire GlobalConfig with the YAML body.
// Unlike POST /config (partial JSON merge), this performs a full replace —
// any section omitted from the body is treated as absent and the corresponding
// service is stopped. This matches what the editor sees on screen.
//
// Validation collects ALL field errors and returns them together so the editor
// can highlight every issue in one round-trip.
//
// @Summary     Replace full server configuration from YAML
// @Description Replaces the entire GlobalConfig with the request body. Validation errors are returned as a list. On success, the runtime manager diffs against the previous config and starts/stops/restarts services accordingly.
// @Tags        system
// @Accept      application/yaml
// @Produce     json
// @Param       body body string true "Full GlobalConfig YAML document"
// @Success     200 {object} map[string]any
// @Failure     400 {object} map[string]any
// @Failure     422 {object} map[string]any
// @Failure     500 {object} map[string]any
// @Router      /config/yaml [put].
func (h *ConfigHandler) ReplaceConfigYAML(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxYAMLBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %d bytes", maxYAMLBodyBytes))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "EMPTY_BODY", "YAML body is empty")
		return
	}

	// Strict decode — unknown fields are an error so typos don't silently disappear.
	var newCfg domain.GlobalConfig
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&newCfg); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_YAML", err.Error())
		return
	}

	if errs := validateGlobalConfig(&newCfg); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{
				"code":    "VALIDATION_FAILED",
				"message": fmt.Sprintf("%d field(s) failed validation", len(errs)),
				"fields":  errs,
			},
		})
		return
	}

	if err := h.rtm.Apply(r.Context(), &newCfg); err != nil {
		writeError(w, http.StatusInternalServerError, "APPLY_FAILED", err.Error())
		return
	}

	gcfg := h.rtm.CurrentConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"global_config": gcfg,
		"ports":         portsFromConfig(gcfg),
	})
}

// validateGlobalConfig performs structural checks and returns ALL failures.
// Returning the full list (rather than first-fail) lets the editor surface
// every issue at once instead of forcing the user through many round-trips.
func validateGlobalConfig(c *domain.GlobalConfig) []fieldError {
	var errs []fieldError
	if c == nil {
		return []fieldError{{Path: "", Message: "config is nil"}}
	}

	if c.Server != nil {
		errs = append(errs, validateServer(c.Server)...)
	}
	if c.Buffer != nil {
		if c.Buffer.Capacity <= 0 {
			errs = append(errs, fieldError{Path: "buffer.capacity", Message: "must be > 0"})
		}
	}
	if c.Transcoder != nil {
		if c.Transcoder.MaxWorkers < 0 {
			errs = append(errs, fieldError{Path: "transcoder.max_workers", Message: "must be >= 0"})
		}
		if c.Transcoder.MaxRestarts < 0 {
			errs = append(errs, fieldError{Path: "transcoder.max_restarts", Message: "must be >= 0"})
		}
	}
	if c.Publisher != nil {
		errs = append(errs, validatePublisher(c.Publisher)...)
	}
	if c.Hooks != nil {
		if c.Hooks.WorkerCount <= 0 {
			errs = append(errs, fieldError{Path: "hooks.worker_count", Message: "must be > 0"})
		}
	}
	if c.Log != nil {
		errs = append(errs, validateLog(c.Log)...)
	}
	if c.Ingestor != nil {
		errs = append(errs, validateIngestor(c.Ingestor)...)
	}
	return errs
}

func validateServer(s *config.ServerConfig) []fieldError {
	var errs []fieldError
	if strings.TrimSpace(s.HTTPAddr) == "" {
		errs = append(errs, fieldError{Path: "server.http_addr", Message: "required (e.g. \":8080\")"})
	} else if _, _, err := net.SplitHostPort(s.HTTPAddr); err != nil {
		errs = append(errs, fieldError{Path: "server.http_addr", Message: "invalid host:port: " + err.Error()})
	}
	if s.CORS.AllowCredentials {
		for _, o := range s.CORS.AllowedOrigins {
			if o == "*" {
				errs = append(errs, fieldError{
					Path:    "server.cors.allow_credentials",
					Message: "must be false when allowed_origins contains \"*\"",
				})
				break
			}
		}
	}
	return errs
}

func validatePublisher(p *config.PublisherConfig) []fieldError {
	var errs []fieldError
	errs = append(errs, validatePort("publisher.rtsp.port_min", p.RTSP.PortMin)...)
	errs = append(errs, validatePort("publisher.rtmp.port", p.RTMP.Port)...)
	errs = append(errs, validatePort("publisher.srt.port", p.SRT.Port)...)

	if p.SRT.LatencyMS < 0 {
		errs = append(errs, fieldError{Path: "publisher.srt.latency_ms", Message: "must be >= 0"})
	}
	if p.HLS.LiveSegmentSec < 0 {
		errs = append(errs, fieldError{Path: "publisher.hls.live_segment_sec", Message: "must be >= 0"})
	}
	if p.DASH.LiveSegmentSec < 0 {
		errs = append(errs, fieldError{Path: "publisher.dash.live_segment_sec", Message: "must be >= 0"})
	}
	if p.HLS.Dir != "" && p.DASH.Dir != "" && p.HLS.Dir == p.DASH.Dir {
		errs = append(errs, fieldError{
			Path:    "publisher.dash.dir",
			Message: "must differ from publisher.hls.dir",
		})
	}
	return errs
}

func validateIngestor(i *config.IngestorConfig) []fieldError {
	var errs []fieldError
	if i.RTMPEnabled {
		if err := validateListenAddr(i.RTMPAddr); err != "" {
			errs = append(errs, fieldError{Path: "ingestor.rtmp_addr", Message: err})
		}
	}
	if i.SRTEnabled {
		if err := validateListenAddr(i.SRTAddr); err != "" {
			errs = append(errs, fieldError{Path: "ingestor.srt_addr", Message: err})
		}
	}
	return errs
}

func validateLog(l *config.LogConfig) []fieldError {
	var errs []fieldError
	if l.Level != "" {
		switch strings.ToLower(l.Level) {
		case "debug", "info", "warn", "error":
		default:
			errs = append(errs, fieldError{Path: "log.level", Message: "must be debug|info|warn|error"})
		}
	}
	if l.Format != "" {
		switch strings.ToLower(l.Format) {
		case "text", "json":
		default:
			errs = append(errs, fieldError{Path: "log.format", Message: "must be text|json"})
		}
	}
	return errs
}

// validatePort allows 0 (disabled) or a real TCP/UDP port number.
func validatePort(path string, port int) []fieldError {
	if port == 0 || (port >= 1 && port <= 65535) {
		return nil
	}
	return []fieldError{{Path: path, Message: "must be 0 (disabled) or 1-65535"}}
}

func validateListenAddr(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return "required when service is enabled (e.g. \":1935\")"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "invalid host:port: " + err.Error()
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "port must be numeric"
	}
	_ = host
	return ""
}
