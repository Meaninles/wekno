package agentruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

type recordingProfessionalSkillProvider struct {
	names []string
	all   bool
	calls int
}

func TestBuildGeneralAgentHistoryKeepsRecentPairsAndBuildsCompleteUserLedger(t *testing.T) {
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	var messages []*types.Message
	for index := 1; index <= 7; index++ {
		requestID := "request-" + string(rune('0'+index))
		messages = append(messages,
			&types.Message{
				ID: requestID + "-user", RequestID: requestID, Role: "user",
				Content: "user-fact-" + string(rune('0'+index)), CreatedAt: base.Add(time.Duration(index) * time.Minute),
			},
			&types.Message{
				ID: requestID + "-assistant", RequestID: requestID, Role: "assistant",
				Content: "assistant-inference-" + string(rune('0'+index)), IsCompleted: true,
				CreatedAt: base.Add(time.Duration(index)*time.Minute + time.Second),
			},
		)
	}
	messages = append(messages, &types.Message{
		ID: "in-flight-user", RequestID: "in-flight", Role: "user",
		Content: "must-not-archive-incomplete", CreatedAt: base.Add(20 * time.Minute),
	})

	history, archive := buildGeneralAgentHistory(messages, 2, "in-flight-user")
	if len(history) != 4 {
		t.Fatalf("history messages = %d, want two complete Q&A pairs", len(history))
	}
	if history[0].Content != "user-fact-6" || history[2].Content != "user-fact-7" {
		t.Fatalf("recent history is not chronological/newest: %#v", history)
	}
	if history[0].SourceID != "user_message_request-6-user" || history[2].SourceID != "user_message_request-7-user" {
		t.Fatalf("recent user source IDs are not stable: %#v", history)
	}
	if history[1].SourceID != "assistant_message_request-6-assistant" ||
		history[1].Content != "assistant-inference-6" || history[1].Role != "assistant" {
		t.Fatalf("historical assistant answer did not retain its native role: %#v", history[1])
	}
	for _, expected := range []string{"user-fact-1", "user-fact-5"} {
		if !strings.Contains(archive, expected) {
			t.Fatalf("user ledger missing %q: %s", expected, archive)
		}
	}
	for _, forbidden := range []string{"assistant-inference", "must-not-archive-incomplete"} {
		if strings.Contains(archive, forbidden) {
			t.Fatalf("user ledger contains forbidden value %q: %s", forbidden, archive)
		}
	}
}

func (p *recordingProfessionalSkillProvider) ProfessionalPackages(
	_ context.Context,
	names []string,
	all bool,
) ([]skillhub.ProfessionalSkillPackage, error) {
	p.names = append([]string(nil), names...)
	p.all = all
	p.calls++
	packages := make([]skillhub.ProfessionalSkillPackage, 0, len(names))
	for _, name := range names {
		packages = append(packages, skillhub.ProfessionalSkillPackage{
			Name:        name,
			DisplayName: name,
			Description: "test " + name,
			Files: []skillhub.ProfessionalSkillFile{{
				Path:          "SKILL.md",
				ContentBase64: "dGVzdA==",
			}},
		})
	}
	return packages, nil
}

func TestArtifactReturnPolicyAdmitsAtRegistrationWithoutCountTruncation(t *testing.T) {
	manager := artifactReturnPolicy()
	if manager["artifact_count_limited"] != false || manager["max_artifact_count"] != nil {
		t.Fatalf("manager policy = %#v, want unlimited artifact count", manager)
	}
	if manager["total_return_size_limit_bytes"] != int64(128*1024*1024) {
		t.Fatalf("manager size limit = %v, want 128MB", manager["total_return_size_limit_bytes"])
	}
}

func TestRuntimePromptUsesSharedCitationProtocolOnce(t *testing.T) {
	for _, body := range []string{"Use the available tools.", sourcerefs.EnsureGenerationContract("Custom instructions.")} {
		prompt := renderSystemPrompt(context.Background(), body, false)
		if strings.Count(prompt, "[WEKNORA_CITATION_OUTPUT]") != 1 || !strings.Contains(prompt, `<src id="S1" />`) {
			t.Fatalf("runtime did not receive the common source protocol exactly once: %s", prompt)
		}
	}
}

func TestKnowledgeQAStylePrioritizesAccuracyAndLeavesGeneralUnchanged(t *testing.T) {
	qa := &types.CustomAgent{Config: types.CustomAgentConfig{AgentType: types.AgentTypeKnowledgeQA}}
	got := answerStylePrompt("Base instructions", qa)
	if !strings.Contains(got, "[KNOWLEDGE_QA_STYLE]") || !strings.Contains(got, "Accuracy is the first priority") || !strings.Contains(got, "Base instructions") {
		t.Fatal(got)
	}
	general := &types.CustomAgent{Config: types.CustomAgentConfig{AgentType: types.AgentTypeGeneralAgent}}
	if answerStylePrompt("Base instructions", general) != "Base instructions" {
		t.Fatal("General style changed")
	}
}

