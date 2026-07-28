package session

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestBuildQARequestCarriesProfessionalSkillNames(t *testing.T) {
	rc := &qaRequestContext{
		session:                &types.Session{},
		assistantMessage:       &types.Message{ID: "assistant-1"},
		professionalSkillNames: []string{"word-docx"},
	}

	req := rc.buildQARequest()
	if len(req.ProfessionalSkillNames) != 1 || req.ProfessionalSkillNames[0] != "word-docx" {
		t.Fatalf(
			"ProfessionalSkillNames = %#v, want [word-docx]",
			req.ProfessionalSkillNames,
		)
	}

	rc.professionalSkillNames[0] = "mutated"
	if req.ProfessionalSkillNames[0] != "word-docx" {
		t.Fatalf("ProfessionalSkillNames aliases request context: %#v", req.ProfessionalSkillNames)
	}
}
