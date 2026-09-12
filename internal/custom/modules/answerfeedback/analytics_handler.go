package answerfeedback

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/logger"
)

func (h *Handler) ListAnalytics(c *gin.Context) {
	query, err := parseAnalyticsQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	if h.service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈数据服务不可用"})
		return
	}
	result, err := h.service.ListAnalytics(c.Request.Context(), query)
	if err != nil {
		logger.Errorf(c.Request.Context(), "[answerfeedback] list analytics: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈数据读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) ListAnalyticsGroups(c *gin.Context) {
	query, err := parseAnalyticsQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	dimension := strings.TrimSpace(c.DefaultQuery("dimension", "agent"))
	if h.service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈数据服务不可用"})
		return
	}
	result, err := h.service.ListAnalyticsGroups(c.Request.Context(), query, dimension)
	if err != nil {
		logger.Errorf(c.Request.Context(), "[answerfeedback] list analytics groups: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈统计读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) ListAnalyticsOptions(c *gin.Context) {
	kind := strings.TrimSpace(c.DefaultQuery("type", "agents"))
	limit, err := parseAnalyticsInt(c.Query("limit"), analyticsMaxOptionSize, analyticsMaxOptionSize)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "limit必须是1到100之间的正整数"})
		return
	}
	if h.service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈数据服务不可用"})
		return
	}
	result, err := h.service.ListAnalyticsResourceOptions(c.Request.Context(), kind, c.Query("q"), limit)
	if err != nil {
		logger.Errorf(c.Request.Context(), "[answerfeedback] list analytics options: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "筛选项读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) GetAnalyticsDetail(c *gin.Context) {
	page, err := parseAnalyticsInt(c.Query("page"), 1, 0)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "page必须是正整数"})
		return
	}
	pageSize, err := parseAnalyticsInt(c.Query("page_size"), 50, analyticsMaxPageSize)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "page_size必须是1到100之间"})
		return
	}
	includeConversation := false
	if raw := strings.TrimSpace(c.Query("include_conversation")); raw != "" {
		includeConversation, err = strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "include_conversation参数无效"})
			return
		}
	}
	if h.service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "反馈数据服务不可用"})
		return
	}
	result, err := h.service.GetAnalyticsDetail(c.Request.Context(), c.Param("feedback_id"), page, pageSize, includeConversation)
	if errors.Is(err, ErrAnalyticsFeedbackNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "反馈记录不存在或无权查看"})
		return
	}
	if err != nil {
		logger.Errorf(c.Request.Context(), "[answerfeedback] get analytics detail: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "问答详情读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func parseAnalyticsQuery(c *gin.Context) (AnalyticsQuery, error) {
	page, err := parseAnalyticsInt(c.Query("page"), 1, 0)
	if err != nil {
		return AnalyticsQuery{}, errors.New("page必须是正整数")
	}
	pageSize, err := parseAnalyticsInt(c.Query("page_size"), analyticsDefaultPageSize, analyticsMaxPageSize)
	if err != nil {
		return AnalyticsQuery{}, errors.New("page_size必须是1到100之间")
	}
	agentKeys, err := parseAnalyticsResourceKeys(strings.Join(c.QueryArray("agent_keys"), ","))
	if err != nil {
		return AnalyticsQuery{}, errors.New("agent_keys参数无效")
	}
	knowledgeKeys, err := parseAnalyticsResourceKeys(strings.Join(c.QueryArray("knowledge_base_keys"), ","))
	if err != nil {
		return AnalyticsQuery{}, errors.New("knowledge_base_keys参数无效")
	}
	feedback := strings.TrimSpace(c.Query("feedback"))
	if feedback != "" {
		normalized, ok := normalizeFeedback(feedback)
		if !ok || normalized == FeedbackNone {
			return AnalyticsQuery{}, errors.New("feedback参数无效")
		}
		feedback = normalized
	}
	from, err := parseAnalyticsTime(c.Query("from"), false)
	if err != nil {
		return AnalyticsQuery{}, errors.New("from时间格式无效")
	}
	to, err := parseAnalyticsTime(c.Query("to"), true)
	if err != nil {
		return AnalyticsQuery{}, errors.New("to时间格式无效")
	}
	return AnalyticsQuery{
		Page: page, PageSize: pageSize,
		UserID:    strings.TrimSpace(c.Query("user_id")),
		UserQuery: strings.TrimSpace(c.Query("user_q")),
		AgentKeys: agentKeys, AgentQuery: strings.TrimSpace(c.Query("agent_q")),
		KnowledgeBaseKeys:  knowledgeKeys,
		KnowledgeBaseQuery: strings.TrimSpace(c.Query("knowledge_base_q")),
		Feedback:           feedback, Channel: strings.TrimSpace(c.Query("channel")),
		From: from, To: to,
	}, nil
}

func parseAnalyticsInt(raw string, fallback, maximum int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || (maximum > 0 && value > maximum) {
		return 0, errors.New("invalid positive integer")
	}
	return value, nil
}

func parseAnalyticsResourceKeys(raw string) ([]analyticsResourceKey, error) {
	parts := strings.Split(raw, ",")
	keys := make([]analyticsResourceKey, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pair := strings.SplitN(part, ":", 2)
		if len(pair) != 2 {
			return nil, errors.New("invalid resource key")
		}
		tenantID, err := strconv.ParseUint(strings.TrimSpace(pair[0]), 10, 64)
		if err != nil || tenantID == 0 || strings.TrimSpace(pair[1]) == "" {
			return nil, errors.New("invalid resource key")
		}
		key := analyticsResourceKey{TenantID: tenantID, ID: strings.TrimSpace(pair[1])}
		keyString := analyticsResourceKeyString(key)
		if !seen[keyString] {
			seen[keyString] = true
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func parseAnalyticsTime(raw string, dateEnd bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return &parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, err
	}
	if dateEnd {
		parsed = parsed.Add(24 * time.Hour)
	}
	return &parsed, nil
}
