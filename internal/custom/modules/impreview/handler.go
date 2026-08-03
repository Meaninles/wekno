// Package impreview exposes an authenticated, read-only API that renders an
// already completed real message through the exact same final IM boundary used
// in production. It never accepts caller-supplied answer text or references.
package impreview

import (
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/im/wecom"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	imService          *im.Service
	sessionService     interfaces.SessionService
	messageService     interfaces.MessageService
	customAgentService interfaces.CustomAgentService
	tenantService      interfaces.TenantService
}

func NewHandler(
	imService *im.Service,
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	customAgentService interfaces.CustomAgentService,
	tenantService interfaces.TenantService,
) *Handler {
	return &Handler{
		imService:          imService,
		sessionService:     sessionService,
		messageService:     messageService,
		customAgentService: customAgentService,
		tenantService:      tenantService,
	}
}

type previewRequest struct {
	SessionID  string `json:"session_id" binding:"required"`
	MessageID  string `json:"message_id" binding:"required"`
	AgentID    string `json:"agent_id"`
	ChannelID  string `json:"channel_id"`
	Platform   string `json:"platform"`
	Mode       string `json:"mode"`
	OutputMode string `json:"output_mode"`
}

type previewResponse struct {
	AgentID    string             `json:"agent_id"`
	ChannelID  string             `json:"channel_id,omitempty"`
	Platform   string             `json:"platform"`
	Mode       string             `json:"mode"`
	OutputMode string             `json:"output_mode"`
	Streaming  bool               `json:"streaming"`
	Result     interface{}        `json:"result"`
	Payloads   []transportPayload `json:"transport_payloads"`
}

type transportPayload struct {
	Sequence int         `json:"sequence"`
	Kind     string      `json:"kind"`
	Body     interface{} `json:"body"`
}

// Preview renders a persisted assistant message without sending anything to
// an external platform. Session/message services enforce the caller's tenant
// and session visibility before any reference content is returned.
func (h *Handler) Preview(c *gin.Context) {
	if h == nil || h.imService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "IM preview service is unavailable"})
		return
	}
	var req previewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	session, err := h.sessionService.GetSession(ctx, strings.TrimSpace(req.SessionID))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	message, err := h.messageService.GetMessage(ctx, session.ID, strings.TrimSpace(req.MessageID))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if message.Role != "assistant" || !message.IsCompleted {
		c.JSON(http.StatusConflict, gin.H{"error": "message must be a completed assistant response"})
		return
	}

	agentID := strings.TrimSpace(req.AgentID)
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	outputMode := strings.ToLower(strings.TrimSpace(req.OutputMode))
	channelID := strings.TrimSpace(req.ChannelID)
	streaming := false

	if channelID != "" {
		channel, channelErr := h.imService.GetChannelByIDAndTenant(channelID, session.TenantID)
		if channelErr != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "IM channel not found"})
			return
		}
		agentID = channel.AgentID
		platform = channel.Platform
		mode = channel.Mode
		outputMode = channel.OutputMode
		if adapter, _, ok := h.imService.GetChannelAdapter(channelID); ok {
			_, streaming = adapter.(im.StreamSender)
			streaming = streaming && outputMode != "full"
		} else {
			streaming = previewStreaming(platform, mode, outputMode)
		}
	} else {
		if agentID == "" && session.LastRequestState != nil {
			agentID = strings.TrimSpace(session.LastRequestState.AgentID)
		}
		if platform == "" {
			platform = string(im.PlatformWeCom)
		}
		if mode == "" {
			mode = "websocket"
		}
		if outputMode == "" {
			outputMode = "stream"
		}
		streaming = previewStreaming(platform, mode, outputMode)
	}
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id is required when the session has no saved agent"})
		return
	}

	agent, err := h.customAgentService.GetAgentByID(ctx, agentID)
	if err != nil || agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}
	tenant, err := h.tenantService.GetTenantByID(ctx, session.TenantID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}

	result := h.imService.RenderFinalOutbound(
		ctx,
		message.Content,
		[]*types.SearchResult(message.KnowledgeReferences),
		tenant,
		im.Platform(platform),
		streaming,
	)
	c.JSON(http.StatusOK, gin.H{"data": previewResponse{
		AgentID:    agent.ID,
		ChannelID:  channelID,
		Platform:   platform,
		Mode:       mode,
		OutputMode: outputMode,
		Streaming:  streaming,
		Result:     result,
		Payloads:   buildTransportPreview(platform, mode, result.Content),
	}})
}

func buildTransportPreview(platform, mode, content string) []transportPayload {
	if platform != string(im.PlatformWeCom) {
		return []transportPayload{{Sequence: 1, Kind: "final_content", Body: map[string]interface{}{"content": content}}}
	}
	if mode == "webhook" {
		chunks := wecom.SplitApplicationMarkdown(content)
		payloads := make([]transportPayload, 0, len(chunks))
		for index, chunk := range chunks {
			payloads = append(payloads, transportPayload{
				Sequence: index + 1,
				Kind:     "application_markdown",
				Body: map[string]interface{}{
					"msgtype":  "markdown",
					"markdown": map[string]string{"content": chunk},
				},
			})
		}
		return payloads
	}
	return []transportPayload{{
		Sequence: 1,
		Kind:     "bot_stream_final",
		Body:     wecom.NewBotStreamReplyBody("preview-stream", content, true),
	}}
}

func previewStreaming(platform, mode, outputMode string) bool {
	if outputMode == "full" {
		return false
	}
	// A WeCom self-built application uses callback + application-message APIs
	// and has no replace-stream transport. Its intelligent Bot websocket mode
	// does. Other current streaming adapters decide at runtime in production.
	return !(platform == string(im.PlatformWeCom) && mode == "webhook")
}
