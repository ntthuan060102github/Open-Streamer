package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const yamlPath = "/config/yaml"

func TestGetConfigYAMLRoundTrips(t *testing.T) {
	rtm := &fakeRuntimeManager{cfg: &domain.GlobalConfig{
		Server:    &config.ServerConfig{HTTPAddr: ":8080"},
		Buffer:    &config.BufferConfig{Capacity: 1000},
		Publisher: &config.PublisherConfig{RTMP: config.PublisherRTMPServeConfig{Port: 1936}},
	}}
	h := &ConfigHandler{rtm: rtm}

	w := httptest.NewRecorder()
	h.GetConfigYAML(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, yamlPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != yamlContentType {
		t.Errorf("content-type=%s", got)
	}

	var back domain.GlobalConfig
	if err := yaml.Unmarshal(w.Body.Bytes(), &back); err != nil {
		t.Fatalf("response not valid YAML: %v", err)
	}
	if back.Server == nil || back.Server.HTTPAddr != ":8080" {
		t.Errorf("round-trip lost server config: %+v", back.Server)
	}
	if back.Buffer == nil || back.Buffer.Capacity != 1000 {
		t.Errorf("round-trip lost buffer config: %+v", back.Buffer)
	}
}

func TestReplaceConfigYAMLApplies(t *testing.T) {
	rtm := &fakeRuntimeManager{cfg: &domain.GlobalConfig{
		Server: &config.ServerConfig{HTTPAddr: ":8080"},
	}}
	h := &ConfigHandler{rtm: rtm}

	body := []byte(`server:
  http_addr: ":9999"
buffer:
  capacity: 2048
`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if rtm.applied == nil || rtm.applied.Server.HTTPAddr != ":9999" {
		t.Errorf("apply not called with new config: %+v", rtm.applied)
	}
	if rtm.applied.Buffer == nil || rtm.applied.Buffer.Capacity != 2048 {
		t.Errorf("buffer not propagated: %+v", rtm.applied.Buffer)
	}
}

func TestReplaceConfigYAMLFullReplaceDropsOmittedSection(t *testing.T) {
	// Existing config has Hooks. New body omits Hooks → must be nil after replace.
	rtm := &fakeRuntimeManager{cfg: &domain.GlobalConfig{
		Server: &config.ServerConfig{HTTPAddr: ":8080"},
		Hooks:  &config.HooksConfig{WorkerCount: 4},
	}}
	h := &ConfigHandler{rtm: rtm}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath,
		strings.NewReader("server:\n  http_addr: \":8080\"\n"))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if rtm.applied.Hooks != nil {
		t.Error("PUT must drop omitted sections (full replace, not merge)")
	}
}

func TestReplaceConfigYAMLEmptyBody(t *testing.T) {
	h := &ConfigHandler{rtm: &fakeRuntimeManager{cfg: &domain.GlobalConfig{}}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath,
		strings.NewReader("   \n  "))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestReplaceConfigYAMLInvalidYAML(t *testing.T) {
	h := &ConfigHandler{rtm: &fakeRuntimeManager{cfg: &domain.GlobalConfig{}}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath,
		strings.NewReader("server: [unbalanced"))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReplaceConfigYAMLUnknownFieldsRejected(t *testing.T) {
	h := &ConfigHandler{rtm: &fakeRuntimeManager{cfg: &domain.GlobalConfig{}}}
	body := strings.NewReader("server:\n  http_addr: \":80\"\n  totally_made_up_field: 42\n")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath, body)
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("typos must be caught, got status=%d", w.Code)
	}
}

func TestReplaceConfigYAMLValidationCollectsAllErrors(t *testing.T) {
	h := &ConfigHandler{rtm: &fakeRuntimeManager{cfg: &domain.GlobalConfig{}}}
	body := []byte(`server:
  http_addr: ""
buffer:
  capacity: -1
publisher:
  rtmp:
    port: 99999
hooks:
  worker_count: 0
log:
  level: trace
  format: xml
`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Code   string       `json:"code"`
			Fields []fieldError `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got.Error.Code != "VALIDATION_FAILED" {
		t.Errorf("code=%s", got.Error.Code)
	}
	have := map[string]bool{}
	for _, f := range got.Error.Fields {
		have[f.Path] = true
	}
	for _, p := range []string{
		"server.http_addr",
		"buffer.capacity",
		"publisher.rtmp.port",
		"hooks.worker_count",
		"log.level",
		"log.format",
	} {
		if !have[p] {
			t.Errorf("missing validation error for path %q (got: %+v)", p, got.Error.Fields)
		}
	}
}

func TestReplaceConfigYAMLApplyError(t *testing.T) {
	rtm := &fakeRuntimeManager{
		cfg:      &domain.GlobalConfig{},
		applyErr: errors.New("disk full"),
	}
	h := &ConfigHandler{rtm: rtm}
	body := []byte("server:\n  http_addr: \":8080\"\n")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestReplaceConfigYAMLBodyTooLarge(t *testing.T) {
	h := &ConfigHandler{rtm: &fakeRuntimeManager{cfg: &domain.GlobalConfig{}}}
	huge := bytes.Repeat([]byte("a"), maxYAMLBodyBytes+1)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, yamlPath, bytes.NewReader(huge))
	w := httptest.NewRecorder()
	h.ReplaceConfigYAML(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestValidateGlobalConfigPassesForValid(t *testing.T) {
	cfg := &domain.GlobalConfig{
		Server:    &config.ServerConfig{HTTPAddr: ":8080"},
		Buffer:    &config.BufferConfig{Capacity: 2000},
		Publisher: &config.PublisherConfig{RTMP: config.PublisherRTMPServeConfig{Port: 1936}},
		Hooks:     &config.HooksConfig{WorkerCount: 2},
		Log:       &config.LogConfig{Level: "info", Format: "json"},
	}
	if errs := validateGlobalConfig(cfg); len(errs) != 0 {
		t.Errorf("valid config flagged errors: %+v", errs)
	}
}

func TestValidateGlobalConfigCORSWildcardWithCredentials(t *testing.T) {
	cfg := &domain.GlobalConfig{
		Server: &config.ServerConfig{
			HTTPAddr: ":8080",
			CORS: config.CORSConfig{
				AllowedOrigins:   []string{"*"},
				AllowCredentials: true,
			},
		},
	}
	errs := validateGlobalConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Path, "allow_credentials") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected allow_credentials error, got %+v", errs)
	}
}

func TestValidateGlobalConfigSameDirHLSDASH(t *testing.T) {
	cfg := &domain.GlobalConfig{
		Publisher: &config.PublisherConfig{
			HLS:  config.PublisherHLSConfig{Dir: "/x"},
			DASH: config.PublisherDASHConfig{Dir: "/x"},
		},
	}
	errs := validateGlobalConfig(cfg)
	found := false
	for _, e := range errs {
		if e.Path == "publisher.dash.dir" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dash.dir collision error, got %+v", errs)
	}
}

func TestValidateGlobalConfigIngestorAddrRequiredWhenEnabled(t *testing.T) {
	cfg := &domain.GlobalConfig{
		Ingestor: &config.IngestorConfig{
			RTMPEnabled: true,
			RTMPAddr:    "",
			SRTEnabled:  true,
			SRTAddr:     "not-a-valid-addr",
		},
	}
	errs := validateGlobalConfig(cfg)
	have := map[string]bool{}
	for _, e := range errs {
		have[e.Path] = true
	}
	if !have["ingestor.rtmp_addr"] {
		t.Error("missing ingestor.rtmp_addr error")
	}
	if !have["ingestor.srt_addr"] {
		t.Error("missing ingestor.srt_addr error")
	}
}

func TestValidateGlobalConfigNil(t *testing.T) {
	errs := validateGlobalConfig(nil)
	if len(errs) != 1 {
		t.Errorf("nil config should yield exactly one error, got %+v", errs)
	}
}
