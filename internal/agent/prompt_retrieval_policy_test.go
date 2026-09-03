package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func loadPromptTemplateFileForPolicyTest(t *testing.T, name string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve prompt policy test location")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "config", "prompt_templates", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func loadAgentPromptTemplateForPolicyTest(t *testing.T) string {
	t.Helper()
	return loadPromptTemplateFileForPolicyTest(t, "agent_system_prompt.yaml")
}

func promptTemplateSection(t *testing.T, content, start, end string) string {
	t.Helper()
	startAt := strings.Index(content, start)
	if startAt < 0 {
		t.Fatalf("prompt section start %q not found", start)
	}
	endAt := strings.Index(content[startAt+len(start):], end)
	if endAt < 0 {
		t.Fatalf("prompt section end %q not found", end)
	}
	return content[startAt : startAt+len(start)+endAt]
}

func TestProgressiveRAGPromptRoutesByEvidenceNeed(t *testing.T) {
	content := loadAgentPromptTemplateForPolicyTest(t)
	section := promptTemplateSection(t, content, `  - id: "progressive_rag_agent"`, `  - id: "table_analyst"`)

	for _, required := range []string{
		"silently evaluate the user's semantic request",
		"If the request is conversation-only",
		"answer directly without retrieval or tools",
		"If the request needs external or domain evidence",
		"A domain question still retrieves when it needs source evidence",
		"mixed requests where even one requested claim needs document/domain evidence or an external citation",
		"Asking only to quote or attribute the user's own messages does not retrieve",
		"atomic object/field/value/modality/source propositions",
		"unknown and explicitly pending as different classes",
		"Assistant text, retrieved schemas, examples and placeholders do not populate a case",
		"Chat wording and structured representations remain dialogue content",
		"semantic actor, action, object, destination and scope",
		"Never say you searched, retrieved, read, verified, saved, sent, updated",
		"Follow an explicit output-language request in the current turn",
		"Never expose intent classification, chain-of-thought, self-talk, tool planning, or process narration",
		"Include each requested fact, boundary, source, or external rule once",
		"Choose the single retrieval method most likely to answer the request",
		"Evidence Sufficiency",
		"need not be fetched again",
		"Absence from one result is not proof that the source lacks the fact",
		"navigation/search result without a canonical citation handle is not citable",
		"bound or selected knowledge source only makes retrieval available",
		"external statement, require entailment from a retrieved claim-bearing fragment",
		"recompute from the newest active user facts",
		"Turn-Local Evidence Invariant",
		"current turn already contains a successful retrieval result",
		"next action is one focused retrieval call rather than a prose answer",
		"Do not transition directly from intent assessment to answer generation",
		"for each document or external-domain claim, identify the current-turn retrieval result",
		"does not establish their order, transition criteria, workflow, or completion",
		"proposed, intended, requested, or planned change remains non-completed",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("progressive RAG prompt is missing %q", required)
		}
	}
	if strings.Contains(section, "Otherwise, proceed to retrieval") {
		t.Error("progressive RAG prompt still contains the unconditional retrieval branch")
	}
	for _, forbidden := range []string{
		"Mandatory Deep Read",
		"Whenever grep_chunks or knowledge_search returns matches, you **MUST** read full content",
	} {
		if strings.Contains(section, forbidden) {
			t.Errorf("progressive RAG prompt still contains unconditional deep-read guidance %q", forbidden)
		}
	}
}

