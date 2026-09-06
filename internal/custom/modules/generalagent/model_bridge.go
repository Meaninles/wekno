package generalagent

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// CallModel keeps credentials, provider adaptation and effective parameters in
// the platform. Sidecars own workspace execution, never a second model config.
func (h *Handler) CallModel(c *gin.Context) {
	if !validInternalAPIKey(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		RunID    string         `json:"run_id"`
		CallID   string         `json:"call_id"`
		Messages []chat.Message `json:"messages"`
		Tools    []chat.Tool    `json:"tools"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	run := lookupActiveRun(req.RunID)
	if run == nil || run.chatModel == nil || run.agentConfig == nil {
		c.JSON(404, gin.H{"error": "model run is not active"})
		return
	}
	if len(req.Messages) == 0 || len(req.Tools) > 128 {
		c.JSON(400, gin.H{"error": "invalid message or tool count"})
		return
	}
	ctx, cancel := context.WithCancel(run.ctx)
	defer cancel()
	stop := context.AfterFunc(c.Request.Context(), cancel)
	defer stop()
	timeout := time.Duration(run.agentConfig.LLMCallTimeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	opts := &chat.ChatOptions{Temperature: run.agentConfig.Temperature, Thinking: run.agentConfig.Thinking,
		MaxCompletionTokens: run.agentConfig.MaxCompletionTokens, Tools: req.Tools}
	messages, budgetErr := conversationmemory.BoundMessages(ctx, req.Messages, req.Tools, run.agentConfig.MaxContextTokens, opts.MaxCompletionTokens)
	if budgetErr != nil {
		c.JSON(400, gin.H{"error": budgetErr.Error()})
		return
	}
	stream, err := run.chatModel.ChatStream(ctx, messages, opts)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Model-Call-ID", req.CallID)
	enc := json.NewEncoder(c.Writer)
	done := false
	for {
		select {
		case <-ctx.Done():
			_ = enc.Encode(types.StreamResponse{ResponseType: types.ResponseTypeError, Content: ctx.Err().Error(), Done: true})
			c.Writer.Flush()
			return
		case chunk, ok := <-stream:
			if !ok {
				if !done {
					_ = enc.Encode(types.StreamResponse{ResponseType: types.ResponseTypeError, Content: "model stream closed before terminal event", Done: true})
					c.Writer.Flush()
				}
				return
			}
			done = done || (chunk.Done && chunk.ResponseType != types.ResponseTypeThinking)
			if err := enc.Encode(chunk); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}
