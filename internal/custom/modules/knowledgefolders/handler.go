package knowledgefolders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service          *Service
	knowledgeService interfaces.KnowledgeService
}

func NewHandler(service *Service, knowledgeService interfaces.KnowledgeService) *Handler {
	return &Handler{service: service, knowledgeService: knowledgeService}
}

func (h *Handler) CreateFolder(c *gin.Context) {
	var req CreateFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("文件夹参数格式错误").WithDetails(err.Error()))
		return
	}
	folder, err := h.service.CreateFolder(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": folder})
}

func (h *Handler) UpdateFolder(c *gin.Context) {
	var req UpdateFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("文件夹参数格式错误").WithDetails(err.Error()))
		return
	}
	folder, err := h.service.UpdateFolder(
		c.Request.Context(), c.Param("id"), c.Param("folder_id"), req,
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": folder})
}

func (h *Handler) DeleteFolder(c *gin.Context) {
	if err := h.service.DeleteFolder(
		c.Request.Context(), c.Param("id"), c.Param("folder_id"), c.DefaultQuery("mode", "reject"),
	); err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) GetFolder(c *gin.Context) {
	folder, err := h.service.GetFolder(c.Request.Context(), c.Param("id"), c.Param("folder_id"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": folder})
}

func (h *Handler) ListFolderOptions(c *gin.Context) {
	folders, err := h.service.ListFolderOptions(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": folders})
}

func (h *Handler) ListNodes(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		h.writeError(c, ErrInvalidPage)
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(DefaultPageSize)))
	if err != nil {
		h.writeError(c, ErrInvalidPage)
		return
	}
	filter, err := parseKnowledgeFilter(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	result, err := h.service.ListNodes(
		c.Request.Context(),
		c.Param("id"),
		c.Query("folder_id"),
		page,
		pageSize,
		filter,
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"data":        result.Data,
		"total":       result.Total,
		"page":        result.Page,
		"page_size":   result.PageSize,
		"current":     result.Current,
		"breadcrumbs": result.Breadcrumbs,
	})
}

func (h *Handler) SearchKnowledgeBase(c *gin.Context) {
	page, pageSize, err := parsePageQuery(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	filter, err := parseKnowledgeFilter(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	result, err := h.service.SearchKnowledgeBase(
		c.Request.Context(),
		c.Param("id"),
		strings.TrimSpace(c.Query("folder_id")),
		strings.TrimSpace(c.Query("keyword")),
		page,
		pageSize,
		filter,
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true, "data": result.Data, "total": result.Total,
		"page": result.Page, "page_size": result.PageSize,
	})
}

func (h *Handler) SearchAccessible(c *gin.Context) {
	page, pageSize, err := parsePageQuery(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	result, err := h.service.SearchAccessible(
		c.Request.Context(),
		strings.TrimSpace(c.Query("keyword")),
		page,
		pageSize,
		splitNonEmpty(c.Query("knowledge_base_ids")),
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true, "data": result.Data, "total": result.Total,
		"page": result.Page, "page_size": result.PageSize,
	})
}

func parsePageQuery(c *gin.Context) (int, int, error) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		return 0, 0, ErrInvalidPage
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(DefaultPageSize)))
	if err != nil {
		return 0, 0, ErrInvalidPage
	}
	return page, pageSize, nil
}

func parseKnowledgeFilter(c *gin.Context) (types.KnowledgeListFilter, error) {
	filter := types.KnowledgeListFilter{
		TagIDs:         splitNonEmpty(c.Query("tag_ids")),
		Keyword:        strings.TrimSpace(c.Query("keyword")),
		FileType:       strings.TrimSpace(c.Query("file_type")),
		ParseStatus:    strings.TrimSpace(c.Query("parse_status")),
		WorkflowStatus: strings.TrimSpace(c.Query("workflow_status")),
		Source:         strings.TrimSpace(c.Query("source")),
	}
	var err error
	if raw := strings.TrimSpace(c.Query("start_time")); raw != "" {
		filter.UpdatedFrom, err = parseFilterTime(raw)
		if err != nil {
			return filter, apperrors.NewBadRequestError("start_time 格式错误")
		}
	}
	if raw := strings.TrimSpace(c.Query("end_time")); raw != "" {
		filter.UpdatedTo, err = parseFilterTime(raw)
		if err != nil {
			return filter, apperrors.NewBadRequestError("end_time 格式错误")
		}
	}
	return filter, nil
}

func parseFilterTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			if layout == "2006-01-02" {
				return parsed, nil
			}
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("unsupported time format")
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (h *Handler) MoveDocuments(c *gin.Context) {
	var req MoveDocumentsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("文档移动参数格式错误").WithDetails(err.Error()))
		return
	}
	affected, err := h.service.MoveDocuments(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"affected": affected}})
}

func (h *Handler) UploadFile(c *gin.Context) {
	ctx := c.Request.Context()
	kbID := c.Param("id")
	file, err := c.FormFile("file")
	if err != nil {
		c.Error(apperrors.NewBadRequestError("文件上传失败").WithDetails(err.Error()))
		return
	}
	maxSizeMB := utils.GetMaxKnowledgeSourceFileSizeMB()
	if file.Size > maxSizeMB*1024*1024 {
		c.Error(apperrors.NewBadRequestError(fmt.Sprintf("文件大小不能超过%dMB", maxSizeMB)))
		return
	}
	targetFolderID := strings.TrimSpace(c.PostForm("folder_id"))
	if targetFolderID != "" {
		if _, err := h.service.GetFolder(ctx, kbID, targetFolderID); err != nil {
			h.writeError(c, err)
			return
		}
	}
	customFileName := strings.TrimSpace(c.PostForm("fileName"))
	if customFileName == "" {
		customFileName = file.Filename
	}
	if relativePath := strings.TrimSpace(c.PostForm("relative_path")); relativePath != "" {
		directories, baseName, err := parseRelativeUploadPath(relativePath)
		if err != nil {
			h.writeError(c, err)
			return
		}
		targetFolderID, err = h.service.EnsurePath(ctx, kbID, targetFolderID, directories)
		if err != nil {
			h.writeError(c, err)
			return
		}
		customFileName = baseName
	} else if strings.ContainsAny(customFileName, `/\`) {
		c.Error(apperrors.NewBadRequestError("文件名不能包含目录分隔符，请通过 relative_path 上传文件夹"))
		return
	}

	var metadata map[string]string
	if raw := c.PostForm("metadata"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			c.Error(apperrors.NewBadRequestError("metadata 格式错误").WithDetails(err.Error()))
			return
		}
	}
	var enableMultimodel *bool
	if raw := c.PostForm("enable_multimodel"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			c.Error(apperrors.NewBadRequestError("enable_multimodel 格式错误"))
			return
		}
		enableMultimodel = &value
	}
	var processOverrides *types.KnowledgeProcessOverrides
	if raw := c.PostForm("process_config"); raw != "" {
		processOverrides = &types.KnowledgeProcessOverrides{}
		if err := json.Unmarshal([]byte(raw), processOverrides); err != nil {
			c.Error(apperrors.NewBadRequestError("process_config 格式错误").WithDetails(err.Error()))
			return
		}
	}
	if enableMultimodel != nil && (processOverrides == nil || processOverrides.EnableMultimodel == nil) {
		if processOverrides == nil {
			processOverrides = &types.KnowledgeProcessOverrides{}
		}
		processOverrides.EnableMultimodel = enableMultimodel
	}
	ctx = context.WithValue(ctx, types.KnowledgeFolderIDContextKey, targetFolderID)
	knowledge, err := h.knowledgeService.CreateKnowledgeFromFile(
		ctx,
		kbID,
		file,
		metadata,
		enableMultimodel,
		customFileName,
		splitNonEmpty(c.PostForm("tag_ids")),
		c.PostForm("channel"),
		processOverrides,
	)
	if err != nil {
		if duplicate, ok := err.(*types.DuplicateKnowledgeError); ok {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": duplicate.Error(),
				"code":    "duplicate_file",
				"data":    duplicate.Knowledge,
			})
			return
		}
		if appError, ok := apperrors.IsAppError(err); ok {
			c.Error(appError)
			return
		}
		c.Error(apperrors.NewInternalServerError("创建知识文档失败").WithDetails(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": knowledge})
}

func (h *Handler) CreateFromURL(c *gin.Context) {
	var req URLKnowledgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("URL 参数格式错误").WithDetails(err.Error()))
		return
	}
	ctx := c.Request.Context()
	kbID := c.Param("id")
	if err := utils.ValidateURLForSSRF(req.URL); err != nil {
		c.Error(apperrors.NewBadRequestError(utils.FormatSSRFError("URL", req.URL, err)))
		return
	}
	if strings.ContainsAny(req.FileName, `/\`) {
		c.Error(apperrors.NewBadRequestError("file_name 不能包含目录分隔符"))
		return
	}
	folderID, err := h.validateTargetFolder(ctx, kbID, req.FolderID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	ctx = context.WithValue(ctx, types.KnowledgeFolderIDContextKey, folderID)
	knowledge, err := h.knowledgeService.CreateKnowledgeFromURL(
		ctx,
		kbID,
		req.URL,
		req.FileName,
		req.FileType,
		req.EnableMultimodel,
		req.Title,
		req.TagIDs,
		req.Channel,
		req.ProcessConfig,
	)
	if err != nil {
		h.writeKnowledgeCreateError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": knowledge})
}

func (h *Handler) CreateManual(c *gin.Context) {
	var req ManualKnowledgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("手工文档参数格式错误").WithDetails(err.Error()))
		return
	}
	ctx := c.Request.Context()
	kbID := c.Param("id")
	folderID, err := h.validateTargetFolder(ctx, kbID, req.FolderID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	ctx = context.WithValue(ctx, types.KnowledgeFolderIDContextKey, folderID)
	knowledge, err := h.knowledgeService.CreateKnowledgeFromManual(
		ctx, kbID, &req.ManualKnowledgePayload, req.Channel,
	)
	if err != nil {
		h.writeKnowledgeCreateError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": knowledge})
}

func (h *Handler) validateTargetFolder(ctx context.Context, kbID, folderID string) (string, error) {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return "", nil
	}
	if _, err := h.service.GetFolder(ctx, kbID, folderID); err != nil {
		return "", err
	}
	return folderID, nil
}

func (h *Handler) writeKnowledgeCreateError(c *gin.Context, err error) {
	if duplicate, ok := err.(*types.DuplicateKnowledgeError); ok {
		c.JSON(http.StatusConflict, gin.H{
			"success": false, "message": duplicate.Error(),
			"code": "duplicate_file", "data": duplicate.Knowledge,
		})
		return
	}
	if appError, ok := apperrors.IsAppError(err); ok {
		c.Error(appError)
		return
	}
	c.Error(apperrors.NewInternalServerError("创建知识文档失败").WithDetails(err.Error()))
}

func parseRelativeUploadPath(value string) ([]string, string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	value = strings.Trim(value, "/")
	if value == "" || strings.Contains(value, ":") {
		return nil, "", apperrors.NewBadRequestError("relative_path 格式错误")
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return nil, "", apperrors.NewBadRequestError("relative_path 必须包含目录和文件名")
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || part == "." || part == ".." {
			return nil, "", apperrors.NewBadRequestError("relative_path 包含非法路径段")
		}
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrFolderNotFound):
		c.Error(apperrors.NewNotFoundError("文件夹不存在"))
	case errors.Is(err, ErrFolderNameInvalid):
		c.Error(apperrors.NewValidationError("文件夹名称为空、过长或包含非法字符"))
	case errors.Is(err, ErrFolderNameExists):
		c.Error(apperrors.NewConflictError("同一级已存在同名文件夹"))
	case errors.Is(err, ErrFolderDepth):
		c.Error(apperrors.NewValidationError("文件夹层级超过限制"))
	case errors.Is(err, ErrFolderCycle):
		c.Error(apperrors.NewValidationError("不能将文件夹移动到自身或其子文件夹中"))
	case errors.Is(err, ErrFolderNotEmpty):
		c.Error(apperrors.NewConflictError("文件夹非空，请选择将内容移动到上一级后删除"))
	case errors.Is(err, ErrFolderDeleteMode):
		c.Error(apperrors.NewBadRequestError("文件夹删除模式仅支持 reject 或 move_to_parent"))
	case errors.Is(err, ErrDocumentNotFound):
		c.Error(apperrors.NewNotFoundError("部分文档不存在或不属于当前知识库"))
	case errors.Is(err, ErrInvalidPage):
		c.Error(apperrors.NewBadRequestError("分页参数必须为正整数"))
	default:
		if appError, ok := apperrors.IsAppError(err); ok {
			c.Error(appError)
			return
		}
		c.Error(apperrors.NewInternalServerError("文件夹操作失败").WithDetails(err.Error()))
	}
}
