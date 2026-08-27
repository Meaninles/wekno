package agenteval

import (
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	config          Config
	recorderEnabled func() bool
}

func NewHandler(config Config, recorderEnabled func() bool) *Handler {
	return &Handler{config: config, recorderEnabled: recorderEnabled}
}

func buildRevision() string {
	if value := strings.TrimSpace(os.Getenv("GIT_COMMIT")); value != "" {
		return value
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

// Capabilities is a read-only handshake for the external eval runner. It
// exposes no credentials, data, prompts or control action. The runner refuses
// to execute when mode is not "eval".
func (h *Handler) Capabilities(c *gin.Context) {
	recorderEnabled := false
	if h.recorderEnabled != nil {
		recorderEnabled = h.recorderEnabled()
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"schema_version":   1,
			"mode":             h.config.Mode,
			"capture_policy":   h.config.CapturePolicy,
			"recorder_enabled": recorderEnabled,
			"environment":      strings.TrimSpace(os.Getenv("LANGFUSE_ENVIRONMENT")),
			"release":          strings.TrimSpace(os.Getenv("LANGFUSE_RELEASE")),
			"commit":           buildRevision(),
			"capabilities": []string{
				"rag_retrieval",
				"document_processing",
				"long_context_dialogue",
				"tool_use",
				"citation",
			},
			"agent_types": []string{
				"simple-chat",
				"smart-reasoning",
				"general-agent",
				"document-processing-agent",
				"data-analysis",
				"table-analysis",
				"knowledge-base-manager",
			},
		},
	})
}
