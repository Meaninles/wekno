package generalagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
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
		!strings.Contains(history[1].Content, `authority="model_output_not_evidence"`) {
		t.Fatalf("historical assistant answer was not marked non-authoritative: %#v", history[1])
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

func TestArtifactReturnPolicyAdmitsAtRegistrationWithoutCountTruncation(t *testing.T) {
	manager := artifactReturnPolicy()
	if manager["artifact_count_limited"] != false || manager["max_artifact_count"] != nil {
		t.Fatalf("manager policy = %#v, want unlimited artifact count", manager)
	}
	if manager["total_return_size_limit_bytes"] != int64(128*1024*1024) {
		t.Fatalf("manager size limit = %v, want 128MB", manager["total_return_size_limit_bytes"])
	}
}

func TestArtifactDeliveryBelongsToItsActualToolCall(t *testing.T) {
	for _, origin := range []string{"workspace", "sdk"} {
		t.Run(origin, func(t *testing.T) {
			run, bus := &activeRun{sessionID: "session"}, event.NewEventBus()
			var results []event.AgentToolResultData
			bus.On(event.EventAgentToolResult, func(_ context.Context, e event.Event) error {
				results = append(results, e.Data.(event.AgentToolResultData))
				return nil
			})
			item := SidecarArtifact{FileToken: "token", ArtifactID: "stored", FileName: "report.pdf",
				FileSize: 12, SHA256: "hash", Persisted: true, DownloadURL: "/download"}
			var output any = item
			if origin == "sdk" {
				encoded, _ := json.Marshal(item)
				output = string(encoded)
			}
			for _, phase := range []string{"start", "success", "success"} {
				data, _ := json.Marshal(map[string]any{"tool_call_id": "model-call", "tool_name": "create_artifact",
					"phase": phase, "origin": origin, "output": output, "duration_ms": 91})
				run.recordRuntimeToolEvent(context.Background(), bus, sidecarProgressDataFromEvent(StreamEvent{Data: data}))
			}
			steps := run.snapshotSteps("")
			if len(results) != 1 || len(steps) != 1 || results[0].ToolCallID != "model-call" || results[0].Duration != 91 {
				t.Fatalf("delivery added a fake tool or lost identity/duration: %+v / %+v", results, steps)
			}
			data := results[0].Data
			if data["display_type"] != displayTypeArtifacts || data["artifacts"].([]map[string]interface{})[0]["artifact_id"] != "stored" {
				t.Fatalf("actual committed file missing from real tool: %+v", data)
			}
		})
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
			AgentType:                       types.AgentTypeDocumentProcessingAgent,
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

func TestEmitSidecarStatusProgressDoesNotBecomeATool(t *testing.T) {
	svc := &Service{}
	active := &activeRun{}
	bus := event.NewEventBus()
	var got event.AgentProgressData
	bus.On(event.EventAgentProgress, func(ctx context.Context, evt event.Event) error {
		got, _ = evt.Data.(event.AgentProgressData)
		return nil
	})

	var streamed strings.Builder
	lastID := ""
	lastDone := false
	svc.emitSidecarEvent(context.Background(), bus, "session-1", "fallback-answer", StreamEvent{
		ID:      "status-1",
		Type:    "progress",
		Content: "正在整理最终回答",
		Data:    []byte(`{"progress_kind":"assistant_status","progress_id":"status-1","phase":"start","transient":true}`),
	}, &streamed, &lastID, &lastDone, active)

	if got.ToolName != "" {
		t.Fatalf("status progress tool name = %q, want empty", got.ToolName)
	}
	if got.ToolCallID != "status-1" || !got.Transient {
		t.Fatalf("status progress metadata not preserved: %+v", got)
	}
	if got.Metadata["progress_kind"] != "assistant_status" {
		t.Fatalf("progress kind = %#v", got.Metadata["progress_kind"])
	}
	if len(active.snapshotSteps("")) != 0 {
		t.Fatal("status-only events must not enter tool history")
	}
}

func TestSidecarRuntimeToolsUsePlatformEventAndHistoryContract(t *testing.T) {
	for _, origin := range []string{"workspace", "sdk", "runtime_validation"} {
		t.Run(origin, func(t *testing.T) {
			svc, bus, run := &Service{}, event.NewEventBus(), &activeRun{sessionID: "session"}
			var calls []event.AgentToolCallData
			var results []event.AgentToolResultData
			bus.On(event.EventAgentToolCall, func(_ context.Context, e event.Event) error {
				calls = append(calls, e.Data.(event.AgentToolCallData))
				return nil
			})
			bus.On(event.EventAgentToolResult, func(_ context.Context, e event.Event) error {
				results = append(results, e.Data.(event.AgentToolResultData))
				return nil
			})
			var streamed strings.Builder
			id, done := "", false
			for _, phase := range []string{"start", "start", "error", "error"} {
				data, _ := json.Marshal(map[string]any{"tool_call_id": "call", "tool_name": "execute_code",
					"phase": phase, "origin": origin, "arguments": map[string]any{"code": "raise Exception('failure')"},
					"output": map[string]any{"success": false, "stderr": "actual failure"}, "duration_ms": 42})
				svc.emitSidecarEvent(context.Background(), bus, "session", "answer", StreamEvent{
					Type: "runtime_tool", ID: "call", Data: data,
				}, &streamed, &id, &done, run)
			}
			steps := run.snapshotSteps("")
			if len(calls) != 1 || len(results) != 1 || len(steps) != 1 || sourcerefs.AgentToolCallCount(steps) != 1 {
				t.Fatalf("duplicate or missing actual tool: calls=%d results=%d steps=%+v", len(calls), len(results), steps)
			}
			call := steps[0].ToolCalls[0]
			if call.Duration != 42 || call.Result.Success || !strings.Contains(call.Result.Output, "actual failure") || call.Args["code"] != "raise Exception('failure')" {
				t.Fatalf("tool audit lost actual input/result/timing: %+v", call)
			}
			if call.Result.Data != nil || calls[0].ToolCallID != results[0].ToolCallID {
				t.Fatal("runtime tools must not duplicate their payload as progress metadata")
			}
		})
	}
}
