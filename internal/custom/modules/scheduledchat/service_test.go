package scheduledchat

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestNextRunAfterMonthlySkipsInvalidDates(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		day        int
		afterLocal time.Time
		wantLocal  time.Time
	}{
		{
			name:       "day 31 skips February",
			day:        31,
			afterLocal: time.Date(2026, time.January, 31, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2026, time.March, 31, 9, 0, 0, 0, loc),
		},
		{
			name:       "day 30 skips February",
			day:        30,
			afterLocal: time.Date(2026, time.January, 30, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2026, time.March, 30, 9, 0, 0, 0, loc),
		},
		{
			name:       "day 29 uses leap year February",
			day:        29,
			afterLocal: time.Date(2028, time.January, 29, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2028, time.February, 29, 9, 0, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &Task{
				ScheduleType: ScheduleTypeMonthly,
				Timezone:     "Asia/Shanghai",
				DayOfMonth:   tt.day,
				Hour:         9,
				Minute:       0,
			}
			got, err := NextRunAfter(task, tt.afterLocal.UTC())
			if err != nil {
				t.Fatalf("NextRunAfter returned error: %v", err)
			}
			if !got.Equal(tt.wantLocal.UTC()) {
				t.Fatalf("got %s, want %s", got.In(loc), tt.wantLocal)
			}
		})
	}
}

func TestSessionTitleForRunUsesExecutionDate(t *testing.T) {
	startedAt := time.Date(2026, time.June, 4, 16, 30, 0, 0, time.UTC)
	task := &Task{Timezone: "Asia/Shanghai", Name: "每日舆情"}
	run := &Run{
		ScheduledAt: time.Date(2026, time.June, 4, 15, 0, 0, 0, time.UTC),
		StartedAt:   &startedAt,
	}

	got := sessionTitleForRun(task, run)
	want := "2026.6.5 定时任务-每日舆情"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeRequestContextDerivesCapabilitiesFromMentions(t *testing.T) {
	ctx := normalizeRequestContext(RequestContext{
		KnowledgeBaseIDs: []string{"kb-1", "kb-1", ""},
		SkillNames:       []string{"basic-skill"},
		MentionedItems: types.MentionedItems{
			{ID: "kb-2", Name: "KB", Type: "kb"},
			{ID: "doc-1", Name: "Doc", Type: "file"},
			{ID: "skill-id", Name: "Skill", Type: "skill", SkillName: "mention-skill"},
		},
	})

	if got := len(ctx.KnowledgeBaseIDs); got != 2 {
		t.Fatalf("knowledge base ids count = %d, want 2: %#v", got, ctx.KnowledgeBaseIDs)
	}
	if got, want := ctx.KnowledgeBaseIDs[0], "kb-1"; got != want {
		t.Fatalf("first knowledge base id = %q, want %q", got, want)
	}
	if got, want := ctx.KnowledgeBaseIDs[1], "kb-2"; got != want {
		t.Fatalf("second knowledge base id = %q, want %q", got, want)
	}
	if got, want := ctx.KnowledgeIDs[0], "doc-1"; got != want {
		t.Fatalf("knowledge id = %q, want %q", got, want)
	}
	if got, want := ctx.SkillNames[1], "mention-skill"; got != want {
		t.Fatalf("mentioned skill = %q, want %q", got, want)
	}
}

func TestEffectiveRequestContextUsesLatestAgentProfessionalSkillConfig(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			ProfessionalSkillsSelectionMode: "selected",
			SelectedProfessionalSkills:      []string{"allowed-skill"},
		},
	}

	ctx := effectiveRequestContextForAgent(RequestContext{
		ProfessionalSkillNames: []string{"allowed-skill", "old-skill"},
	}, agent)

	if got, want := len(ctx.ProfessionalSkillNames), 1; got != want {
		t.Fatalf("professional skill count = %d, want %d: %#v", got, want, ctx.ProfessionalSkillNames)
	}
	if got, want := ctx.ProfessionalSkillNames[0], "allowed-skill"; got != want {
		t.Fatalf("professional skill = %q, want %q", got, want)
	}
	if got, want := applyProfessionalSkillPrefix(ctx.ProfessionalSkillNames, "处理任务"), "使用allowed-skill技能完成以下工作\n处理任务"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
}

