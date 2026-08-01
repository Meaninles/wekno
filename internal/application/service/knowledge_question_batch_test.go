package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

type questionBatchChatStub struct {
	response  string
	responses []string
	err       error
	calls     int
	messages  []chat.Message
	options   *chat.ChatOptions
}

func (s *questionBatchChatStub) Chat(
	_ context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (*types.ChatResponse, error) {
	s.calls++
	s.messages = messages
	s.options = options
	if s.err != nil {
		return nil, s.err
	}
	response := s.response
	if len(s.responses) >= s.calls {
		response = s.responses[s.calls-1]
	}
	return &types.ChatResponse{Content: response}, nil
}

func (*questionBatchChatStub) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*questionBatchChatStub) GetModelName() string { return "question-batch-stub" }
func (*questionBatchChatStub) GetModelID() string   { return "question-batch-stub" }

type questionBatchModelServiceStub struct {
	interfaces.ModelService
	chatModel      chat.Chat
	embeddingCalls int
}

func (s *questionBatchModelServiceStub) GetChatModel(
	context.Context,
	string,
) (chat.Chat, error) {
	return s.chatModel, nil
}

func (s *questionBatchModelServiceStub) GetEmbeddingModel(
	context.Context,
	string,
) (embedding.Embedder, error) {
	s.embeddingCalls++
	return nil, errors.New("embedding initialization must be deferred")
}

type questionBatchKBServiceStub struct {
	interfaces.KnowledgeBaseService
	kb *types.KnowledgeBase
}

func (s *questionBatchKBServiceStub) GetKnowledgeBaseByID(
	context.Context,
	string,
) (*types.KnowledgeBase, error) {
	return s.kb, nil
}

type questionBatchKnowledgeRepoStub struct {
	interfaces.KnowledgeRepository
	knowledge *types.Knowledge
}

func (s *questionBatchKnowledgeRepoStub) GetKnowledgeByID(
	context.Context,
	uint64,
	string,
) (*types.Knowledge, error) {
	return s.knowledge, nil
}

type questionBatchChunkRepoStub struct {
	interfaces.ChunkRepository
	chunk *types.Chunk
}

func (s *questionBatchChunkRepoStub) GetChunkByID(
	_ context.Context,
	_ uint64,
	id string,
) (*types.Chunk, error) {
	if s.chunk != nil && s.chunk.ID == id {
		return s.chunk, nil
	}
	return nil, nil
}

func (*questionBatchChunkRepoStub) ListChunksByParentIDs(
	context.Context,
	uint64,
	[]string,
) ([]*types.Chunk, error) {
	return nil, nil
}

func TestQuestionProviderCircuitRejectSkipsEmbeddingAndRetrieveInitialization(t *testing.T) {
	providerErr := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 5 * time.Minute,
	}
	chatStub := &questionBatchChatStub{err: providerErr}
	models := &questionBatchModelServiceStub{chatModel: chatStub}
	svc := &knowledgeService{
		config: &config.Config{Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `{{content}} {{output_instructions}}`,
		}},
		repo: &questionBatchKnowledgeRepoStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
			Title: "制度",
		}},
		kbService: &questionBatchKBServiceStub{kb: &types.KnowledgeBase{
			ID: "kb-1", TenantID: 42, SummaryModelID: "chat-1", DerivativeModelID: "chat-1",
			EmbeddingModelID: "embedding-1",
		}},
		modelService: models,
		chunkRepo: &questionBatchChunkRepoStub{chunk: &types.Chunk{
			ID: "chunk-1", Content: "审批由业务部门负责。",
		}},
	}
	payload := types.QuestionGenerationPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", ChunkIDs: []string{"chunk-1"},
		QuestionCount: 2,
	}

	err := svc.processQuestionGenerationForChunks(
		context.Background(),
		asynq.NewTask(types.TypeQuestionGeneration, nil),
		payload,
	)
	require.ErrorIs(t, err, modeladmission.ErrProviderCircuitOpen)
	require.Equal(t, 1, chatStub.calls)
	require.Zero(t, models.embeddingCalls,
		"a circuit-rejected task has no questions to embed or index")
}

func TestParseQuestionBatchResponseKeepsExactChunkMapping(t *testing.T) {
	inputs := []questionBatchInput{
		{RecordID: "chunk-zero", Content: "content zero"},
		{RecordID: "chunk-three", Content: "content three"},
	}
	raw := "```json\n" +
		`{"results":[` +
		`{"record_id":"chunk-zero","questions":["1. 谁负责审批该事项？","谁负责审批该事项？"]},` +
		`{"record_id":"chunk-three","questions":["超过期限后应如何处理？","哪些情形属于例外？","额外问题不会被保留？"]}` +
		`]}` +
		"\n```"
	report, err := parseQuestionBatchResponse(raw, inputs, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, report.Results["chunk-zero"])
	require.Equal(t, []string{"超过期限后应如何处理？", "哪些情形属于例外？"}, report.Results["chunk-three"])
}

