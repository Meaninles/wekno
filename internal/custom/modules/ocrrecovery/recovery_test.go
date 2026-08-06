package ocrrecovery

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/vlmguard"
)

func recoveryImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 200, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 200; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 80, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestRecoverCoversOrderedTilesAndRemovesOverlap(t *testing.T) {
	call := 0
	result, err := Recover(context.Background(), recoveryImage(t), "OCR", func(
		_ context.Context, _ [][]byte, prompt string,
	) (string, error) {
		call++
		if !strings.Contains(prompt, "recovery tile") {
			t.Fatalf("missing recovery marker: %s", prompt)
		}
		if call == 1 {
			return "上半页\n重叠行", nil
		}
		return "重叠行\n下半页", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "上半页\n重叠行\n下半页" || result.LeafTiles != 2 || result.Calls != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRecoverFailsClosedWhenLeafStillTruncates(t *testing.T) {
	_, err := Recover(context.Background(), recoveryImage(t), "OCR", func(
		context.Context, [][]byte, string,
	) (string, error) {
		return "partial", &vlmguard.Error{Kind: vlmguard.FailureOutputLimit}
	})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error=%v, want ErrIncomplete", err)
	}
}
