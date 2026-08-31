package agentresponse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type responseRepairChat struct {
	responses []*types.ChatResponse
	errors    []error
	calls     int
	messages  [][]chat.Message
	options   []*chat.ChatOptions
}

func (c *responseRepairChat) Chat(
	_ context.Context,
	messages []chat.Message,
	opts *chat.ChatOptions,
) (*types.ChatResponse, error) {
	c.messages = append(c.messages, append([]chat.Message(nil), messages...))
	if opts == nil {
		c.options = append(c.options, nil)
	} else {
		copyOptions := *opts
		c.options = append(c.options, &copyOptions)
	}
	index := c.calls
	c.calls++
	if index < len(c.errors) && c.errors[index] != nil {
		return nil, c.errors[index]
	}
	if index >= len(c.responses) {
		return nil, errors.New("unexpected response-repair chat call")
	}
	return c.responses[index], nil
}

func (*responseRepairChat) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, errors.New("response repair must not stream")
}

func (*responseRepairChat) GetModelName() string { return "repair-test" }
func (*responseRepairChat) GetModelID() string   { return "repair-test-id" }

func repairTestReference() *types.SearchResult {
	return &types.SearchResult{
		ID:              "chunk-1",
		KnowledgeID:     "document-1",
		KnowledgeBaseID: "kb-1",
		KnowledgeTitle:  "制度文档",
		ChunkType:       string(types.ChunkTypeText),
		Content:         "审批通过后更新项目计划并归档。",
		EvidenceContent: "审批通过后更新项目计划并归档。",
		Metadata: map[string]string{
			sourcerefs.MetadataCitationID: "S1",
			sourcerefs.MetadataChunkID:    "chunk-1",
			"source_type":                 sourcerefs.SourceTypeKnowledge,
		},
	}
}

func TestRepairResponseProductionPathDoesNothing(t *testing.T) {
	model := &responseRepairChat{}
	draft := strings.Repeat("长", 200)
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars: 0,
		Draft:            draft,
	})

	require.Equal(t, draft, result.Answer)
	require.False(t, result.Attempted)
	require.Zero(t, model.calls)
}

func TestRepairResponseSkipsAlreadyValidDraft(t *testing.T) {
	model := &responseRepairChat{}
	draft := "审批通过后更新项目计划。<src id=\"S1\" />"
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars: 200,
		Draft:            draft,
		References:       []*types.SearchResult{repairTestReference()},
		RequireCitation:  true,
	})

	require.Equal(t, draft, result.Answer)
	require.False(t, result.Attempted)
	require.Zero(t, model.calls)
}

func TestRepairResponseAcceptsBoundedToolFreeRewrite(t *testing.T) {
	model := &responseRepairChat{responses: []*types.ChatResponse{{
		Content:      "审批通过后更新项目计划并归档。<src id=\"S1\" />",
		FinishReason: "stop",
	}}}
	draft := strings.Repeat("过长内容", 80) + "<src id=\"S1\" />"
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars:    100,
		MaxCompletionTokens: 512,
		Query:               "请简要说明审批后的材料更新要求并给出引用。",
		UserStatements:      []string{"这是建设阶段。"},
		Draft:               draft,
		References:          []*types.SearchResult{repairTestReference()},
		RequireCitation:     true,
	})

	require.True(t, result.Attempted)
	require.True(t, result.Repaired)
	require.Equal(t, 1, result.Attempts)
	require.Equal(t, "审批通过后更新项目计划并归档。<src id=\"S1\" />", result.Answer)
	require.Equal(t, 1, model.calls)
	require.Equal(t, "none", model.options[0].ToolChoice)
	require.Empty(t, model.options[0].Tools)
	require.NotNil(t, model.options[0].Thinking)
	require.False(t, *model.options[0].Thinking)
	prompt := model.messages[0][0].Content + model.messages[0][1].Content
	require.Contains(t, prompt, "at most 100 Unicode characters")
	require.Contains(t, prompt, "[CURRENT_TURN_EVIDENCE]")
	require.NotContains(t, prompt, "reference answer")
	require.NotContains(t, prompt, "required claims")
}

func TestRepairResponseRepairsUnknownCitationHandle(t *testing.T) {
	model := &responseRepairChat{responses: []*types.ChatResponse{{
		Content: "审批通过后更新项目计划并归档。<src id=\"S1\" />",
	}}}
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars: 200,
		Query:            "请给出引用。",
		Draft:            "审批通过后更新项目计划。<src id=\"S9\" />",
		References:       []*types.SearchResult{repairTestReference()},
		RequireCitation:  true,
	})

	require.True(t, result.Repaired)
	require.Equal(t, 1, model.calls)
	require.NotContains(t, result.Answer, "S9")
	require.Contains(t, result.Answer, "<src id=\"S1\" />")
}

func TestRepairResponseKeepsOriginalWhenAllAttemptsInvalid(t *testing.T) {
	model := &responseRepairChat{responses: []*types.ChatResponse{
		{Content: strings.Repeat("仍然过长", 80)},
		{Content: "没有任何有效引用"},
	}}
	draft := strings.Repeat("原始答案", 80) + "<src id=\"S1\" />"
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars: 100,
		Query:            "请给出引用。",
		Draft:            draft,
		References:       []*types.SearchResult{repairTestReference()},
		RequireCitation:  true,
	})

	require.True(t, result.Attempted)
	require.False(t, result.Repaired)
	require.Equal(t, maxResponseRepairAttempts, result.Attempts)
	require.Equal(t, draft, result.Answer)
	require.Equal(t, 2, model.calls)
	require.NotEmpty(t, result.LastError)
}

func TestRepairResponseModelFailureIsFailOpen(t *testing.T) {
	model := &responseRepairChat{errors: []error{errors.New("temporary model failure")}}
	draft := strings.Repeat("原始答案", 80)
	result := RepairResponse(context.Background(), model, ResponseRepairRequest{
		MaxResponseChars: 100,
		Draft:            draft,
	})

	require.True(t, result.Attempted)
	require.False(t, result.Repaired)
	require.Equal(t, draft, result.Answer)
	require.Equal(t, "temporary model failure", result.LastError)
}
