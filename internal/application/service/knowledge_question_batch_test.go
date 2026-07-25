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
			ID: "kb-1", TenantID: 42, SummaryModelID: "chat-1",
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
		{ChunkIndex: 0, Content: "content zero"},
		{ChunkIndex: 3, Content: "content three"},
	}
	raw := "```json\n" +
		`{"results":[` +
		`{"chunk_index":0,"questions":["1. 谁负责审批该事项？","谁负责审批该事项？"]},` +
		`{"chunk_index":3,"questions":["超过期限后应如何处理？","哪些情形属于例外？","额外问题不会被保留？"]}` +
		`]}` +
		"\n```"
	result, err := parseQuestionBatchResponse(raw, inputs, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, result[0])
	require.Equal(t, []string{"超过期限后应如何处理？", "哪些情形属于例外？"}, result[3])
}

func TestParseQuestionBatchResponseRejectsUnknownOrDuplicateIndex(t *testing.T) {
	inputs := []questionBatchInput{{ChunkIndex: 0, Content: "content"}}
	_, err := parseQuestionBatchResponse(
		`{"results":[{"chunk_index":9,"questions":["有效问题是什么？"]}]}`,
		inputs,
		2,
	)
	require.ErrorContains(t, err, "unknown chunk_index")

	_, err = parseQuestionBatchResponse(
		`{"results":[`+
			`{"chunk_index":0,"questions":["第一个有效问题是什么？"]},`+
			`{"chunk_index":0,"questions":["第二个有效问题是什么？"]}]}`,
		inputs,
		2,
	)
	require.ErrorContains(t, err, "repeats chunk_index")
}

func TestParseQuestionBatchResponseDistinguishesExplicitEmptyFromOmission(t *testing.T) {
	inputs := []questionBatchInput{
		{ChunkIndex: 0, Content: "useful"},
		{ChunkIndex: 1, Content: "blank form"},
		{ChunkIndex: 2, Content: "omitted"},
	}
	result, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"chunk_index":0,"questions":["谁负责审批该事项？"]},`+
			`{"chunk_index":1,"questions":[]}]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, result[0])
	require.Contains(t, result, 1)
	require.Empty(t, result[1])
	require.NotContains(t, result, 2)
}

func TestParseQuestionBatchResponseAcceptsExplicitQuestionObjects(t *testing.T) {
	inputs := []questionBatchInput{
		{ChunkIndex: 0, Content: "content zero"},
		{ChunkIndex: 1, Content: "content one"},
	}
	result, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"chunk_index":0,"questions":[{"question":"谁负责审批该事项？","answer":"业务部门"}]},`+
			`{"chunk_index":1,"questions":["办理时限不得超过多久？",{"text":"哪些情形属于例外？"}]}`+
			`]}`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, result[0])
	require.Equal(t, []string{"办理时限不得超过多久？", "哪些情形属于例外？"}, result[1])
}

func TestParseQuestionBatchResponseRejectsAmbiguousQuestionObjects(t *testing.T) {
	inputs := []questionBatchInput{{ChunkIndex: 0, Content: "content"}}
	_, err := parseQuestionBatchResponse(
		`{"results":[{"chunk_index":0,"questions":[`+
			`{"question":"谁负责审批该事项？","text":"办理时限是多久？"}`+
			`]}]}`,
		inputs,
		2,
	)
	require.ErrorContains(t, err, "conflicting text aliases")

	_, err = parseQuestionBatchResponse(
		`{"results":[{"chunk_index":0,"questions":[{"answer":"业务部门"}]}]}`,
		inputs,
		2,
	)
	require.ErrorContains(t, err, "must contain")
}

func TestParseQuestionBatchResponseRecoversCompleteItemsFromTruncatedTail(t *testing.T) {
	inputs := []questionBatchInput{
		{ChunkIndex: 0, Content: "content zero"},
		{ChunkIndex: 1, Content: "content one"},
	}
	result, err := parseQuestionBatchResponse(
		`{"results":[`+
			`{"chunk_index":0,"questions":["谁负责审批该事项？"]},`+
			`{"chunk_index":1,"questions":["办理时限不得超过`,
		inputs,
		2,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"谁负责审批该事项？"}, result[0])
	require.NotContains(t, result, 1)
}

func TestGenerateQuestionsBatchUsesOneStructuredModelCall(t *testing.T) {
	model := &questionBatchChatStub{
		response: `{"results":[` +
			`{"chunk_index":0,"questions":["审批职责由谁承担？"]},` +
			`{"chunk_index":1,"questions":["办理时限不得超过多久？"]}` +
			`]}`,
	}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `Rules {{context}} ` +
				`<main_content>{{content}}</main_content> ` +
				`{{question_count}} {{doc_name}} {{language}} {{output_instructions}}`,
		},
	}}
	result, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{
			{ChunkIndex: 0, Content: "审批由业务部门负责。"},
			{ChunkIndex: 1, Content: "办理时限不得超过五个工作日。"},
		},
		"前置上下文",
		"后续上下文",
		"测试制度",
		2,
	)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)
	require.Equal(t, []string{"审批职责由谁承担？"}, result[0])
	require.Equal(t, []string{"办理时限不得超过多久？"}, result[1])
	require.NotNil(t, model.options)
	require.NotEmpty(t, model.options.Format)
	require.GreaterOrEqual(t, model.options.MaxTokens, 1024)
	require.Len(t, model.messages, 1)
	require.True(t, strings.Contains(model.messages[0].Content, `"chunk_index":0`))
	require.True(t, strings.Contains(model.messages[0].Content, "Batch Execution Rules"))
}

func TestGenerateQuestionsBatchRecoversOnlyOmittedRecordsOnce(t *testing.T) {
	model := &questionBatchChatStub{responses: []string{
		`{"results":[{"chunk_index":0,"questions":["审批职责由谁承担？"]}]}`,
		`{"results":[{"chunk_index":1,"questions":[]}]}`,
	}}
	svc := &knowledgeService{config: &config.Config{
		Conversation: &config.ConversationConfig{
			GenerateQuestionsPrompt: `Rules {{context}} ` +
				`<main_content>{{content}}</main_content> ` +
				`{{question_count}} {{doc_name}} {{language}} {{output_instructions}}`,
		},
	}}
	result, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{
			{ChunkIndex: 0, Content: "审批由业务部门负责。"},
			{ChunkIndex: 1, Content: "空白表单。"},
		},
		"",
		"",
		"测试制度",
		2,
	)
	require.NoError(t, err)
	require.Equal(t, 2, model.calls)
	require.Equal(t, []string{"审批职责由谁承担？"}, result[0])
	require.Contains(t, result, 1)
	require.Empty(t, result[1])
	require.Contains(t, model.messages[0].Content, `"chunk_index":1`)
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
	result, err := svc.generateQuestionsBatchWithContext(
		context.Background(),
		model,
		[]questionBatchInput{{ChunkIndex: 7, Content: "空白模板。"}},
		"",
		"",
		"测试制度",
		1,
	)
	require.NoError(t, err)
	require.Equal(t, 2, model.calls)
	require.Contains(t, result, 7)
	require.Empty(t, result[7])
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
