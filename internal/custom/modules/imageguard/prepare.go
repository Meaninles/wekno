package imageguard

import (
	"bytes"
	"errors"
	"fmt"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
)

const (
	// Providers commonly reject an absolute aspect ratio >= 200. Keep margin
	// for model-side rounding and patch-size normalization.
	maxPreparedAspectRatio       = 180
	minMeaningfulAxis            = 8
	maxDecodedPixels       int64 = 50_000_000
	maxDecodedAxis               = 32_768
)

var ErrUnsafeDimensions = errors.New("image guard: unsafe decoded dimensions")

// Prepared is the deterministic image handed to a VLM.
type Prepared struct {
	Bytes          []byte
	Format         string
	Width          int
	Height         int
	PreparedWidth  int
	PreparedHeight int
	Skip           bool
	SkipReason     string
	Normalized     bool
}

// PrepareForVLM filters decorative slivers and pads meaningful extreme-aspect
// images onto a white canvas. Padding preserves every source pixel while
// satisfying provider geometry limits; it never stretches or crops content.
func PrepareForVLM(source []byte) (Prepared, error) {
	if len(source) == 0 {
		return Prepared{}, errors.New("image guard: empty image")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(source))
	if err != nil {
		return Prepared{}, fmt.Errorf("image guard: decode image dimensions: %w", err)
	}
	if err := validateDimensions(cfg.Width, cfg.Height); err != nil {
		return Prepared{}, err
	}
	result := Prepared{
		Bytes: source, Format: format,
		Width: cfg.Width, Height: cfg.Height,
		PreparedWidth: cfg.Width, PreparedHeight: cfg.Height,
	}
	if cfg.Width < minMeaningfulAxis || cfg.Height < minMeaningfulAxis {
		result.Skip = true
		result.SkipReason = "decorative_thin_image"
		return result, nil
	}

	major, minor := cfg.Width, cfg.Height
	horizontal := true
	if cfg.Height > cfg.Width {
		major, minor = cfg.Height, cfg.Width
		horizontal = false
	}
	if major <= maxPreparedAspectRatio*minor {
		return result, nil
	}

	targetMinor := (major + maxPreparedAspectRatio - 1) / maxPreparedAspectRatio
	targetWidth, targetHeight := cfg.Width, cfg.Height
	if horizontal {
		targetHeight = targetMinor
	} else {
		targetWidth = targetMinor
	}
	if err := validateDimensions(targetWidth, targetHeight); err != nil {
		return Prepared{}, err
	}

	decoded, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return Prepared{}, fmt.Errorf("image guard: decode image for aspect padding: %w", err)
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.NRGBA{R: 255, G: 255, B: 255, A: 255}}, image.Point{}, draw.Src)
	offset := image.Pt(
		(targetWidth-cfg.Width)/2,
		(targetHeight-cfg.Height)/2,
	)
	draw.Draw(canvas, image.Rectangle{Min: offset, Max: offset.Add(decoded.Bounds().Size())}, decoded, decoded.Bounds().Min, draw.Over)

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return Prepared{}, fmt.Errorf("image guard: encode aspect-padded image: %w", err)
	}
	result.Bytes = encoded.Bytes()
	result.PreparedWidth = targetWidth
	result.PreparedHeight = targetHeight
	result.Normalized = true
	return result, nil
}

func validateDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%w: %dx%d", ErrUnsafeDimensions, width, height)
	}
	w, h := int64(width), int64(height)
	if width > maxDecodedAxis || height > maxDecodedAxis || w > maxDecodedPixels/h {
		return fmt.Errorf("%w: %dx%d", ErrUnsafeDimensions, width, height)
	}
	return nil
}
