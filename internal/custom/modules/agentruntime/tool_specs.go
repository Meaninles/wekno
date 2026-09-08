package agentruntime

import (
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"strings"
)

func runtimeToolSpecs(registry interfaces.AgentToolRegistry) []RuntimeToolSpec {
	if registry == nil {
		return nil
	}
	defs := registry.GetFunctionDefinitions()
	out := make([]RuntimeToolSpec, 0, len(defs))
	for _, def := range defs {
		readOnly := toolReadOnly(def.Name)
		if catalog, ok := registry.(interface {
			GetTool(string) (types.Tool, error)
		}); ok {
			if tool, err := catalog.GetTool(def.Name); err == nil {
				if metadata, ok := tool.(interface{ ConcurrentReadOnly() bool }); ok {
					readOnly = metadata.ConcurrentReadOnly()
				}
			}
		}
		out = append(out, RuntimeToolSpec{
			Name:              def.Name,
			IsReadOnly:        readOnly,
			IsConcurrencySafe: readOnly,
			TimeoutSeconds:    900,
			Description:       def.Description,
			Parameters:        def.Parameters,
			Source:            classifyToolSource(def.Name),
		})
	}
	return out
}

func classifyToolSource(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "mcp_"):
		return "mcp"
	case strings.HasPrefix(name, "db_"):
		return "database"
	case strings.HasPrefix(name, "wiki_"):
		return "wiki"
	case strings.Contains(name, "skill"):
		return "skill"
	case name == agenttools.ToolKnowledgeSearch ||
		name == agenttools.ToolGrepChunks ||
		name == agenttools.ToolListKnowledgeChunks ||
		name == agenttools.ToolGetDocumentInfo ||
		name == agenttools.ToolQueryKnowledgeGraph ||
		name == agenttools.ToolDatabaseQuery:
		return "knowledge"
	case name == agenttools.ToolWebSearch || name == agenttools.ToolWebFetch:
		return "web"
	default:
		return "native"
	}
}