func TestGeneralAgentPromptDoesNotInventFileIntent(t *testing.T) {
	content := loadAgentPromptTemplateForPolicyTest(t)
	section := promptTemplateSection(t, content, `  - id: "general_claude_agent"`, `  - id: "knowledge_base_manager_agent"`)

	if !strings.Contains(section, "only when durable/downloadable bytes are part of the requested outcome") {
		t.Error("general agent prompt does not require semantic file-delivery intent")
	}
	if strings.Contains(section, "when a file is the best deliverable") {
		t.Error("general agent prompt still lets the model invent artifact intent")
	}
	for _, required := range []string{
		"counts and outcomes (including zero)",
		"pragmatic reasonableness of the question",
		"Use only user-authored facts and real current-turn evidence in drafts",
		"honor requested count/form",
		"Include each requested fact, boundary, source, or external rule once",
		"role duties, contact routes, commitments",
		"Never claim that an operation was performed or did not occur",
		"permission and scope information, not an audit record",
		"positive or negative outcome needs its own source",
		"Keep knowledge synthesis inside the retrieved evidence boundary",
		"recompute it from the newest active user facts",
		"operation boundary remains active",
		"would be incomplete without durable bytes",
		"never a phrase match",
		"does not create a new task",
		"without inventing or guessing the ID",
		"JSON/YAML/Markdown/code representation is chat content",
		"Never create a file merely to make artifact registration applicable",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("general agent prompt is missing %q", required)
		}
	}
}

func TestQuickAnswerPromptChecksClaimCoverageAcrossEvidenceSet(t *testing.T) {
	content := loadPromptTemplateFileForPolicyTest(t, "system_prompt.yaml")
	section := promptTemplateSection(t, content, `  - id: "default_kb"`, `  - id: "expert_assistant"`)

	for _, required := range []string{
		"contexts as an evidence set",
		"check every requested claim",
		"Absence from one passage does not prove absence from the source",
		"complete supplied evidence set",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("quick-answer prompt is missing %q", required)
		}
	}
}

func TestDialogueStateIntentPromptIsGenericAndTerminal(t *testing.T) {
	content := loadPromptTemplateFileForPolicyTest(t, "intent_prompts.yaml")
	section := promptTemplateSection(t, content, `  - id: "conversation_state"`, `  - id: "kb_search"`)

	for _, required := range []string{
		"Only exact user-authored messages are factual sources",
		"Decompose compound statements into atomic propositions",
		"retire only incompatible propositions",
		"Missing information remains unknown or pending",
		"A count or outcome (including zero)",
		"Do not infer lifecycle from conversational plausibility",
		"Drafts, plans, templates, and sample text",
		"honor the requested count/form",
		"include each requested fact, boundary, source, or external rule once",
		"does not create a new task/object",
		"quote the user text without an ID",
		"Preserve the exact actor, action, object, destination, modality, and turn scope",
		"operation boundary remains active",
		"Never claim that you searched, retrieved, read, verified, saved, sent, updated",
		"current user explicitly requests a different output language",
		"Do not expose intent analysis, chain-of-thought, self-talk",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("conversation-state prompt is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"case_id", "required_claim", "reference_answer", "采购", "培训", "茅台",
	} {
		if strings.Contains(strings.ToLower(section), strings.ToLower(forbidden)) {
			t.Errorf("conversation-state prompt contains scenario/Eval term %q", forbidden)
		}
	}
	if strings.Contains(content, "ALWAYS respond in {{language}}") {
		t.Error("intent prompts still override an explicit current-turn language request")
	}
}

func TestDefaultRewriteSeparatesSemanticIntentFromEvidenceNeed(t *testing.T) {
	content := loadPromptTemplateFileForPolicyTest(t, "rewrite.yaml")
	section := promptTemplateSection(t, content, `  - id: "default_rewrite"`, `  - id: "standard_rewrite"`)

	for _, required := range []string{
		"Independently classify `evidence_need`",
		"`none`",
		"`knowledge_base`",
		"`web`",
		`intent="conversation_state"`,
		`evidence_need="knowledge_base"`,
		`"evidence_need":"string"`,
	} {
		if !strings.Contains(section, required) {
			t.Errorf("default rewrite prompt is missing %q", required)
		}
	}
	for _, line := range strings.Split(section, "\n") {
		if strings.Contains(line, "Output: {") && !strings.Contains(line, `"evidence_need"`) {
			t.Errorf("rewrite example omits evidence_need: %s", line)
		}
	}
	for _, forbidden := range []string{"temporary access card", "case_id", "required_claim", "reference_answer"} {
		if strings.Contains(strings.ToLower(section), forbidden) {
			t.Errorf("default rewrite prompt contains scenario/Eval term %q", forbidden)
		}
	}
}
