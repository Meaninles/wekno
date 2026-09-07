package types

import (
	"encoding/json"
	"testing"
)

func TestCustomAgentMultimodalDefaultsPreserveExplicitChoices(t *testing.T) {
	for _, tc := range []struct {
		json         string
		image, audio bool
	}{
		{`{}`, true, true},
		{`{"image_upload_enabled":false}`, false, true},
		{`{"audio_upload_enabled":false}`, true, false},
		{`{"image_upload_enabled":false,"audio_upload_enabled":false}`, false, false},
	} {
		t.Run(tc.json, func(t *testing.T) {
			var cfg CustomAgentConfig
			if err := json.Unmarshal([]byte(tc.json), &cfg); err != nil {
				t.Fatal(err)
			}
			agent := CustomAgent{Config: cfg}
			agent.EnsureDefaults()
			if agent.Config.ImageUploadEnabled != tc.image || agent.Config.AudioUploadEnabled != tc.audio {
				t.Fatalf("unexpected upload choices: image=%v audio=%v", agent.Config.ImageUploadEnabled, agent.Config.AudioUploadEnabled)
			}
		})
	}
}
