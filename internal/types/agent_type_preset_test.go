package types

import "testing"

func TestKnowledgeQAPresetDisablesSkillsByDefault(t *testing.T) {
	if err := LoadAgentTypePresetsConfig("../../config"); err != nil {
		t.Fatalf("load agent type presets: %v", err)
	}

	preset := GetAgentTypePreset("knowledge-qa")
	if preset == nil || preset.Config == nil {
		t.Fatal("knowledge-qa preset is missing")
	}
	if preset.Config.LightweightSkillsSelectionMode != "none" {
		t.Fatalf("lightweight mode = %q, want none", preset.Config.LightweightSkillsSelectionMode)
	}
	if preset.Config.ProfessionalSkillsSelectionMode != "none" {
		t.Fatalf("professional mode = %q, want none", preset.Config.ProfessionalSkillsSelectionMode)
	}
}
