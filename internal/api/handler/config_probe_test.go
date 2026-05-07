package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// transcoder.Probe spawns a real subprocess against the path the caller
// supplies. These tests use a path that does not exist on disk so Probe
// returns an "invoke ...: ..." error — we never need a real ffmpeg binary
// to exercise the error branches of the probe handler.

const bogusFFmpegPath = "/nonexistent/ffmpeg-binary-for-tests"

func TestConfigHandler_ProbeTranscoder_InvalidJSON(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/config/transcoder/probe", bytes.NewReader([]byte("{nope")))
	w := httptest.NewRecorder()
	h.ProbeTranscoder(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfigHandler_ProbeTranscoder_BinaryUnusable(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}
	body := []byte(`{"ffmpeg_path":"` + bogusFFmpegPath + `"}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/config/transcoder/probe", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ProbeTranscoder(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "FFMPEG_UNUSABLE")
}

func TestConfigHandler_TranscoderPathChanged(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}

	cases := []struct {
		name             string
		current, propsed *domain.GlobalConfig
		want             bool
	}{
		{"both nil", nil, nil, false},
		{"current nil, proposed empty", nil, &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{}}, false},
		{"current set, proposed nil", &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/old"}}, nil, true},
		{"unchanged", &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/x"}},
			&domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/x"}}, false},
		{"changed", &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/x"}},
			&domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/y"}}, true},
		{"whitespace ignored", &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: "/x"}},
			&domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: " /x  "}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, h.transcoderPathChanged(tc.current, tc.propsed))
		})
	}
}

func TestConfigHandler_ValidateTranscoderPath_BinaryUnusable(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}
	proposed := &domain.GlobalConfig{Transcoder: &config.TranscoderConfig{FFmpegPath: bogusFFmpegPath}}
	err := h.validateTranscoderPath(t.Context(), proposed)
	require.NotNil(t, err)
	assert.Equal(t, "FFMPEG_UNUSABLE", err.code)
}

func TestConfigHandler_ValidateTranscoderPath_NilProposed(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}
	// Path defaults to empty → transcoder.Probe falls back to "ffmpeg" via
	// $PATH. On test runners without ffmpeg in $PATH this still returns an
	// FFMPEG_UNUSABLE error; either outcome (binary present and incompatible
	// or absent) hits the same handler branch we want to cover.
	err := h.validateTranscoderPath(t.Context(), nil)
	if err != nil {
		assert.Contains(t, []string{"FFMPEG_UNUSABLE", "FFMPEG_INCOMPATIBLE"}, err.code)
	}
}
