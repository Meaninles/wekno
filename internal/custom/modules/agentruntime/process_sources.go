package agentruntime

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	embedservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type ProcessSnippet struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}
type ProcessSource struct {
	ID            string           `json:"id"`
	Kind          string           `json:"kind"`
	Title         string           `json:"title"`
	URL           string           `json:"url,omitempty"`
	KnowledgeID   string           `json:"knowledge_id,omitempty"`
	KnowledgeBase string           `json:"knowledge_base,omitempty"`
	Snippets      []ProcessSnippet `json:"snippets"`
}

func sourceString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := row[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
func sourceRows(value any) []map[string]any {
	raw, _ := json.Marshal(value)
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	return rows
}

// Only typed result fields become sources; shell output and model prose cannot
// fabricate a document, URL or a claim that a file was read.
func processSources(data map[string]any) []ProcessSource {
	sources := []ProcessSource{}
	indexes := map[string]int{}
	rows := sourceRows(data["results"])
	rows = append(rows, sourceRows(data["knowledge_results"])...)
	rows = append(rows, sourceRows(data["chunk_results"])...)
	rows = append(rows, sourceRows(data["pages"])...)
	for _, chunk := range sourceRows(data["chunks"]) {
		for _, key := range []string{"knowledge_id", "knowledge_title", "knowledge_base_name"} {
			if chunk[key] == nil {
				chunk[key] = data[key]
			}
		}
		rows = append(rows, chunk)
	}
	for _, row := range rows {
		if sourceString(row, "error") != "" {
			continue
		}
		doc := sourceString(row, "knowledge_id")
		rawURL := sourceString(row, "url")
		kind, key := "document", doc
		if doc == "" && sourceString(row, "slug") != "" {
			kind, key = "wiki", sourceString(row, "knowledge_base_id")+":"+sourceString(row, "slug")
		}
		if doc == "" && rawURL != "" {
			u, err := url.Parse(rawURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				continue
			}
			u.Fragment = ""
			u.Host = strings.ToLower(u.Host)
			kind, key = "web", u.String()
		}
		if key == "" {
			continue
		}
		key = kind + ":" + key
		index, found := indexes[key]
		if !found {
			title := sourceString(row, "knowledge_title", "title", "url")
			if title == "" {
				title = "未命名文档"
			}
			index = len(sources)
			indexes[key] = index
			sources = append(sources, ProcessSource{ID: key, Kind: kind, Title: displayText(title, 240), URL: rawURL, KnowledgeID: doc, KnowledgeBase: displayText(sourceString(row, "knowledge_base_name"), 240), Snippets: []ProcessSnippet{}})
		}
		content := sourceString(row, "content", "snippet", "match_snippet", "raw_content", "summary")
		if content == "" {
			continue
		}
		snippet := ProcessSnippet{ID: sourceString(row, "chunk_id", "id"), Content: displayText(content, 3000)}
		duplicate := false
		for _, prior := range sources[index].Snippets {
			if prior.Content == snippet.Content {
				duplicate = true
				break
			}
		}
		if !duplicate {
			sources[index].Snippets = append(sources[index].Snippets, snippet)
		}
	}
	return sources
}

func withProcessSources(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	sources := processSources(data)
	out := make(map[string]any, len(data)+2)
	for k, v := range data {
		out[k] = v
	}
	// Unknown/unrecognized result shapes are not proof of an empty search.
	known := len(sources) > 0
	rowCount := 0
	for _, key := range []string{"results", "knowledge_results", "chunk_results", "chunks", "pages"} {
		value, exists := data[key]
		if !exists {
			continue
		}
		rowCount += len(sourceRows(value))
		raw, _ := json.Marshal(value)
		if string(raw) == "[]" {
			known = true
		}
	}
	if known && (len(sources) > 0 || rowCount == 0) {
		out["process_source_count"] = len(sources)
	}
	preview := sources
	if len(preview) > 2 {
		preview = preview[:2]
	}
	// SSE carries identities only. Snippets are fetched when expanded.
	previews := make([]ProcessSource, len(preview))
	for i, s := range preview {
		s.Snippets = nil
		previews[i] = s
	}
	out["process_sources"] = sourceRows(previews)
	return out
}

func (h *Handler) ProcessSources(c *gin.Context)      { h.serveProcessSources(c, false) }
func (h *Handler) EmbedProcessSources(c *gin.Context) { h.serveProcessSources(c, true) }
func (h *Handler) serveProcessSources(c *gin.Context, embedded bool) {
	ctx := c.Request.Context()
	tenant, ok := types.TenantIDFromContext(ctx)
	if !ok || tenant == 0 {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	query := h.service.db.WithContext(ctx).Where("message_id = ? AND tenant_id = ?", c.Param("message_id"), tenant)
	if embedded {
		ch, exists := middleware.EmbedChannelFromContext(ctx)
		session := c.Param("session_id")
		if !exists || !embedservice.VerifyEmbedSessionHandle(ch, session, c.GetHeader("X-Embed-Session")) {
			c.JSON(403, gin.H{"error": "session signature invalid"})
			return
		}
		query = query.Where("session_id = ?", session)
	} else {
		query = query.Where("user_id = ?", types.SessionOwnerIDFromContext(ctx))
	}
	var run RunRecord
	if query.First(&run).Error != nil {
		c.JSON(404, gin.H{"error": "process not found"})
		return
	}
	var messageCount int64
	if h.service.db.WithContext(ctx).Model(&types.Message{}).Where("id = ? AND session_id = ?", run.MessageID, run.SessionID).Count(&messageCount).Error != nil || messageCount != 1 {
		c.JSON(404, gin.H{"error": "process not found"})
		return
	}
	var receipt ToolReceipt
	if h.service.db.WithContext(ctx).Where("run_id = ? AND call_id = ?", run.ID, c.Param("call_id")).First(&receipt).Error != nil {
		c.JSON(404, gin.H{"error": "process not found"})
		return
	}
	var result ToolCallResponse
	if len(receipt.Response) > 0 && json.Unmarshal(receipt.Response, &result) != nil {
		c.JSON(500, gin.H{"error": "result unavailable"})
		return
	}
	sources := processSources(result.Data)
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > len(sources) {
		offset = len(sources)
	}
	end := offset + 10
	if end > len(sources) {
		end = len(sources)
	}
	c.JSON(200, gin.H{"sources": sources[offset:end], "total": len(sources), "next_offset": end})
}
