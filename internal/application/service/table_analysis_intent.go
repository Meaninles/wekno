package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const tableAnalysisDisplayIntentTool = "table_analysis_display_intent"

func normalizeTableAnalysisDisplayIntent(raw *types.TableAnalysisDisplayIntent) *types.TableAnalysisDisplayIntent {
	if raw == nil {
		raw = &types.TableAnalysisDisplayIntent{}
	}
	confidence := strings.ToLower(strings.TrimSpace(raw.Confidence))
	switch confidence {
	case "high", "medium", "low", "unknown", "error":
	default:
		confidence = "unknown"
	}
	preferred := strings.ToLower(strings.TrimSpace(raw.PreferredChart))
	preferred = strings.ReplaceAll(preferred, "-", "_")
	preferred = strings.ReplaceAll(preferred, " ", "_")
	reason := strings.TrimSpace(raw.Reason)
	if len([]rune(reason)) > 600 {
		reason = string([]rune(reason)[:600]) + "...[truncated]"
	}
	source := strings.TrimSpace(raw.Source)
	if source == "" {
		source = "llm_intent_classifier"
	}
	return &types.TableAnalysisDisplayIntent{
		ChartRequested: raw.ChartRequested,
		Confidence:     confidence,
		PreferredChart: preferred,
		Reason:         reason,
		Source:         source,
	}
}

func tableAnalysisDisplayIntentMessage(intent *types.TableAnalysisDisplayIntent) string {
	if intent != nil && intent.ChartRequested {
		return "用户需要图表展示"
	}
	return "用户不需要图表展示"
}

func emitTableAnalysisDisplayIntentProgress(ctx context.Context, bus *event.EventBus, sessionID string, intent *types.TableAnalysisDisplayIntent, phase string) {
	if bus == nil {
		return
	}
	content := "正在识别是否需要图表展示"
	done := false
	var metadata map[string]interface{}
	if phase == "success" || phase == "error" {
		content = tableAnalysisDisplayIntentMessage(intent)
		done = true
		if intent != nil {
			metadata = map[string]interface{}{"display_intent": intent}
		}
	}
	bus.Emit(ctx, event.Event{
		Type:      event.EventAgentProgress,
		SessionID: sessionID,
		Data: event.AgentProgressData{
			Content:    content,
			ToolName:   tableAnalysisDisplayIntentTool,
			ToolCallID: "table-analysis-display-intent",
			Phase:      phase,
			Transient:  false,
			Done:       done,
			Metadata:   metadata,
		},
	})
}

func compactTableAnalysisIntentHistory(messages []chat.Message, maxMessages int, maxChars int) []map[string]string {
	if len(messages) == 0 {
		return nil
	}
	start := 0
	if len(messages) > maxMessages {
		start = len(messages) - maxMessages
	}
	out := make([]map[string]string, 0, len(messages)-start)
	for _, msg := range messages[start:] {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		runes := []rune(content)
		if len(runes) > maxChars {
			content = string(runes[:maxChars]) + "...[truncated]"
		}
		out = append(out, map[string]string{"role": msg.Role, "content": content})
	}
	return out
}

func tableAnalysisVisibleContext(req *types.QARequest, cfg *types.AgentConfig) map[string]interface{} {
	context := map[string]interface{}{}
	if cfg != nil {
		context["knowledge_base_ids"] = cfg.KnowledgeBases
		context["knowledge_ids"] = cfg.KnowledgeIDs
	}
	if req != nil {
		attachments := make([]map[string]interface{}, 0, len(req.Attachments))
		for i, att := range req.Attachments {
			item := map[string]interface{}{
				"index":     i + 1,
				"file_name": att.FileName,
				"file_type": att.FileType,
				"file_size": att.FileSize,
			}
			if types.IsTabularFileType(att.FileType) {
				item["table_analysis_id"] = types.RuntimeAttachmentID(i)
			}
			attachments = append(attachments, item)
		}
		context["attachments"] = attachments
	}
	return context
}

