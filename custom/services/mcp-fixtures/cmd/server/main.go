package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const defaultAddr = ":8092"

type kvStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func main() {
	addr := strings.TrimSpace(os.Getenv("MCP_FIXTURES_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}

	mcpServer := server.NewMCPServer(
		"weknora-general-agent-fixtures",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)
	store := &kvStore{data: map[string]string{}}
	fsRoot := strings.TrimSpace(os.Getenv("MCP_FIXTURE_FS_ROOT"))
	if fsRoot == "" {
		fsRoot = "/tmp/weknora-mcp-fixtures-fs"
	}
	if err := os.MkdirAll(fsRoot, 0o755); err != nil {
		log.Fatalf("create fixture fs root: %v", err)
	}
	registerTools(mcpServer, store, fsRoot)

	mux := http.NewServeMux()
	mux.Handle("/mcp", server.NewStreamableHTTPServer(
		mcpServer,
		server.WithEndpointPath("/mcp"),
		server.WithStateLess(true),
	))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	log.Printf("WeKnora MCP fixtures listening on %s", addr)
	log.Printf("MCP endpoint: http://localhost%s/mcp", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func registerTools(s *server.MCPServer, store *kvStore, fsRoot string) {
	s.AddTool(mcp.NewTool(
		"time_now",
		mcp.WithDescription("Return the current time in a requested IANA timezone. Useful to verify MCP time capability."),
		toolAnnotation(true, false, false, false),
		mcp.WithString("timezone", mcp.Description("Optional IANA timezone, e.g. Asia/Shanghai or America/New_York. Defaults to UTC.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tz := strings.TrimSpace(req.GetString("timezone", "UTC"))
		if tz == "" {
			tz = "UTC"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid timezone %q: %v", tz, err)), nil
		}
		now := time.Now().In(loc)
		return jsonText(map[string]any{
			"timezone": tz,
			"rfc3339":  now.Format(time.RFC3339),
			"unix":     now.Unix(),
		}), nil
	})

	s.AddTool(mcp.NewTool(
		"echo",
		mcp.WithDescription("Echo back a message. Useful for verifying selected MCP tools are exposed."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message to echo.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		msg, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonText(map[string]any{"echo": msg}), nil
	})

	s.AddTool(mcp.NewTool(
		"calculator",
		mcp.WithDescription("Perform a deterministic arithmetic operation."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("operation", mcp.Required(), mcp.Enum("add", "subtract", "multiply", "divide", "pow"), mcp.Description("Operation to run.")),
		mcp.WithNumber("x", mcp.Required(), mcp.Description("First number.")),
		mcp.WithNumber("y", mcp.Required(), mcp.Description("Second number.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		op, err := req.RequireString("operation")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		x, err := req.RequireFloat("x")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		y, err := req.RequireFloat("y")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var out float64
		switch op {
		case "add":
			out = x + y
		case "subtract":
			out = x - y
		case "multiply":
			out = x * y
		case "divide":
			if y == 0 {
				return mcp.NewToolResultError("cannot divide by zero"), nil
			}
			out = x / y
		case "pow":
			out = math.Pow(x, y)
		default:
			return mcp.NewToolResultError("unsupported operation"), nil
		}
		return jsonText(map[string]any{"operation": op, "x": x, "y": y, "result": out}), nil
	})

	s.AddTool(mcp.NewTool(
		"kv_put",
		mcp.WithDescription("Store a short value in an in-memory key-value store. Useful to test stateful MCP calls in one fixture service."),
		toolAnnotation(false, false, true, false),
		mcp.WithString("key", mcp.Required(), mcp.Description("Key, max 80 chars.")),
		mcp.WithString("value", mcp.Required(), mcp.Description("Value, max 2000 chars.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key, err := req.RequireString("key")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		value, err := req.RequireString("value")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		key = strings.TrimSpace(key)
		if key == "" || len(key) > 80 {
			return mcp.NewToolResultError("key must be 1-80 characters"), nil
		}
		if len(value) > 2000 {
			return mcp.NewToolResultError("value exceeds 2000 characters"), nil
		}
		store.mu.Lock()
		store.data[key] = value
		store.mu.Unlock()
		return jsonText(map[string]any{"ok": true, "key": key}), nil
	})

	s.AddTool(mcp.NewTool(
		"kv_get",
		mcp.WithDescription("Read a value from the in-memory key-value store."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("key", mcp.Required(), mcp.Description("Key to read.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key, err := req.RequireString("key")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		key = strings.TrimSpace(key)
		store.mu.RLock()
		value, ok := store.data[key]
		store.mu.RUnlock()
		return jsonText(map[string]any{"key": key, "found": ok, "value": value}), nil
	})

	s.AddTool(mcp.NewTool(
		"fetch_stub",
		mcp.WithDescription("Return a deterministic summary for a URL without performing network access. Useful to test Fetch-like MCP wiring safely."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("url", mcp.Required(), mcp.Description("URL to summarize.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := req.RequireString("url")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		url = strings.TrimSpace(url)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return mcp.NewToolResultError("url must start with http:// or https://"), nil
		}
		return jsonText(map[string]any{
			"url":     url,
			"title":   "Fixture fetch result",
			"summary": "This is a deterministic fetch-like MCP fixture. It does not access the network.",
		}), nil
	})

	s.AddTool(mcp.NewTool(
		"fs_list",
		mcp.WithDescription("List files under the fixture safe directory. Paths are relative and cannot escape the sandbox."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("path", mcp.Description("Relative directory path. Defaults to '.'.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rel := req.GetString("path", ".")
		dir, err := safeFixturePath(fsRoot, rel)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			info, _ := entry.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			out = append(out, map[string]any{
				"name":  entry.Name(),
				"isDir": entry.IsDir(),
				"size":  size,
			})
		}
		return jsonText(map[string]any{"root": fsRoot, "path": rel, "entries": out}), nil
	})

	s.AddTool(mcp.NewTool(
		"fs_write_text",
		mcp.WithDescription("Write a UTF-8 text file under the fixture safe directory. Paths are relative and cannot escape the sandbox."),
		toolAnnotation(false, true, true, false),
		mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("UTF-8 text content, max 4000 chars.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rel, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		content, err := req.RequireString("content")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(content) > 4000 {
			return mcp.NewToolResultError("content exceeds 4000 characters"), nil
		}
		path, err := safeFixturePath(fsRoot, rel)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonText(map[string]any{"ok": true, "path": rel, "bytes": len([]byte(content))}), nil
	})

	s.AddTool(mcp.NewTool(
		"fs_read_text",
		mcp.WithDescription("Read a UTF-8 text file under the fixture safe directory. Paths are relative and cannot escape the sandbox."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rel, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		path, err := safeFixturePath(fsRoot, rel)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(data) > 8000 {
			data = data[:8000]
		}
		return jsonText(map[string]any{"path": rel, "content": string(data), "bytes": len(data)}), nil
	})

	s.AddTool(mcp.NewTool(
		"sqlite_query",
		mcp.WithDescription("Run a tiny read-only SQL-like query against fixture sales rows. Supports SELECT * FROM demo_sales and simple WHERE region = '...' filters."),
		toolAnnotation(true, false, true, false),
		mcp.WithString("query", mcp.Required(), mcp.Description("Read-only query for demo_sales.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		rows, err := fixtureSQL(query)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonText(map[string]any{"rows": rows, "row_count": len(rows)}), nil
	})
}

func toolAnnotation(readOnly, destructive, idempotent, openWorld bool) mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    boolPtr(readOnly),
		DestructiveHint: boolPtr(destructive),
		IdempotentHint:  boolPtr(idempotent),
		OpenWorldHint:   boolPtr(openWorld),
	})
}

func boolPtr(value bool) *bool {
	return &value
}

func safeFixturePath(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes fixture root")
	}
	out := filepath.Join(rootAbs, clean)
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if outAbs != rootAbs && !strings.HasPrefix(outAbs, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes fixture root")
	}
	return outAbs, nil
}

func jsonText(v any) *mcp.CallToolResult {
	data, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(data))
}

func fixtureSQL(query string) ([]map[string]any, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("query is required")
	}
	if strings.Contains(q, ";") || strings.Contains(q, "insert ") || strings.Contains(q, "update ") ||
		strings.Contains(q, "delete ") || strings.Contains(q, "drop ") || strings.Contains(q, "alter ") {
		return nil, fmt.Errorf("only one read-only SELECT query is allowed")
	}
	if !strings.HasPrefix(q, "select ") || !strings.Contains(q, " from demo_sales") {
		return nil, fmt.Errorf("only SELECT queries from demo_sales are supported")
	}
	rows := []map[string]any{
		{"region": "north", "product": "notebook", "revenue": 1200, "orders": 12},
		{"region": "south", "product": "keyboard", "revenue": 840, "orders": 7},
		{"region": "east", "product": "monitor", "revenue": 1800, "orders": 9},
	}
	if strings.Contains(q, "where") && strings.Contains(q, "region") {
		for _, region := range []string{"north", "south", "east"} {
			if strings.Contains(q, "'"+region+"'") || strings.Contains(q, "\""+region+"\"") {
				filtered := make([]map[string]any, 0, len(rows))
				for _, row := range rows {
					if row["region"] == region {
						filtered = append(filtered, row)
					}
				}
				rows = filtered
				break
			}
		}
	}
	if strings.Contains(q, "order by revenue desc") {
		sort.Slice(rows, func(i, j int) bool {
			return rows[i]["revenue"].(int) > rows[j]["revenue"].(int)
		})
	}
	return rows, nil
}
