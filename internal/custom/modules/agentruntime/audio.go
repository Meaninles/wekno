package agentruntime

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

const maxReturnedAudioTranscriptBytes = 2 * 1024 * 1024

func (s *Service) transcribeInputFile(
	ctx context.Context,
	row *RunRecord,
	payload ChatPayload,
	arguments map[string]any,
	config *types.AgentConfig,
) (*types.ToolResult, error) {
	if config == nil || strings.TrimSpace(config.ASRModelID) == "" {
		return nil, fmt.Errorf("the Agent has no configured ASR model")
	}
	inputID, _ := arguments["input_file_id"].(string)
	inputID = strings.TrimSpace(inputID)
	if inputID == "" {
		return nil, fmt.Errorf("input_file_id is required")
	}
	var spec *OriginalInputFileSpec
	for index := range payload.OriginalInputFiles {
		if payload.OriginalInputFiles[index].ID == inputID {
			spec = &payload.OriginalInputFiles[index]
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("input file is not part of the current run")
	}
	if !hasAudioOriginalInput([]OriginalInputFileSpec{*spec}) {
		return nil, fmt.Errorf("input file %s is not an audio file", spec.FileName)
	}
	data, fileName, _, err := s.ResolveRunFile(ctx, row.ID, "input_file", inputID)
	if err != nil {
		return nil, err
	}
	modelCtx := context.WithValue(ctx, types.TenantIDContextKey, config.AgentTenantID)
	transcriber, err := s.modelService.GetASRModel(modelCtx, config.ASRModelID)
	if err != nil {
		return nil, fmt.Errorf("load configured ASR model: %w", err)
	}
	result, err := transcriber.Transcribe(ctx, data, fileName)
	if err != nil {
		return nil, fmt.Errorf("audio transcription failed: %w", err)
	}
	if result == nil || strings.TrimSpace(result.Text) == "" {
		return nil, fmt.Errorf("audio transcription returned no text")
	}
	text := result.Text
	truncated := false
	if len([]byte(text)) > maxReturnedAudioTranscriptBytes {
		text = truncateUTF8(text, maxReturnedAudioTranscriptBytes)
		truncated = true
	}
	output := fmt.Sprintf("Audio transcript for %s (input_file_id=%s):\n%s", fileName, inputID, text)
	if truncated {
		output += "\n[Transcript display was truncated; use the timestamped segments/data when more detail is needed.]"
	}
	dataMap := map[string]interface{}{
		"input_file_id":     inputID,
		"file_name":         fileName,
		"model_id":          config.ASRModelID,
		"text":              result.Text,
		"segments":          result.Segments,
		"display_truncated": truncated,
	}
	return &types.ToolResult{Success: true, Output: output, Data: dataMap}, nil
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	encoded := []byte(value[:limit])
	for len(encoded) > 0 && !utf8.Valid(encoded) {
		encoded = encoded[:len(encoded)-1]
	}
	return string(encoded)
}
