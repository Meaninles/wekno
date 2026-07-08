package generalagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestDedupeSidecarArtifactsByFilenameKeepLast(t *testing.T) {
	items := []SidecarArtifact{
		{FileToken: "first", FileName: "report.xlsx", FileSize: 10},
		{FileToken: "second", FileName: "other.xlsx", FileSize: 20},
		{FileToken: "third", FileName: "report.xlsx", FileSize: 30},
	}

	got := dedupeSidecarArtifactsByFilenameKeepLast(items)

	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].FileToken != "second" || got[1].FileToken != "third" {
		t.Fatalf("tokens=%v, want [second third]", []string{got[0].FileToken, got[1].FileToken})
	}
}

func TestBuildArtifactToolResultIncludesPersistFailureNotice(t *testing.T) {
	data, output, emit := buildArtifactToolResult(&ChatResult{
		ArtifactOriginalCount: 1,
		ArtifactReturnedCount: 1,
	}, nil, errors.New("ERROR: value too long for type character varying(36)"))

	if !emit {
		t.Fatalf("emit = false, want true")
	}
	if data["display_type"] != displayTypeArtifacts {
		t.Fatalf("display_type = %v, want %s", data["display_type"], displayTypeArtifacts)
	}
	if data["persist_failed"] != true {
		t.Fatalf("persist_failed = %v, want true", data["persist_failed"])
	}
	if got := data["persist_error"]; !strings.Contains(got.(string), "value too long") {
		t.Fatalf("persist_error = %v, want value-too-long detail", got)
	}
	notice, _ := data["notice"].(string)
	if !strings.Contains(notice, "保存下载记录失败") || !strings.Contains(notice, "暂时无法提供下载链接") {
		t.Fatalf("notice = %q, want explicit persistence failure", notice)
	}
	if !strings.Contains(output, "保存下载记录失败") {
		t.Fatalf("output = %q, want persistence failure", output)
	}
}

func TestBuildArtifactToolResultSerializesArtifactsAsMapList(t *testing.T) {
	data, _, emit := buildArtifactToolResult(&ChatResult{
		ArtifactOriginalCount: 1,
		ArtifactReturnedCount: 1,
	}, []ArtifactResult{{
		ArtifactID:  "artifact-1",
		FileName:    "report.pdf",
		FileType:    "pdf",
		FileSize:    1234,
		SHA256:      "abc123",
		DownloadURL: "/api/v1/custom/general-agent/artifacts/artifact-1/download",
	}}, nil)

	if !emit {
		t.Fatalf("emit = false, want true")
	}
	artifacts, ok := data["artifacts"].([]map[string]interface{})
	if !ok {
		t.Fatalf("artifacts type = %T, want []map[string]interface{}", data["artifacts"])
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts len = %d, want 1", len(artifacts))
	}
	if got := artifacts[0]["artifact_id"]; got != "artifact-1" {
		t.Fatalf("artifact_id = %v, want artifact-1", got)
	}
	if got := artifacts[0]["filename"]; got != "report.pdf" {
		t.Fatalf("filename = %v, want report.pdf", got)
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

	lightMode, lightNames := configuredLightweightSkillSelection(agent)
	if lightMode != "selected" || strings.Join(lightNames, ",") != "light-a,light-b" {
		t.Fatalf("light selection = (%q, %v), want split lightweight fields", lightMode, lightNames)
	}
	proMode, proNames := configuredProfessionalSkillSelection(agent)
	if proMode != "selected" || strings.Join(proNames, ",") != "pro-a" {
		t.Fatalf("professional selection = (%q, %v), want split professional fields", proMode, proNames)
	}
}

func TestConfiguredLightweightSkillSelectionFallsBackToLegacyFields(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			SkillsSelectionMode: "selected",
			SelectedSkills:      []string{"legacy-a"},
		},
	}

	mode, names := configuredLightweightSkillSelection(agent)
	if mode != "selected" || strings.Join(names, ",") != "legacy-a" {
		t.Fatalf("light selection = (%q, %v), want legacy fallback", mode, names)
	}
}

