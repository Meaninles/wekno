package agentruntime

import (
	"encoding/json"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const ToolTranscribeInputFile = "transcribe_input_file"

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

func runtimeToolSpecsWithInputs(registry interfaces.AgentToolRegistry, inputs []OriginalInputFileSpec, config *types.AgentConfig) []RuntimeToolSpec {
	out := runtimeToolSpecs(registry)
	if config == nil || strings.TrimSpace(config.ASRModelID) == "" || !hasAudioOriginalInput(inputs) {
		return out
	}
	for _, item := range out {
		if item.Name == ToolTranscribeInputFile {
			return out
		}
	}
	return append(out, audioTranscriptionToolSpec())
}

func hasAudioOriginalInput(inputs []OriginalInputFileSpec) bool {
	for _, item := range inputs {
		switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(item.FileType)), ".") {
		case "mp3", "wav", "m4a", "flac", "ogg":
			return true
		}
	}
	return false
}

func audioTranscriptionToolSpec() RuntimeToolSpec {
	return RuntimeToolSpec{
		Name:              ToolTranscribeInputFile,
		IsReadOnly:        true,
		IsConcurrencySafe: false,
		TimeoutSeconds:    900,
		Description:       "Transcribe one uploaded audio input file with the Agent's configured ASR model. Use the exact input_file_id from the workspace input manifest. The primary chat model remains in control; this tool returns transcript and timestamped segments as evidence.",
		Parameters:        json.RawMessage(`{"type":"object","additionalProperties":false,"required":["input_file_id"],"properties":{"input_file_id":{"type":"string","description":"Exact input_file_id from original_input_manifest.json"}}}`),
		Source:            "native",
	}
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
