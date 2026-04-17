package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const (
	validationErrFmt = "validation err: %+v"
	rtmpInputA       = "rtmp://a"
)

func decodeBodyHelper(t *testing.T, code domain.StreamCode, cur *domain.Stream, exists bool, raw any) (*domain.Stream, *putValidationError) {
	t.Helper()
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/streams/"+string(code), bytes.NewReader(body))
	return decodeStreamBody(req, code, cur, exists)
}

func TestDecodeStreamBodyNew(t *testing.T) {
	body := domain.Stream{Name: "hello"}
	got, vErr := decodeBodyHelper(t, "live1", nil, false, body)
	if vErr != nil {
		t.Fatalf("unexpected validation err: %+v", vErr)
	}
	if got.Code != "live1" || got.Name != "hello" {
		t.Errorf("body=%+v", got)
	}
}

func TestDecodeStreamBodyMergesExisting(t *testing.T) {
	cur := &domain.Stream{Code: "x", Name: "old", Disabled: true}
	got, vErr := decodeBodyHelper(t, "x", cur, true, map[string]any{"name": "new"})
	if vErr != nil {
		t.Fatalf(validationErrFmt, vErr)
	}
	if got.Name != "new" {
		t.Errorf("name not overwritten: %q", got.Name)
	}
	if !got.Disabled {
		t.Error("omitted Disabled field must keep prior value")
	}
}

func TestDecodeStreamBodyURLCodeWins(t *testing.T) {
	got, vErr := decodeBodyHelper(t, "url_code", nil, false,
		map[string]any{"code": "body_code"})
	if vErr != nil {
		t.Fatalf(validationErrFmt, vErr)
	}
	if got.Code != "url_code" {
		t.Errorf("URL code must win, got %q", got.Code)
	}
}

func TestDecodeStreamBodyInvalidJSON(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/streams/x", bytes.NewReader([]byte("{bad")))
	_, vErr := decodeStreamBody(req, "x", nil, false)
	if vErr == nil || vErr.code != "INVALID_BODY" {
		t.Errorf("want INVALID_BODY, got %+v", vErr)
	}
}

func TestDecodeStreamBodyInvalidCode(t *testing.T) {
	_, vErr := decodeBodyHelper(t, "bad-code!", nil, false, map[string]any{})
	if vErr == nil || vErr.code != "INVALID_CODE" {
		t.Errorf("want INVALID_CODE, got %+v", vErr)
	}
}

func TestDecodeStreamBodyDuplicateInputs(t *testing.T) {
	body := map[string]any{
		"inputs": []map[string]any{
			{"url": rtmpInputA, "priority": 0},
			{"url": rtmpInputA, "priority": 1},
		},
	}
	_, vErr := decodeBodyHelper(t, "x", nil, false, body)
	if vErr == nil || vErr.code != "DUPLICATE_INPUT" {
		t.Errorf("want DUPLICATE_INPUT, got %+v", vErr)
	}
}

func TestDecodeStreamBodyInvalidPriority(t *testing.T) {
	body := map[string]any{
		"inputs": []map[string]any{
			{"url": rtmpInputA, "priority": 5}, // does not start at 0
		},
	}
	_, vErr := decodeBodyHelper(t, "x", nil, false, body)
	if vErr == nil || vErr.code != "INVALID_INPUT_PRIORITY" {
		t.Errorf("want INVALID_INPUT_PRIORITY, got %+v", vErr)
	}
}
