package generalagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
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

var visionParameters = json.RawMessage(`{"type":"object","properties":{"image_base64":{"type":"string","minLength":1,"maxLength":27962028},"prompt":{"type":"string","minLength":1},"file_name":{"type":"string"},"image_view":{"type":"object","properties":{"source_width":{"type":"integer","minimum":1},"source_height":{"type":"integer","minimum":1},"view_width":{"type":"integer","minimum":1},"view_height":{"type":"integer","minimum":1},"region":{"type":"array","items":{"type":"integer","minimum":0},"minItems":4,"maxItems":4},"resized":{"type":"boolean"},"frame":{"type":"integer","minimum":0},"frame_count":{"type":"integer","minimum":1}},"required":["source_width","source_height","view_width","view_height","region","resized","frame","frame_count"],"additionalProperties":false}},"required":["image_base64","prompt"],"additionalProperties":false}`)

type imageView struct {
	SourceWidth  int   `json:"source_width"`
	SourceHeight int   `json:"source_height"`
	ViewWidth    int   `json:"view_width"`
	ViewHeight   int   `json:"view_height"`
	Region       []int `json:"region"`
	Resized      bool  `json:"resized"`
	Frame        int   `json:"frame"`
	FrameCount   int   `json:"frame_count"`
}

func (r *visionRegistry) GetFunctionDefinitions() []types.FunctionDefinition {
	return append(r.AgentToolRegistry.GetFunctionDefinitions(), types.FunctionDefinition{Name: "inspect_image", Description: "Inspect a local image with the configured vision model. Returns visual observations and the viewed region/dimensions. Large originals use a bounded overview; select a region in original pixel coordinates to inspect fine details. Observations are not automatic artifact approval.", Parameters: visionParameters})
}

func (r *visionRegistry) ExecuteTool(ctx context.Context, name string, raw json.RawMessage) (*types.ToolResult, error) {
	if name != "inspect_image" {
		return r.AgentToolRegistry.ExecuteTool(ctx, name, raw)
	}
	if err := toolcontract.Validate(raw, visionParameters); err != nil {
		return nil, err
	}
	var args struct {
		Image    string     `json:"image_base64"`
		Prompt   string     `json:"prompt"`
		FileName string     `json:"file_name"`
		View     *imageView `json:"image_view"`
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
	if view := args.View; view != nil {
		cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(data))
		if decodeErr != nil || cfg.Width != view.ViewWidth || cfg.Height != view.ViewHeight {
			return nil, fmt.Errorf("image view dimensions do not match the transported image")
		}
		box := view.Region
		if box[0] >= box[2] || box[1] >= box[3] || box[2] > view.SourceWidth || box[3] > view.SourceHeight || view.Frame >= view.FrameCount {
			return nil, fmt.Errorf("image view is outside the original source")
		}
		if view.Resized != (cfg.Width != box[2]-box[0] || cfg.Height != box[3]-box[1]) {
			return nil, fmt.Errorf("image view resize metadata does not match the viewed region")
		}
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
	output := observation
	metadata := map[string]any{"file_name": args.FileName, "sha256": fmt.Sprintf("%x", sha256.Sum256(data)), "model_id": r.modelID}
	if args.View != nil {
		metadata["image_view"] = args.View
		encoded, _ := json.Marshal(map[string]any{"image_view": args.View, "observation": observation})
		output = string(encoded)
	}
	return &types.ToolResult{Success: true, Output: output, Data: metadata}, nil
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
