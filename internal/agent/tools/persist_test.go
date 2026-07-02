package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestShouldOmitRawToolOutput(t *testing.T) {
	if !ShouldOmitRawToolOutput(ToolListKnowledgeChunks, map[string]interface{}{"display_type": "knowledge_chunks_list"}) {
		t.Fatal("structured list_knowledge_chunks output should be omitted")
	}
	if !ShouldOmitRawToolOutput(ToolGrepChunks, map[string]interface{}{"display_type": "grep_results"}) {
		t.Fatal("structured grep output should be omitted")
	}
	if ShouldOmitRawToolOutput("custom_tool", nil) {
		t.Fatal("unknown tools should keep raw output by default")
	}
}

func TestSanitizeToolDataForPersist_knowledgeChunksList(t *testing.T) {
	data := map[string]interface{}{
		"display_type":    "knowledge_chunks_list",
		"knowledge_title": "sample.pdf",
		"fetched_chunks":  50,
		"total_chunks":    282,
		"chunks":          []map[string]interface{}{{"content": "secret"}},
	}
	out := SanitizeToolDataForPersist(data)
	if _, ok := out["chunks"]; ok {
		t.Fatal("chunk bodies should be stripped from persisted tool data")
	}
	if out["fetched_chunks"] != 50 {
		t.Fatalf("summary fields should be kept, got %#v", out["fetched_chunks"])
	}
}

func TestSanitizeAgentStepsForStorage_stripsLargeOutput(t *testing.T) {
	steps := []types.AgentStep{{
		Iteration: 1,
		ToolCalls: []types.ToolCall{{
			ID:   "call-1",
			Name: ToolListKnowledgeChunks,
			Result: &types.ToolResult{
				Success: true,
				Output:  strings.Repeat("x", 10000),
				Data: map[string]interface{}{
					"display_type":    "knowledge_chunks_list",
					"knowledge_title": "sample.pdf",
					"fetched_chunks":  50,
					"total_chunks":    282,
					"chunks":          []map[string]interface{}{{"content": "body"}},
				},
			},
		}},
	}}

	sanitized := SanitizeAgentStepsForStorage(steps)
	result := sanitized[0].ToolCalls[0].Result
	if len(result.Output) >= 10000 {
		t.Fatal("persisted output should be compacted")
	}
	if !strings.Contains(result.Output, "content omitted from history") {
		t.Fatalf("unexpected compact output: %q", result.Output)
	}
	if _, ok := result.Data["chunks"]; ok {
		t.Fatal("chunk bodies should be removed from persisted data")
	}
}

func TestSanitizeToolResultForClient_omitsOutput(t *testing.T) {
	meta := SanitizeToolResultForClient(ToolListKnowledgeChunks, &types.ToolResult{
		Success: true,
		Output:  "<knowledge_chunks>very large</knowledge_chunks>",
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_title": "sample.pdf",
			"fetched_chunks":  1,
			"total_chunks":    1,
		},
	})
	if _, ok := meta["output"]; ok {
		t.Fatal("raw output should not be sent to client metadata")
	}
	if meta["fetched_chunks"] != 1 {
		t.Fatalf("summary metadata should remain, got %#v", meta["fetched_chunks"])
	}
}

func TestSanitizeToolResultForClient_minimizesWebSearchPayload(t *testing.T) {
	meta := SanitizeToolResultForClient(ToolWebSearch, &types.ToolResult{
		Success: true,
		Output:  strings.Repeat("raw", 1000),
		Data: map[string]interface{}{
			"display_type": "web_search_results",
			"query":        "search query",
			"count":        1,
			"results": []map[string]interface{}{{
				"result_index": 1,
				"title":        "title",
				"url":          "https://example.com",
				"snippet":      "snippet should not be sent",
				"content":      "full content should not be sent",
				"source":       "example",
			}},
		},
	})

	if _, ok := meta["output"]; ok {
		t.Fatal("raw output should not be sent to client metadata")
	}
	results, ok := meta["results"].([]map[string]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("results = %#v, want one sanitized result", meta["results"])
	}
	if _, ok := results[0]["content"]; ok {
		t.Fatalf("web search content leaked to client metadata: %#v", results[0])
	}
	if _, ok := results[0]["snippet"]; ok {
		t.Fatalf("web search snippet leaked to client metadata: %#v", results[0])
	}
	if results[0]["title"] != "title" || results[0]["url"] != "https://example.com" {
		t.Fatalf("display fields missing from sanitized result: %#v", results[0])
	}
}

func TestSanitizeToolResultForClient_stripsWebFetchRawContent(t *testing.T) {
	meta := SanitizeToolResultForClient(ToolWebFetch, &types.ToolResult{
		Success: true,
		Data: map[string]interface{}{
			"display_type": "web_fetch_results",
			"count":        1,
			"results": []map[string]interface{}{{
				"url":            "https://example.com",
				"prompt":         "summarize",
				"summary":        "short summary",
				"raw_content":    "raw page body should not be sent",
				"content_length": 12345,
			}},
		},
	})

	results, ok := meta["results"].([]map[string]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("results = %#v, want one sanitized result", meta["results"])
	}
	if _, ok := results[0]["raw_content"]; ok {
		t.Fatalf("web fetch raw_content leaked to client metadata: %#v", results[0])
	}
	if results[0]["summary"] != "short summary" {
		t.Fatalf("summary should remain, got %#v", results[0]["summary"])
	}
}

func TestSanitizeAgentStepsForClient_stripsArgsProviderMetadataAndRawOutput(t *testing.T) {
	steps := []types.AgentStep{{
		Iteration: 1,
		ToolCalls: []types.ToolCall{{
			ID:   "call-1",
			Name: "custom_tool",
			Args: map[string]interface{}{"secret": "do not send"},
			ProviderMetadata: types.ToolCallMetadata{
				"provider": json.RawMessage(`{"secret":"metadata"}`),
			},
			Result: &types.ToolResult{
				Success: true,
				Output:  "raw tool output should not be sent",
				Data: map[string]interface{}{
					"display_type": "web_fetch_results",
					"results": []map[string]interface{}{{
						"url":         "https://example.com",
						"raw_content": "raw page body",
					}},
				},
			},
		}},
	}}

	sanitized := SanitizeAgentStepsForClient(steps)
	call := sanitized[0].ToolCalls[0]
	if call.Args != nil {
		t.Fatalf("tool args leaked to client: %#v", call.Args)
	}
	if call.ProviderMetadata != nil {
		t.Fatalf("provider metadata leaked to client: %#v", call.ProviderMetadata)
	}
	if strings.Contains(call.Result.Output, "raw tool output") {
		t.Fatalf("raw output leaked to client: %q", call.Result.Output)
	}
	results, ok := call.Result.Data["results"].([]map[string]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("sanitized data results = %#v", call.Result.Data["results"])
	}
	if _, ok := results[0]["raw_content"]; ok {
		t.Fatalf("raw_content leaked in client agent steps: %#v", results[0])
	}
}
