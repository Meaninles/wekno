// Package ocrrecovery performs bounded, loss-averse recovery when a full-page
// VLM OCR call reaches an output/repetition guard. It covers the same pixels
// with ordered overlapping tiles and succeeds only when every tile completes.
package ocrrecovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/Tencent/WeKnora/internal/custom/modules/ocrstructure"
	"github.com/Tencent/WeKnora/internal/custom/modules/vlmguard"
)

const maxRecoveryDepth = 1

var ErrIncomplete = errors.New("OCR tile recovery incomplete")

type Predictor func(context.Context, [][]byte, string) (string, error)

type Result struct {
	Text             string
	Calls            int
	LeafTiles        int
	EmptyRowsTrimmed int
}

func Recover(
	ctx context.Context,
	imageBytes []byte,
	basePrompt string,
	predict Predictor,
) (Result, error) {
	var result Result
	if predict == nil {
		return result, errors.New("OCR tile recovery predictor is unavailable")
	}
	decoded, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return result, fmt.Errorf("decode OCR recovery image: %w", err)
	}
	parts := split(decoded)
	texts := make([]string, 0, len(parts))
	for index, part := range parts {
		text, stats, recoverErr := recoverTile(
			ctx, part, basePrompt, fmt.Sprintf("%d/%d", index+1, len(parts)), 0, predict,
		)
		result.Calls += stats.Calls
		result.LeafTiles += stats.LeafTiles
		result.EmptyRowsTrimmed += stats.EmptyRowsTrimmed
		if recoverErr != nil {
			return result, recoverErr
		}
		texts = append(texts, text)
	}
	result.Text = mergeOrdered(texts)
	return result, nil
}

func recoverTile(
	ctx context.Context,
	img image.Image,
	basePrompt, path string,
	depth int,
	predict Predictor,
) (string, Result, error) {
	var stats Result
	if err := ctx.Err(); err != nil {
		return "", stats, err
	}
	encoded, err := encodePNG(img)
	if err != nil {
		return "", stats, err
	}
	prompt := basePrompt +
		"\n\n<recovery_tile>\n" +
		"This image is ordered OCR recovery tile " + path + ". Extract every recognizable item visible in this tile.\n" +
		"Never emit blank Markdown table rows or continue a table after the last visible character.\n" +
		"The tile overlaps its neighbor; do not invent text outside the visible pixels.\n" +
		"</recovery_tile>"
	text, predictErr := predict(ctx, [][]byte{encoded}, prompt)
	stats.Calls++
	if predictErr == nil {
		text, removed := ocrstructure.TrimEmptyTableTail(text)
		stats.EmptyRowsTrimmed += removed
		stats.LeafTiles++
		return text, stats, nil
	}
	if !recoverable(predictErr) {
		return "", stats, fmt.Errorf("OCR recovery tile %s: %w", path, predictErr)
	}
	if depth >= maxRecoveryDepth {
		return "", stats, fmt.Errorf("%w at tile %s: %v", ErrIncomplete, path, predictErr)
	}
	children := split(img)
	texts := make([]string, 0, len(children))
	for index, child := range children {
		childText, childStats, childErr := recoverTile(
			ctx, child, basePrompt, fmt.Sprintf("%s.%d", path, index+1), depth+1, predict,
		)
		stats.Calls += childStats.Calls
		stats.LeafTiles += childStats.LeafTiles
		stats.EmptyRowsTrimmed += childStats.EmptyRowsTrimmed
		if childErr != nil {
			return "", stats, childErr
		}
		texts = append(texts, childText)
	}
	return mergeOrdered(texts), stats, nil
}

func recoverable(err error) bool {
	kind, ok := vlmguard.FailureKindOf(err)
	return ok && (kind == vlmguard.FailureOutputLimit || kind == vlmguard.FailureRunaway)
}

func split(source image.Image) []image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	vertical := height >= width
	major := height
	if !vertical {
		major = width
	}
	overlap := major / 20
	if overlap < 16 {
		overlap = 16
	}
	if overlap > major/4 {
		overlap = major / 4
	}
	middle := major / 2
	if vertical {
		return []image.Image{
			crop(source, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Min.Y+middle+overlap)),
			crop(source, image.Rect(bounds.Min.X, bounds.Min.Y+middle-overlap, bounds.Max.X, bounds.Max.Y)),
		}
	}
	return []image.Image{
		crop(source, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Min.X+middle+overlap, bounds.Max.Y)),
		crop(source, image.Rect(bounds.Min.X+middle-overlap, bounds.Min.Y, bounds.Max.X, bounds.Max.Y)),
	}
}

func crop(source image.Image, rectangle image.Rectangle) image.Image {
	rectangle = rectangle.Intersect(source.Bounds())
	result := image.NewNRGBA(image.Rect(0, 0, rectangle.Dx(), rectangle.Dy()))
	draw.Draw(result, result.Bounds(), source, rectangle.Min, draw.Src)
	return result
}

func encodePNG(source image.Image) ([]byte, error) {
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		return nil, fmt.Errorf("encode OCR recovery tile: %w", err)
	}
	return output.Bytes(), nil
}

func mergeOrdered(parts []string) string {
	merged := make([]string, 0)
	for _, part := range parts {
		lines := nonEmptyEdgeLines(part)
		if len(lines) == 0 {
			continue
		}
		overlap := exactLineOverlap(merged, lines, 12)
		merged = append(merged, lines[overlap:]...)
	}
	return strings.TrimSpace(strings.Join(merged, "\n"))
}

func nonEmptyEdgeLines(text string) []string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func exactLineOverlap(left, right []string, maximum int) int {
	if maximum > len(left) {
		maximum = len(left)
	}
	if maximum > len(right) {
		maximum = len(right)
	}
	for size := maximum; size > 0; size-- {
		matched := true
		for index := 0; index < size; index++ {
			if strings.TrimSpace(left[len(left)-size+index]) != strings.TrimSpace(right[index]) {
				matched = false
				break
			}
		}
		if matched {
			return size
		}
	}
	return 0
}
