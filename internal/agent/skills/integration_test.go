package skills

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestExampleSkillsIntegration tests with the actual example skills in examples/skills
func TestExampleSkillsIntegration(t *testing.T) {
	// Get the path to examples/skills relative to this test file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("Failed to get current file path")
	}

	// Navigate from internal/agent/skills to examples/skills
	skillsDir := filepath.Join(filepath.Dir(filename), "..", "..", "..", "examples", "skills")

	// Create loader
	loader := NewLoader([]string{skillsDir})

	// Discover skills
	metadata, err := loader.DiscoverSkills()
	if err != nil {
		t.Fatalf("Failed to discover skills: %v", err)
	}

	if len(metadata) == 0 {
		t.Skip("No example skills found in examples/skills directory")
	}

	t.Logf("Discovered %d example skills:", len(metadata))
	for _, m := range metadata {
		t.Logf("  - %s: %s", m.Name, truncate(m.Description, 60))
	}

	// Test loading the pdf-processing skill
	pdfSkill, err := loader.LoadSkillInstructions("pdf-processing")
	if err != nil {
		t.Fatalf("Failed to load pdf-processing skill: %v", err)
	}

	// Verify metadata
	if pdfSkill.Name != "pdf-processing" {
		t.Errorf("Expected name 'pdf-processing', got '%s'", pdfSkill.Name)
	}

	if pdfSkill.Instructions == "" {
		t.Error("Expected instructions to be non-empty")
	}

	t.Logf("PDF Processing skill instructions length: %d characters", len(pdfSkill.Instructions))

	// Test loading additional file (FORMS.md)
	formsFile, err := loader.LoadSkillFile("pdf-processing", "FORMS.md")
	if err != nil {
		t.Fatalf("Failed to load FORMS.md: %v", err)
	}

	if formsFile.Content == "" {
		t.Error("Expected FORMS.md content to be non-empty")
	}

	t.Logf("FORMS.md content length: %d characters", len(formsFile.Content))

	// Test loading script
	scriptFile, err := loader.LoadSkillFile("pdf-processing", "scripts/analyze_form.py")
	if err != nil {
		t.Fatalf("Failed to load analyze_form.py: %v", err)
	}

	if !scriptFile.IsScript {
		t.Error("analyze_form.py should be marked as script")
	}

	t.Logf("analyze_form.py content length: %d characters", len(scriptFile.Content))

	// Test list files
	files, err := loader.ListSkillFiles("pdf-processing")
	if err != nil {
		t.Fatalf("Failed to list skill files: %v", err)
	}

	t.Logf("Files in pdf-processing skill:")
	for _, f := range files {
		t.Logf("  - %s (script: %v)", f, IsScript(f))
	}
}

// TestManagerWithExampleSkills tests the Manager with example skills
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
