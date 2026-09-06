package session

import (
	"context"
	"fmt"
	"mime"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type UploadResolver interface {
	Resolve(context.Context, string, []string) (types.MessageAttachments, []string, error)
	HistoryTargets(context.Context, string) ([]string, error)
}

func (h *Handler) SetUploadResolver(resolver UploadResolver) { h.uploadResolver = resolver }

// ResolveUploadAgent uses exactly the effective agent selection used by chat.
func (h *Handler) ResolveUploadAgent(c *gin.Context, id, filename string) (*types.CustomAgent, error) {
	if bound := c.GetString("chat_upload_agent_id"); bound != "" {
		id = bound
	}
	agent, _ := h.resolveAgent(c.Request.Context(), c, id)
	if agent == nil {
		return nil, fmt.Errorf("selected agent is not accessible")
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	if strings.HasPrefix(contentType, "image/") {
		if !agent.Config.ImageUploadEnabled {
			return nil, fmt.Errorf("image upload is not enabled for this agent")
		}
		if strings.TrimSpace(agent.Config.VLMModelID) == "" {
			return nil, fmt.Errorf("a vision model is required to process images")
		}
	} else if !attachmentFileTypeAllowed(filename, agent.Config.SupportedFileTypes) {
		return nil, fmt.Errorf("file type is not supported by the selected agent")
	}
	return agent, nil
}
