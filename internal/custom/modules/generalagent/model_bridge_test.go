package generalagent

import (
	"context"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

type repairBridgeChat interface{ chat.Chat }
type repairBridgeModel struct {
	repairBridgeChat
	events   []types.StreamResponse
	options  *chat.ChatOptions
	messages []chat.Message
}

func (m *repairBridgeModel) ChatStream(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	m.options = opts
	m.messages = messages
	ch := make(chan types.StreamResponse, len(m.events))
	for _, event := range m.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}
func TestRepairModelBridgeAuthParametersAndTypedEOF(t *testing.T) {
	t.Setenv("CUSTOM_GENERAL_AGENT_API_KEY", "fixture-key")
	gin.SetMode(gin.TestMode)
	thinking := true
	model := &repairBridgeModel{events: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "exact", Done: true, FinishReason: "stop"}}}
	release := registerActiveRun(&activeRun{runID: "bridge-test", ctx: context.Background(), chatModel: model, agentConfig: &types.AgentConfig{Temperature: .7, Thinking: &thinking, MaxCompletionTokens: 321}})
	defer release()
	router := gin.New()
	router.Use(middleware.Auth(nil, nil, nil, &config.Config{}))
	router.POST("/api/v1/custom/general-agent/internal/model/call", NewHandler(nil).CallModel)
	call := func(auth, run string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/custom/general-agent/internal/model/call", strings.NewReader(`{"run_id":"`+run+`","call_id":"c1","messages":[{"role":"user","content":"exact input"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	require.Equal(t, 401, call("", "bridge-test").Code)
	require.Equal(t, 404, call("Bearer fixture-key", "other-run").Code)
	result := call("Bearer fixture-key", "bridge-test")
	require.Equal(t, 200, result.Code)
	require.Contains(t, result.Body.String(), `"content":"exact"`)
	require.Equal(t, .7, model.options.Temperature)
	require.Equal(t, &thinking, model.options.Thinking)
	require.Equal(t, 321, model.options.MaxCompletionTokens)
	require.Equal(t, "exact input", model.messages[0].Content)
	model.events = []types.StreamResponse{{ResponseType: types.ResponseTypeThinking, Done: true}, {ResponseType: types.ResponseTypeAnswer, Content: "partial", FinishReason: "stop"}}
	result = call("Bearer fixture-key", "bridge-test")
	require.Contains(t, result.Body.String(), "closed before terminal event")
}

func TestRepairModelRuntimeAdapterUsesExplicitCapability(t *testing.T) {
	model := &types.Model{Name: "configured", Type: types.ModelTypeKnowledgeQA, Parameters: types.ModelParameters{Provider: "openai", APIKey: "must-stay-in-platform", ExtraConfig: map[string]string{}}}
	cfg, err := runtimeLLMConfigFromModel(model)
	require.NoError(t, err)
	require.Equal(t, "platform", cfg.RuntimeAdapter)
	require.Empty(t, cfg.APIKey)
	model.Parameters.ExtraConfig["agent_runtime_adapter"] = "claude-sdk"
	_, err = runtimeLLMConfigFromModel(model)
	require.ErrorContains(t, err, "Anthropic")
	model.Parameters.ExtraConfig["agent_runtime_adapter"] = "invented"
	_, err = runtimeLLMConfigFromModel(model)
	require.ErrorContains(t, err, "unsupported")
}