func TestQuestionBatchRecordIDIsShortStableAndNonPositional(t *testing.T) {
	first := questionBatchRecordID("generation-1", "chunk-a")
	replayed := questionBatchRecordID("generation-1", "chunk-a")
	otherChunk := questionBatchRecordID("generation-1", "chunk-b")
	otherGeneration := questionBatchRecordID("generation-2", "chunk-a")
	require.Equal(t, first, replayed)
	require.Regexp(t, `^r_[0-9a-f]{16}$`, first)
	require.NotEqual(t, first, otherChunk)
	require.NotEqual(t, first, otherGeneration)
}

func TestParseQuestionBatchResponseNormalizesUnknownAndDuplicateRecordIDs(t *testing.T) {
	inputs := []questionBatchInput{{RecordID: "chunk-zero", Content: "content"}}
	report, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"record_id":"one-past-end","questions":["不应链接到任何块？"]},`+
			`{"record_id":"chunk-zero","questions":["第一个有效问题是什么？"]},`+
			`{"record_id":"chunk-zero","questions":["第二个有效问题是什么？"]}]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"one-past-end"}, report.UnknownRecordIDs)
	require.Equal(t, 1, report.DuplicateRecordCount)
	require.Equal(t, []string{"第一个有效问题是什么？", "第二个有效问题是什么？"}, report.Results["chunk-zero"])
}

func TestParseQuestionBatchResponseDistinguishesExplicitEmptyFromOmission(t *testing.T) {
	inputs := []questionBatchInput{
		{RecordID: "chunk-0", Content: "useful"},
		{RecordID: "chunk-1", Content: "blank form"},
		{RecordID: "chunk-2", Content: "omitted"},
	}
	report, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"record_id":"chunk-0","questions":["谁负责审批该事项？"]},`+
			`{"record_id":"chunk-1","questions":[]}]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, report.Results["chunk-0"])
	require.Contains(t, report.Results, "chunk-1")
	require.Empty(t, report.Results["chunk-1"])
	require.Equal(t, []string{"chunk-2"}, report.MissingRecordIDs)
}

func TestParseQuestionBatchResponseAcceptsExplicitQuestionObjects(t *testing.T) {
	inputs := []questionBatchInput{
		{RecordID: "chunk-0", Content: "content zero"},
		{RecordID: "chunk-1", Content: "content one"},
	}
	report, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"record_id":"chunk-0","questions":[{"question":"谁负责审批该事项？","answer":"业务部门"}]},`+
			`{"record_id":"chunk-1","questions":["办理时限不得超过多久？",{"text":"哪些情形属于例外？"}]}`+
			`]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, report.Results["chunk-0"])
	require.Equal(t, []string{"办理时限不得超过多久？", "哪些情形属于例外？"}, report.Results["chunk-1"])
}

func TestParseQuestionBatchResponseSkipsAmbiguousQuestionObjects(t *testing.T) {
	inputs := []questionBatchInput{{RecordID: "chunk-0", Content: "content"}}
	report, err := parseQuestionBatchResponse(
		`{"results":[{"record_id":"chunk-0","questions":[`+
			`{"question":"谁负责审批该事项？","text":"办理时限是多久？"},`+
			`{"answer":"业务部门"},"谁负责最终复核？"]}]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, 2, report.InvalidQuestionCount)
	require.Equal(t, []string{"谁负责最终复核？"}, report.Results["chunk-0"])
}

func TestParseQuestionBatchResponseRecoversCompleteItemsFromTruncatedTail(t *testing.T) {
	inputs := []questionBatchInput{
		{RecordID: "chunk-0", Content: "content zero"},
		{RecordID: "chunk-1", Content: "content one"},
	}
	report, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"record_id":"chunk-0","questions":["谁负责审批该事项？"]},`+
			`{"record_id":"chunk-1","questions":["办理时限不得超过`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, report.Results["chunk-0"])
	require.Equal(t, []string{"chunk-1"}, report.MissingRecordIDs)
}

func TestGenerateQuestionsBatchUsesOneStructuredModelCall(t *testing.T) {
	model := &questionBatchChatStub{
		response: `{"results":[` +
			`{"record_id":"chunk-0","questions":["审批职责由谁承担？"]},` +
			`{"record_id":"chunk-1","questions":["办理时限不得超过多久？"]}` +
			`]}`,
	}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `Rules {{context}} ` +
				`<main_content>{{content}}</main_content> ` +
				`{{question_count}} {{doc_name}} {{language}} {{output_instructions}}`,
		},
	}}
	result, coverage, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{
			{RecordID: "chunk-0", Content: "审批由业务部门负责。"},
			{RecordID: "chunk-1", Content: "办理时限不得超过五个工作日。"},
		},
		"前置上下文",
		"后续上下文",
		"测试制度",
		2,
	)
	require.NoError(t, err)
	require.Zero(t, coverage.UnresolvedEligible)
	require.Equal(t, 1, model.calls)
	require.Equal(t, []string{"审批职责由谁承担？"}, result["chunk-0"])
	require.Equal(t, []string{"办理时限不得超过多久？"}, result["chunk-1"])
	require.NotNil(t, model.options)
	require.NotEmpty(t, model.options.Format)
	require.GreaterOrEqual(t, model.options.MaxTokens, 1024)
	require.Len(t, model.messages, 1)
	require.True(t, strings.Contains(model.messages[0].Content, `"record_id":"chunk-0"`))
	require.Contains(t, string(model.options.Format), `"enum":["chunk-0","chunk-1"]`)
	require.True(t, strings.Contains(model.messages[0].Content, "Batch Execution Rules"))
}

