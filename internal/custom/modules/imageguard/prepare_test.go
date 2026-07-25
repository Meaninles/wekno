package imageguard

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x % 255), G: uint8(y % 255), B: 80, A: 255})
		}
	}
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, img))
	return out.Bytes()
}

func TestPrepareForVLMSkipsDecorativeThinImage(t *testing.T) {
	source := encodedPNG(t, 922, 2)
	prepared, err := PrepareForVLM(source)
	require.NoError(t, err)
	require.True(t, prepared.Skip)
	require.Equal(t, "decorative_thin_image", prepared.SkipReason)
	require.Equal(t, 922, prepared.Width)
	require.Equal(t, 2, prepared.Height)
	require.False(t, prepared.Normalized)
}

func TestPrepareForVLMPadsMeaningfulExtremeAspectWithoutStretching(t *testing.T) {
	source := encodedPNG(t, 2000, 8)
	prepared, err := PrepareForVLM(source)
	require.NoError(t, err)
	require.False(t, prepared.Skip)
	require.True(t, prepared.Normalized)
	require.Equal(t, 2000, prepared.PreparedWidth)
	require.Equal(t, 12, prepared.PreparedHeight)
	require.LessOrEqual(t,
		float64(prepared.PreparedWidth)/float64(prepared.PreparedHeight),
		float64(maxPreparedAspectRatio),
	)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(prepared.Bytes))
	require.NoError(t, err)
	require.Equal(t, "png", format)
	require.Equal(t, prepared.PreparedWidth, cfg.Width)
	require.Equal(t, prepared.PreparedHeight, cfg.Height)
}

func TestPrepareForVLMLeavesNormalImageByteIdentical(t *testing.T) {
	source := encodedPNG(t, 320, 180)
	prepared, err := PrepareForVLM(source)
	require.NoError(t, err)
	require.False(t, prepared.Skip)
	require.False(t, prepared.Normalized)
	require.Equal(t, source, prepared.Bytes)
}

func TestPrepareForVLMRejectsMalformedImage(t *testing.T) {
	_, err := PrepareForVLM([]byte("not-an-image"))
	require.Error(t, err)
}
