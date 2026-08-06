package capacitycontrol

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Effective(c *gin.Context) {
	report, err := h.service.Compile(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": report})
}

type validationResult struct {
	Valid     bool                         `json:"valid"`
	Canonical *modeladmission.ResourcePool `json:"canonical,omitempty"`
	Report    *PoolReport                  `json:"report,omitempty"`
	Issues    []Issue                      `json:"issues"`
}

func (h *Handler) Validate(c *gin.Context) {
	var pool modeladmission.ResourcePool
	if err := c.ShouldBindJSON(&pool); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := modeladmission.NormalizePool(&pool); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": validationResult{
			Valid:  false,
			Issues: []Issue{{Severity: "error", Code: "invalid_capacity", Scope: "pool:" + pool.ID, Message: err.Error()}},
		}})
		return
	}
	report := CompilePool(pool, nil)
	valid := true
	for _, issue := range report.Issues {
		if issue.Severity == "error" {
			valid = false
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": validationResult{
		Valid: valid, Canonical: &pool, Report: &report, Issues: report.Issues,
	}})
}
