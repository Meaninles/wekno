package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// The sidecar transports local image bytes; credentials and provider-specific
// vision APIs stay in the platform's existing model service.
type visionRegistry struct {
	interfaces.AgentToolRegistry
	models  interfaces.ModelService
	modelID string
}

var visionParameters = json.RawMessage(`{"type":"object","properties":{"image_base64":{"type":"string","minLength":1,"maxLength":27962028},"prompt":{"type":"string","minLength":1},"file_name":{"type":"string"}},"required":["image_base64","prompt"],"additionalProperties":false}`)

func (r *visionRegistry) GetFunctionDefinitions() []types.FunctionDefinition {
	return append(r.AgentToolRegistry.GetFunctionDefinitions(), types.FunctionDefinition{Name: "inspect_image", Description: "Inspect a local image with the configured vision model. Ask about visible content, layout, readability or specific visual details; the result is a textual observation, not an automatic artifact approval.", Parameters: visionParameters})
}

func (r *visionRegistry) ExecuteTool(ctx context.Context, name string, raw json.RawMessage) (*types.ToolResult, error) {
	if name != "inspect_image" {
		return r.AgentToolRegistry.ExecuteTool(ctx, name, raw)
	}
	if err := toolcontract.Validate(raw, visionParameters); err != nil {
		return nil, err
	}
	var args struct {
		Image    string `json:"image_base64"`
		Prompt   string `json:"prompt"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(args.Image)
	if err != nil {
		return nil, fmt.Errorf("invalid image encoding: %w", err)
	}
	if len(data) > 20*1024*1024 || !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return nil, fmt.Errorf("expected an image no larger than 20 MiB")
	}
	model, err := r.models.GetVLMModel(ctx, r.modelID)
	if err != nil {
		return nil, err
	}
	observation, err := model.Predict(ctx, [][]byte{data}, args.Prompt)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(observation) == "" {
		return nil, fmt.Errorf("vision model returned no observation")
	}
	return &types.ToolResult{Success: true, Output: observation, Data: map[string]any{"file_name": args.FileName, "sha256": fmt.Sprintf("%x", sha256.Sum256(data)), "model_id": r.modelID, "observation": observation}}, nil
}

// Keep binary payloads out of message histories and UI events while retaining
// a stable identity for the exact bytes supplied to the vision model.
func imageTransportAuditArgs(args map[string]any) map[string]any {
	encoded, ok := args["image_base64"].(string)
	if !ok {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if k != "image_base64" {
			out[k] = v
		}
	}
	if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
		out["image_sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		out["image_bytes"] = len(data)
	}
	return out
}