func TestProfessionalSkillSpecsForDocumentProcessingAllowsNoProfessionalSkill(t *testing.T) {
	specs, err := (&Service{}).professionalSkillSpecs(context.Background(), &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentType:                       types.AgentTypeDocumentProcessingAgent,
			ProfessionalSkillsSelectionMode: "none",
		},
	})
	if err != nil {
		t.Fatalf("professionalSkillSpecs returned error: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %+v, want no professional skills", specs)
	}
}

func TestEmitSidecarAnswerUsesSegmentIDAndDone(t *testing.T) {
	svc := &Service{}
	bus := event.NewEventBus()
	var got []event.Event
	bus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
		got = append(got, evt)
		return nil
	})

	var streamed strings.Builder
	lastID := ""
	lastDone := false
	svc.emitSidecarEvent(context.Background(), bus, "session-1", "fallback-answer", StreamEvent{
		ID:      "segment-1",
		Type:    "answer_delta",
		Content: "hello",
	}, &streamed, &lastID, &lastDone, nil)
	svc.emitSidecarEvent(context.Background(), bus, "session-1", "fallback-answer", StreamEvent{
		ID:   "segment-1",
		Type: "answer_delta",
		Done: true,
	}, &streamed, &lastID, &lastDone, nil)

	if streamed.String() != "hello" {
		t.Fatalf("streamed = %q, want hello", streamed.String())
	}
	if lastID != "segment-1" || !lastDone {
		t.Fatalf("last segment state = (%q, %v), want (segment-1, true)", lastID, lastDone)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	for _, evt := range got {
		if evt.ID != "segment-1" {
			t.Fatalf("event ID = %q, want segment-1", evt.ID)
		}
	}
	data, ok := got[1].Data.(event.AgentFinalAnswerData)
	if !ok {
		t.Fatalf("done event data type = %T, want AgentFinalAnswerData", got[1].Data)
	}
	if !data.Done {
		t.Fatalf("done event Done = false, want true")
	}
}

func TestEmitSidecarProgressUsesAgentProgress(t *testing.T) {
	svc := &Service{}
	bus := event.NewEventBus()
	var got []event.Event
	bus.On(event.EventAgentProgress, func(ctx context.Context, evt event.Event) error {
		got = append(got, evt)
		return nil
	})
	bus.On(event.EventAgentThought, func(ctx context.Context, evt event.Event) error {
		t.Fatalf("progress should not emit thought event: %+v", evt)
		return nil
	})

	var streamed strings.Builder
	lastID := ""
	lastDone := false
	svc.emitSidecarEvent(context.Background(), bus, "session-1", "fallback-answer", StreamEvent{
		ID:      "toolu-1",
		Type:    "progress",
		Content: "正在执行命令",
		Data:    []byte(`{"tool_name":"Bash","tool_call_id":"toolu-1","phase":"start","message":"正在执行命令","validation_issue_codes":["table_not_requested"]}`),
	}, &streamed, &lastID, &lastDone, nil)

	if len(got) != 1 {
		t.Fatalf("progress events = %d, want 1", len(got))
	}
	if got[0].Type != event.EventAgentProgress {
		t.Fatalf("event type = %s, want agent progress", got[0].Type)
	}
	data, ok := got[0].Data.(event.AgentProgressData)
	if !ok {
		t.Fatalf("progress data type = %T, want AgentProgressData", got[0].Data)
	}
	if data.Content != "正在执行命令" || data.ToolName != "Bash" || data.ToolCallID != "toolu-1" || data.Phase != "start" {
		t.Fatalf("progress data not preserved: %+v", data)
	}
	if codes, ok := data.Metadata["validation_issue_codes"].([]interface{}); !ok || len(codes) != 1 || codes[0] != "table_not_requested" {
		t.Fatalf("progress metadata not preserved: %#v", data.Metadata)
	}
}
