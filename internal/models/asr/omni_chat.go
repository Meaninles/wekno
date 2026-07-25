package asr

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/audiochatasr"
	"github.com/Tencent/WeKnora/internal/logger"
)

// OmniChatASR adapts a multimodal chat-completions endpoint to the ASR
// interface. The transport itself lives under internal/custom so the native
// model package only retains this registration wrapper.
type OmniChatASR struct {
	modelName string
	modelID   string
	baseURL   string
	client    *audiochatasr.Client
}

func NewOmniChatASR(config *Config) (*OmniChatASR, error) {
	client, err := audiochatasr.New(audiochatasr.Config{
		BaseURL:   config.BaseURL,
		Model:     config.ModelName,
		APIKey:    config.APIKey,
		Prompt:    config.Prompt,
		Language:  config.Language,
		Path:      config.ChatPath,
		MaxTokens: config.MaxTokens,
		Headers:   config.CustomHeaders,
	})
	if err != nil {
		return nil, err
	}
	return &OmniChatASR{
		modelName: config.ModelName,
		modelID:   config.ModelID,
		baseURL:   config.BaseURL,
		client:    client,
	}, nil
}

func (s *OmniChatASR) Transcribe(
	ctx context.Context,
	audioBytes []byte,
	fileName string,
) (*TranscriptionResult, error) {
	logger.Infof(
		ctx,
		"[ASR] Calling multimodal chat transcription API, model=%s, baseURL=%s, audioSize=%d, file=%s",
		s.modelName,
		s.baseURL,
		len(audioBytes),
		fileName,
	)
	text, err := s.client.Transcribe(ctx, audioBytes, fileName)
	if err != nil {
		return nil, fmt.Errorf("ASR multimodal chat request failed: %w", err)
	}
	text = strings.TrimSpace(text)
	logger.Infof(ctx, "[ASR] Multimodal chat transcription completed, text length=%d", len(text))
	return &TranscriptionResult{Text: text}, nil
}

func (s *OmniChatASR) GetModelName() string { return s.modelName }
func (s *OmniChatASR) GetModelID() string   { return s.modelID }