func classifyTableAnalysisDisplayIntent(
	ctx context.Context,
	model chat.Chat,
	req *types.QARequest,
	cfg *types.AgentConfig,
	history []chat.Message,
) (*types.TableAnalysisDisplayIntent, error) {
	if model == nil {
		return normalizeTableAnalysisDisplayIntent(&types.TableAnalysisDisplayIntent{
			ChartRequested: false,
			Confidence:     "error",
			Reason:         "图表展示意图识别缺少可用模型，按不需要图表展示处理。",
		}), nil
	}
	contextPayload := map[string]interface{}{
		"current_user_query":          "",
		"recent_conversation_history": compactTableAnalysisIntentHistory(history, 8, 1600),
		"visible_context":             tableAnalysisVisibleContext(req, cfg),
	}
	if req != nil {
		contextPayload["current_user_query"] = req.Query
	}
	payloadJSON, _ := json.Marshal(contextPayload)
	system := "你是 WeKnora 表格分析运行时的展示意图识别器。你只判断当前用户这一轮是否需要生成可渲染的数据图表。不要做数据分析，不要生成 SQL，不要输出 Markdown，只返回 JSON。"
	prompt := "请基于 current_user_query，并只在需要解析指代时参考 recent_conversation_history，语义判断用户本轮是否需要图表展示。不要做关键词匹配式判断；要理解否定、追问、上下文指代和“没看到图”等反馈。\n\n" +
		"返回且仅返回 JSON，格式固定为：{\"chart_requested\": boolean, \"confidence\": \"high|medium|low\", \"preferred_chart\": string|null, \"reason\": string}。\n\n" +
		"字段含义：chart_requested=true 表示本轮需要最终展示可渲染的数据图表；chart_requested=false 表示本轮不需要图表展示，或只是文字解释/表格/代码/图片/图标/地图等非数据图表请求。\n\n" +
		"Context:\n" + string(payloadJSON)
	thinking := false
	resp, err := model.Chat(ctx, []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}, &chat.ChatOptions{
		Temperature:         0,
		MaxCompletionTokens: 512,
		Thinking:            &thinking,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return normalizeTableAnalysisDisplayIntent(&types.TableAnalysisDisplayIntent{
			ChartRequested: false,
			Confidence:     "error",
			Reason:         "图表展示意图识别未返回模型响应，按不需要图表展示处理。",
		}), nil
	}
	parsed := parseTableAnalysisIntentJSON(resp.Content)
	if parsed == nil {
		return normalizeTableAnalysisDisplayIntent(&types.TableAnalysisDisplayIntent{
			ChartRequested: false,
			Confidence:     "error",
			Reason:         "图表展示意图识别未返回有效 JSON，按不需要图表展示处理。",
		}), nil
	}
	parsed.Source = "llm_intent_classifier"
	return normalizeTableAnalysisDisplayIntent(parsed), nil
}

func parseTableAnalysisIntentJSON(raw string) *types.TableAnalysisDisplayIntent {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end >= start {
			text = text[start : end+1]
		}
	}
	var intent types.TableAnalysisDisplayIntent
	if err := json.Unmarshal([]byte(text), &intent); err != nil {
		return nil
	}
	return &intent
}

func tableAnalysisDisplayIntentPromptBlock(intent *types.TableAnalysisDisplayIntent) string {
	if intent == nil {
		return ""
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"chart_requested": intent.ChartRequested,
		"status_text":     tableAnalysisDisplayIntentMessage(intent),
		"confidence":      intent.Confidence,
		"preferred_chart": intent.PreferredChart,
		"reason":          intent.Reason,
		"source":          intent.Source,
		"runtime_rules": []string{
			"If chart_requested is true, exploratory evidence-inspection table_analysis calls may keep chart_requested=false, but at least one final analytical result query must call table_analysis with chart_requested=true.",
			"If chart_requested is false, do not set table_analysis.chart_requested=true.",
			"When table_analysis returns a chart or visible table result, the tool input must include LLM-authored source_mapping JSON; this is a weak-template evidence mapping, not a fixed schema.",
			"If table_analysis rejects a call because it conflicts with this decision, correct chart_requested/preferred_chart and retry.",
		},
	})
	return fmt.Sprintf("<table_analysis_display_intent source=\"runtime_preflight\" role=\"binding_runtime_decision\">\n%s\n</table_analysis_display_intent>", payload)
}
