package domain

import "testing"

// TestTranscoderConfigIsMultiOutput pins the empty-Mode → multi default
// (the user-facing contract: the toggle is per-stream, default multi).
func TestTranscoderConfigIsMultiOutput(t *testing.T) {
	cases := map[string]struct {
		cfg  *TranscoderConfig
		want bool
	}{
		"nil":                  {nil, true},
		"empty mode → multi":   {&TranscoderConfig{}, true},
		"explicit multi":       {&TranscoderConfig{Mode: TranscoderModeMulti}, true},
		"explicit per_profile": {&TranscoderConfig{Mode: TranscoderModePerProfile}, false},
		"unknown reads as not-multi (validation rejects elsewhere)": {
			&TranscoderConfig{Mode: "weird"}, false,
		},
	}
	for name, c := range cases {
		if got := c.cfg.IsMultiOutput(); got != c.want {
			t.Errorf("%s: IsMultiOutput()=%v, want %v", name, got, c.want)
		}
	}
}

// TestTranscoderConfigValidateMode rejects garbage Mode strings at
// save time so a typo can't silently flip topology.
func TestTranscoderConfigValidateMode(t *testing.T) {
	cases := map[string]struct {
		cfg     *TranscoderConfig
		wantErr bool
	}{
		"nil":         {nil, false},
		"empty mode":  {&TranscoderConfig{}, false},
		"multi":       {&TranscoderConfig{Mode: TranscoderModeMulti}, false},
		"per_profile": {&TranscoderConfig{Mode: TranscoderModePerProfile}, false},
		"typo":        {&TranscoderConfig{Mode: "per_pofile"}, true},
		"empty space": {&TranscoderConfig{Mode: " "}, true},
		"old legacy":  {&TranscoderConfig{Mode: "legacy"}, true}, // hold the line on the rename
	}
	for name, c := range cases {
		err := c.cfg.ValidateMode()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", name, err, c.wantErr)
		}
	}
}
