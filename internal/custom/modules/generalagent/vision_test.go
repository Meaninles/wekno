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

func TestImageInspectionRetainsAndValidatesSourceRegion(t *testing.T) {
	models := &inspectionModels{vision: &inspectionVLM{}}
	registry := &visionRegistry{models: models, modelID: "configured-vision"}
	view := imageView{SourceWidth: 6688, SourceHeight: 6688, ViewWidth: 1, ViewHeight: 1,
		Region: []int{6000, 6001, 6001, 6002}, FrameCount: 1}
	args := map[string]any{
		"image_base64": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6V8AAAAASUVORK5CYII=",
		"prompt":       "Read the visible text", "image_view": &view,
	}
	raw, _ := json.Marshal(args)
	result, err := registry.ExecuteTool(context.Background(), "inspect_image", raw)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		View        imageView `json:"image_view"`
		Observation string    `json:"observation"`
	}
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatal(err)
	}
	if output.View.Region[0] != 6000 || output.View.Resized || output.Observation != "Visible labels and layout" {
		t.Fatalf("lost the actual source region: %+v", output)
	}
	if _, duplicate := result.Data["observation"]; duplicate {
		t.Fatal("duplicated image observation in history")
	}
	for _, change := range []func(*imageView){
		func(v *imageView) { v.ViewWidth = 2 },
		func(v *imageView) { v.SourceWidth = 5000 },
		func(v *imageView) { v.Resized = true },
		func(v *imageView) { v.Frame = 1 },
	} {
		bad := view
		change(&bad)
		args["image_view"] = bad
		raw, _ := json.Marshal(args)
		if _, err := registry.ExecuteTool(context.Background(), "inspect_image", raw); err == nil {
			t.Fatalf("accepted inconsistent image view: %+v", bad)
		}
	}
}
