package handler

import (
	"net/http"
	"os"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// SkillHandler handles skill-related HTTP requests
type SkillHandler struct {
	skillService interfaces.SkillService
}

// NewSkillHandler creates a new skill handler
func NewSkillHandler(skillService interfaces.SkillService) *SkillHandler {
	return &SkillHandler{
		skillService: skillService,
	}
}

// SkillInfoResponse represents the skill info returned to frontend
type SkillInfoResponse struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description"`
	Kind        string `json:"kind,omitempty"`
}

// ListSkills godoc
// @Summary      获取预装Skills列表
// @Description  获取所有预装的Agent Skills元数据
// @Tags         Skills
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "Skills列表"
// @Failure      500  {object}  errors.AppError         "服务器错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills [get]
func (h *SkillHandler) ListSkills(c *gin.Context) {
	ctx := c.Request.Context()

	skillsMetadata, err := h.skillService.ListPreloadedSkills(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to list skills: " + err.Error()))
		return
	}

	// Convert to response format
	var response []SkillInfoResponse
	for _, meta := range skillsMetadata {
		response = append(response, SkillInfoResponse{
			Name:        meta.Name,
			DisplayName: meta.DisplayName,
			Description: meta.Description,
			Kind:        "lightweight",
		})
	}
	professionalMetadata, err := h.skillService.ListProfessionalSkills(ctx)
	if err != nil {
		logger.Warnf(ctx, "Failed to list professional skills: %v", err)
	}
	var professionalResponse []SkillInfoResponse
	for _, meta := range professionalMetadata {
		professionalResponse = append(professionalResponse, SkillInfoResponse{
			Name:        meta.Name,
			DisplayName: meta.DisplayName,
			Description: meta.Description,
			Kind:        "professional",
		})
	}

	// skills_available now controls lightweight prompt skills; these do not
	// require sandbox execution.
	sandboxMode := os.Getenv("WEKNORA_SANDBOX_MODE")
	skillsAvailable := len(response) > 0

	logger.Infof(ctx, "skills_available: %v, sandboxMode: %s", skillsAvailable, sandboxMode)

	c.JSON(http.StatusOK, gin.H{
		"success":                       true,
		"data":                          response,
		"professional_data":             professionalResponse,
		"skills_available":              skillsAvailable,
		"professional_skills_available": len(professionalResponse) > 0,
	})
}