func TestScheduledLastRequestStateKeepsRequestCapabilities(t *testing.T) {
	task := &Task{
		TenantID:         10,
		AgentID:          "agent-data",
		WebSearchEnabled: true,
	}
	agent := &types.CustomAgent{
		ID:       "agent-data",
		TenantID: 20,
		Config: types.CustomAgentConfig{
			AgentMode: types.AgentModeSmartReasoning,
		},
	}
	ctx := RequestContext{
		KnowledgeBaseIDs:       []string{"kb-1"},
		KnowledgeIDs:           []string{"file-1"},
		TagIDs:                 []string{"tag-1"},
		MCPServiceIDs:          []string{"mcp-1"},
		SkillNames:             []string{"light-skill"},
		ProfessionalSkillNames: []string{"pro-skill"},
		SummaryModelID:         "model-1",
		MentionedItems: types.MentionedItems{
			{ID: "file-1", Type: "file", KBID: "kb-1"},
		},
	}

	state := scheduledLastRequestState(task, agent, ctx)

	if state.AgentID != task.AgentID {
		t.Fatalf("agent_id = %q, want %q", state.AgentID, task.AgentID)
	}
	if !state.AgentEnabled {
		t.Fatalf("agent_enabled = false, want true")
	}
	if got, want := state.AgentSourceTenantID, "20"; got != want {
		t.Fatalf("agent_source_tenant_id = %q, want %q", got, want)
	}
	if got, want := state.ModelID, "model-1"; got != want {
		t.Fatalf("model_id = %q, want %q", got, want)
	}
	if got, want := state.KnowledgeBaseIDs[0], "kb-1"; got != want {
		t.Fatalf("knowledge_base_ids[0] = %q, want %q", got, want)
	}
	if got, want := state.KnowledgeIDs[0], "file-1"; got != want {
		t.Fatalf("knowledge_ids[0] = %q, want %q", got, want)
	}
	if got, want := state.TagIDs[0], "tag-1"; got != want {
		t.Fatalf("tag_ids[0] = %q, want %q", got, want)
	}
	if got, want := state.MCPServiceIDs[0], "mcp-1"; got != want {
		t.Fatalf("mcp_service_ids[0] = %q, want %q", got, want)
	}
	if got, want := state.SkillNames[0], "light-skill"; got != want {
		t.Fatalf("skill_names[0] = %q, want %q", got, want)
	}
	if got, want := state.ProfessionalSkillNames[0], "pro-skill"; got != want {
		t.Fatalf("professional_skill_names[0] = %q, want %q", got, want)
	}
	if !state.WebSearchEnabled {
		t.Fatalf("web_search_enabled = false, want true")
	}
}

func TestBackfillScheduledSessionLastRequestStates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&types.Session{}, &Task{}, &Run{}, &types.CustomAgent{}); err != nil {
		t.Fatal(err)
	}
	agent := &types.CustomAgent{
		ID:       "agent-data",
		TenantID: 7,
		Name:     "数据分析",
		Config: types.CustomAgentConfig{
			AgentMode: types.AgentModeSmartReasoning,
		},
	}
	if err := db.Create(agent).Error; err != nil {
		t.Fatal(err)
	}
	session := &types.Session{
		Title:       "old",
		Description: SessionMarkerPrefix + "task=task-1;run=run-1",
		TenantID:    7,
		UserID:      "user-1",
	}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID:               "task-1",
		TenantID:         7,
		CreatedBy:        "user-1",
		RunAsUserID:      "user-1",
		Name:             "数据测试",
		Enabled:          true,
		AgentID:          agent.ID,
		ScheduleType:     ScheduleTypeDaily,
		Timezone:         "Asia/Shanghai",
		PromptTemplate:   "总结数据",
		WebSearchEnabled: true,
		RequestContext: RequestContext{
			KnowledgeBaseIDs:       []string{"kb-1"},
			KnowledgeIDs:           []string{"file-1"},
			MCPServiceIDs:          []string{"mcp-1"},
			SkillNames:             []string{"light-skill"},
			ProfessionalSkillNames: []string{"pro-skill"},
			MentionedItems: types.MentionedItems{
				{ID: "file-1", Type: "file", KBID: "kb-1"},
			},
			SummaryModelID: "model-1",
		},
	}
	if err := db.Create(task).Error; err != nil {
		t.Fatal(err)
	}
	run := &Run{
		ID:          "run-1",
		TaskID:      task.ID,
		TenantID:    task.TenantID,
		RunAsUserID: task.RunAsUserID,
		ScheduledAt: time.Now().UTC(),
		Status:      RunStatusSuccess,
		SessionID:   session.ID,
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}

	service := &Service{}
	if err := service.backfillScheduledSessionLastRequestStates(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	var got types.Session
	if err := db.First(&got, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.LastRequestState == nil {
		t.Fatal("last_request_state is nil")
	}
	if got.LastRequestState.AgentID != agent.ID {
		t.Fatalf("agent_id = %q, want %q", got.LastRequestState.AgentID, agent.ID)
	}
	if !got.LastRequestState.AgentEnabled {
		t.Fatal("agent_enabled = false, want true")
	}
	if got.LastRequestState.ProfessionalSkillNames[0] != "pro-skill" {
		t.Fatalf("professional_skill_names = %#v", got.LastRequestState.ProfessionalSkillNames)
	}
	if !got.LastRequestState.WebSearchEnabled {
		t.Fatal("web_search_enabled = false, want true")
	}
}
