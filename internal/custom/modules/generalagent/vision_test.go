package generalagent

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"testing"
)

type inspectionModels struct {
	interfaces.ModelService
	got    string
	vision *inspectionVLM
}

func (m *inspectionModels) GetVLMModel(ctx context.Context, id string) (vlm.VLM, error) {
	m.got = id
	return m.vision, ctx.Err()
}

type inspectionVLM struct {
	prompt string
	bytes  int
}

func (m *inspectionVLM) Predict(ctx context.Context, images [][]byte, prompt string) (string, error) {
	m.prompt = prompt
	m.bytes = len(images[0])
	return "Visible labels and layout", ctx.Err()
}
func (*inspectionVLM) GetModelName() string { return "vision" }
func (*inspectionVLM) GetModelID() string   { return "configured-vision" }

func TestImageInspectionUsesConfiguredVisionAndRedactsTransport(t *testing.T) {
	encoded := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6V8AAAAASUVORK5CYII="
	args := map[string]any{"image_base64": encoded, "prompt": "Inspect typography", "file_name": "page.png"}
	raw, _ := json.Marshal(args)
	models := &inspectionModels{vision: &inspectionVLM{}}
	r := &visionRegistry{models: models, modelID: "configured-vision"}
	result, err := r.ExecuteTool(context.Background(), "inspect_image", raw)
	if err != nil || !result.Success || models.got != "configured-vision" || models.vision.prompt != "Inspect typography" || models.vision.bytes == 0 {
		t.Fatalf("inspection: %+v %v", result, err)
	}
	audit := imageTransportAuditArgs(args)
	if _, ok := audit["image_base64"]; ok || audit["image_sha256"] == nil {
		t.Fatal("binary image leaked to history")
	}
	if args["image_base64"] != encoded {
		t.Fatal("mutated execution input")
	}
	for _, bad := range []string{`{"image_base64":"!","prompt":"x"}`, `{"image_base64":"aGVsbG8=","prompt":"x"}`, `{"prompt":"x"}`} {
		if _, err := r.ExecuteTool(context.Background(), "inspect_image", json.RawMessage(bad)); err == nil {
			t.Fatal("accepted invalid image")
		}
	}
}
