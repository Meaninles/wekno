package sessiontitle

import (
	"context"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

type Handler struct{ sessions interfaces.SessionService }

func NewHandler(sessions interfaces.SessionService) *Handler { return &Handler{sessions: sessions} }

// Register exposes only title metadata. The service applies the exact session
// owner scope; embedded callers additionally pass the signed-session guard.
func (h *Handler) Register(group *gin.RouterGroup) {
	group.GET("/session-titles/:session_id", h.read)
	group.POST("/session-titles/:session_id", h.repair)
}

func (h *Handler) read(c *gin.Context) {
	session, err := h.sessions.GetSession(c.Request.Context(), c.Param("session_id"))
	if err != nil || session == nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"session_id": session.ID, "title": session.Title}})
}

// This explicit recovery is only requested after the normal async attempt's
// deadline. It uses the persisted first user message and the existing CAS, so
// a delayed response never overwrites a manual rename.
func (h *Handler) repair(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	session, err := h.sessions.GetSession(ctx, c.Param("session_id"))
	if err != nil || session == nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false})
		return
	}
	title, err := h.sessions.GenerateTitle(ctx, session, nil, "")
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"session_id": session.ID, "title": title}})
}