func TestGenerateQuestionsBatchRecoversOnlyOmittedRecordsOnce(t *testing.T) {
	model := &questionBatchChatStub{responses: []string{
		`{"results":[{"record_id":"chunk-0","questions":["审批职责由谁承担？"]}]}`,
		`{"results":[{"record_id":"chunk-1","questions":[]}]}`,
	}}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `Rules {{context}} ` +
				`<main_content>{{content}}</main_content> ` +
				`{{question_count}} {{doc_name}} {{language}} {{output_instructions}}`,
		},
	}}
	result, coverage, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{
			{RecordID: "chunk-0", Content: "审批由业务部门负责。"},
			{RecordID: "chunk-1", Content: "空白表单。"},
		},
		"",
		"",
		"测试制度",
		2,
	)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)
	require.Equal(t, 1, coverage.LowInformation)
	require.Equal(t, []string{"审批职责由谁承担？"}, result["chunk-0"])
	require.Contains(t, result, "chunk-1")
	require.Empty(t, result["chunk-1"])
	require.Contains(t, model.messages[0].Content, `"record_id":"chunk-1"`)
}

func TestGenerateQuestionsBatchRepeatedOmissionBecomesSemanticEmpty(t *testing.T) {
	model := &questionBatchChatStub{responses: []string{
		`{"results":[]}`,
		`{"results":[]}`,
	}}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `{{content}} {{output_instructions}}`,
		},
	}}
	result, coverage, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{{RecordID: "chunk-7", Content: "空白模板。"}},
		"",
		"",
		"测试制度",
		1,
	)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)
	require.Equal(t, 1, coverage.LowInformation)
	require.Contains(t, result, "chunk-7")
	require.Empty(t, result["chunk-7"])
}

func TestGenerateQuestionsBatchRecoversExplicitEmptySubstantiveRecord(t *testing.T) {
	model := &questionBatchChatStub{responses: []string{
		`{"results":[{"record_id":"chunk-8","questions":[]}]}`,
		`{"results":[{"record_id":"chunk-8","questions":["该制度规定由哪个部门负责审批？"]}]}`,
	}}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `{{content}} {{output_instructions}}`,
		},
	}}
	result, coverage, err := svc.generateQuestionsBatchWithContext(
		context.Background(), model,
		[]questionBatchInput{{RecordID: "chunk-8", Content: "申请材料应当由综合管理部门在三个工作日内完成审批。"}},
		"", "", "测试制度", 1,
	)
	require.NoError(t, err)
	require.Equal(t, 2, model.calls)
	require.Equal(t, 1, coverage.Eligible)
	require.Equal(t, 1, coverage.Recovered)
	require.Zero(t, coverage.UnresolvedEligible)
	require.Equal(t, []string{"该制度规定由哪个部门负责审批？"}, result["chunk-8"])
	require.Contains(t, model.messages[0].Content, "Coverage Recovery Pass")
}

func TestGenerateQuestionsBatchMarksSubstantiveRepeatedEmptyUnresolved(t *testing.T) {
	model := &questionBatchChatStub{responses: []string{
		`{"results":[{"record_id":"chunk-9","questions":[]}]}`,
		`{"results":[{"record_id":"chunk-9","questions":[]}]}`,
	}}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `{{content}} {{output_instructions}}`,
		},
	}}
	result, coverage, err := svc.generateQuestionsBatchWithContext(
		context.Background(), model,
		[]questionBatchInput{{RecordID: "chunk-9", Content: "合同金额超过十万元时，应由分管负责人审批。"}},
		"", "", "测试制度", 1,
	)
	require.NoError(t, err)
	require.Equal(t, 2, model.calls)
	require.Equal(t, 1, coverage.UnresolvedEligible)
	require.Empty(t, result["chunk-9"])
}

func TestGraphChunkIDBatchesAreBoundedStableAndComplete(t *testing.T) {
	chunks := []*types.Chunk{
		{ID: "c0"},
		{ID: "c1"},
		{ID: "c2"},
		{ID: "c3"},
		{ID: "c4"},
	}
	require.Equal(t, [][]string{
		{"c0", "c1"},
		{"c2", "c3"},
		{"c4"},
	}, graphChunkIDBatches(chunks, 2))
}

func TestGraphNodeSourceChunkIDsPrefersExactSourceAndFallsBackToBatch(t *testing.T) {
	chunks := []*types.Chunk{
		{ID: "c0", Content: "采购部门负责需求审查。"},
		{ID: "c1", Content: "财务部门负责预算复核。"},
	}
	require.Equal(t, []string{"c1"}, graphNodeSourceChunkIDs("财务部门", chunks))
	require.Equal(t, []string{"c0", "c1"}, graphNodeSourceChunkIDs("标准化实体名", chunks))
}