func TestBuildEffectiveQueryPreservesUserPromptVerbatim(t *testing.T) {
	svc := &Service{}
	req := &types.QARequest{
		Query:            "请总结输入材料\n不要改写这句话。",
		ImageDescription: "图片里有一张收入趋势图，峰值在 6 月。",
		QuotedContext:    "[引用消息]\n上轮提到只看华东区域。",
		Attachments: types.MessageAttachments{
			{
				FileName:    "sales.txt",
				FileType:    ".txt",
				FileSize:    42,
				Content:     "GENERAL_AGENT_ATTACHMENT_TOKEN_20260628\n华东收入 185.75。",
				IsTruncated: false,
			},
		},
	}

	query := svc.buildEffectiveQuery(context.Background(), req)

	if query != req.Query {
		t.Fatalf("effective query = %q, want verbatim user query %q", query, req.Query)
	}
	for _, forbidden := range []string{
		"[用户上传图片内容]",
		"图片里有一张收入趋势图",
		"[引用消息]",
		"<attachments>",
		"GENERAL_AGENT_ATTACHMENT_TOKEN_20260628",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("effective query should not contain contextual block %q:\n%s", forbidden, query)
		}
	}
}

func TestAttachmentSpecsPreserveAllRuntimeFields(t *testing.T) {
	specs := attachmentSpecs(types.MessageAttachments{
		{
			FileName:    "report.md",
			FileType:    ".md",
			FileSize:    128,
			Content:     "token",
			IsTruncated: true,
		},
	})

	if len(specs) != 1 {
		t.Fatalf("len(specs) = %d, want 1", len(specs))
	}
	got := specs[0]
	if got.FileName != "report.md" || got.FileType != ".md" || got.FileSize != 128 ||
		got.Content != "token" || !got.IsTruncated {
		t.Fatalf("attachment spec not preserved: %+v", got)
	}
}

func TestConfiguredSkillSelectionsUseSplitFields(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			SkillsSelectionMode:             "all",
			SelectedSkills:                  []string{"legacy"},
			LightweightSkillsSelectionMode:  "selected",
			SelectedLightweightSkills:       []string{"light-a", "light-b"},
			ProfessionalSkillsSelectionMode: "selected",
			SelectedProfessionalSkills:      []string{"pro-a"},
		},
	}

	proMode, proNames := configuredProfessionalSkillSelection(agent)
	if proMode != "selected" || strings.Join(proNames, ",") != "pro-a" {
		t.Fatalf("professional selection = (%q, %v), want split professional fields", proMode, proNames)
	}
}

func TestProfessionalSkillSpecsForDocumentProcessingAllowsNoProfessionalSkill(t *testing.T) {
	specs, err := (&Service{}).professionalSkillSpecs(context.Background(), &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentType:                       types.AgentTypeGeneralAgent,
			ProfessionalSkillsSelectionMode: "none",
		},
	}, []string{"pro-a"})
	if err != nil {
		t.Fatalf("professionalSkillSpecs returned error: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %+v, want no professional skills", specs)
	}
}

func TestProfessionalSkillSpecsNarrowsConfiguredAllowlistToChatSelection(t *testing.T) {
	provider := &recordingProfessionalSkillProvider{}
	svc := &Service{professionalSkills: provider}
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			ProfessionalSkillsSelectionMode: "selected",
			SelectedProfessionalSkills:      []string{"pro-a", "pro-b"},
		},
	}

	specs, err := svc.professionalSkillSpecs(
		context.Background(),
		agent,
		[]string{" pro-b ", "outside-agent-allowlist", "pro-b"},
	)
	if err != nil {
		t.Fatalf("professionalSkillSpecs returned error: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if provider.all {
		t.Fatalf("provider all = true, want a request-scoped package load")
	}
	if got := strings.Join(provider.names, ","); got != "pro-b" {
		t.Fatalf("provider names = %q, want pro-b", got)
	}
	if len(specs) != 1 || specs[0].Name != "pro-b" {
		t.Fatalf("specs = %+v, want only pro-b", specs)
	}
}

func TestEffectiveProfessionalSkillSelectionPreservesAgentDefaultsWithoutChatSelection(t *testing.T) {
	selectedAgent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			ProfessionalSkillsSelectionMode: "selected",
			SelectedProfessionalSkills:      []string{"pro-a", "pro-b"},
		},
	}
	names, all := effectiveProfessionalSkillSelection(selectedAgent, nil)
	if all || strings.Join(names, ",") != "pro-a,pro-b" {
		t.Fatalf("selected defaults = (%v, %v), want ([pro-a pro-b], false)", names, all)
	}

	allAgent := &types.CustomAgent{
		Config: types.CustomAgentConfig{ProfessionalSkillsSelectionMode: "all"},
	}
	names, all = effectiveProfessionalSkillSelection(allAgent, nil)
	if !all || len(names) != 0 {
		t.Fatalf("all defaults = (%v, %v), want (nil, true)", names, all)
	}

	names, all = effectiveProfessionalSkillSelection(allAgent, []string{"pro-b", "pro-b"})
	if all || strings.Join(names, ",") != "pro-b" {
		t.Fatalf("all request scope = (%v, %v), want ([pro-b], false)", names, all)
	}
}
