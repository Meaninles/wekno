package modeladmission

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/models/asr"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types"
)

func finishLease(lease *Lease, callErr error) error {
	fencingErr := lease.FencingError()
	resultErr := lease.Complete(callErr)
	lease.Release()
	if fencingErr != nil {
		if resultErr != nil {
			return errors.Join(resultErr, fencingErr)
		}
		return fencingErr
	}
	return resultErr
}

type admittedChat struct {
	manager *Manager
	spec    Spec
	inner   chat.Chat
}

func WrapChat(manager *Manager, spec Spec, inner chat.Chat) chat.Chat {
	if inner == nil || manager == nil || !manager.config.Enabled {
		return inner
	}
	return &admittedChat{manager: manager, spec: spec, inner: inner}
}

func (w *admittedChat) Chat(
	ctx context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (*types.ChatResponse, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	if err := runAdmissionGrantedHook(lease.Context()); err != nil {
		lease.Release()
		return nil, err
	}
	response, callErr := w.inner.Chat(lease.Context(), messages, options)
	return response, finishLease(lease, callErr)
}

func (w *admittedChat) ChatStream(
	ctx context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	if err := runAdmissionGrantedHook(lease.Context()); err != nil {
		lease.Release()
		return nil, err
	}
	stream, callErr := w.inner.ChatStream(lease.Context(), messages, options)
	if callErr != nil {
		return nil, finishLease(lease, callErr)
	}
	output := make(chan types.StreamResponse, 16)
	go func() {
		var streamErr error
		defer close(output)
		defer func() {
			lease.Finish(streamErr)
			lease.Release()
		}()
		emitLeaseLoss := func() {
			if lease.FencingError() == nil {
				return
			}
			streamErr = ErrAdmissionLeaseLost
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case output <- types.StreamResponse{
				ResponseType: types.ResponseTypeError,
				Content:      ErrAdmissionLeaseLost.Error(),
				Done:         true,
			}:
			case <-ctx.Done():
			case <-timer.C:
			}
		}
		for {
			select {
			case <-lease.Context().Done():
				if streamErr == nil {
					streamErr = context.Cause(lease.Context())
				}
				emitLeaseLoss()
				return
			case item, ok := <-stream:
				if !ok {
					emitLeaseLoss()
					return
				}
				if item.ResponseType == types.ResponseTypeError && streamErr == nil {
					streamErr = errors.New(item.Content)
				}
				select {
				case output <- item:
				case <-lease.Context().Done():
					emitLeaseLoss()
					return
				}
			}
		}
	}()
	return output, nil
}

func (w *admittedChat) GetModelName() string { return w.inner.GetModelName() }
func (w *admittedChat) GetModelID() string   { return w.inner.GetModelID() }

type admittedEmbedder struct {
	manager *Manager
	spec    Spec
	inner   embedding.Embedder
}

func WrapEmbedder(manager *Manager, spec Spec, inner embedding.Embedder) embedding.Embedder {
	if inner == nil || manager == nil || !manager.config.Enabled {
		return inner
	}
	return &admittedEmbedder{manager: manager, spec: spec, inner: inner}
}

func (w *admittedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	response, callErr := w.inner.Embed(lease.Context(), text)
	return response, finishLease(lease, callErr)
}

func (w *admittedEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	response, callErr := w.inner.BatchEmbed(lease.Context(), texts)
	return response, finishLease(lease, callErr)
}

func (w *admittedEmbedder) BatchEmbedWithPool(
	ctx context.Context,
	_ embedding.Embedder,
	texts []string,
) ([][]float32, error) {
	// Pass this outer wrapper to the pool. Every actual provider batch then
	// obtains its own distributed lease, so local batch parallelism cannot
	// multiply the provider concurrency across Pods.
	return w.inner.BatchEmbedWithPool(ctx, w, texts)
}

func (w *admittedEmbedder) GetModelName() string { return w.inner.GetModelName() }
func (w *admittedEmbedder) GetDimensions() int   { return w.inner.GetDimensions() }
func (w *admittedEmbedder) GetModelID() string   { return w.inner.GetModelID() }
func (w *admittedEmbedder) GetMaxInputTokens() int {
	return embedding.MaxInputTokens(w.inner)
}

type admittedReranker struct {
	manager *Manager
	spec    Spec
	inner   rerank.Reranker
}

func WrapReranker(manager *Manager, spec Spec, inner rerank.Reranker) rerank.Reranker {
	if inner == nil || manager == nil || !manager.config.Enabled {
		return inner
	}
	return &admittedReranker{manager: manager, spec: spec, inner: inner}
}

func (w *admittedReranker) Rerank(
	ctx context.Context,
	query string,
	documents []string,
) ([]rerank.RankResult, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	response, callErr := w.inner.Rerank(lease.Context(), query, documents)
	return response, finishLease(lease, callErr)
}

func (w *admittedReranker) GetModelName() string { return w.inner.GetModelName() }
func (w *admittedReranker) GetModelID() string   { return w.inner.GetModelID() }

type admittedVLM struct {
	manager *Manager
	spec    Spec
	inner   vlm.VLM
}

func WrapVLM(manager *Manager, spec Spec, inner vlm.VLM) vlm.VLM {
	if inner == nil || manager == nil || !manager.config.Enabled {
		return inner
	}
	return &admittedVLM{manager: manager, spec: spec, inner: inner}
}

func (w *admittedVLM) Predict(
	ctx context.Context,
	images [][]byte,
	prompt string,
) (string, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return "", err
	}
	response, callErr := w.inner.Predict(lease.Context(), images, prompt)
	return response, finishLease(lease, callErr)
}

func (w *admittedVLM) GetModelName() string { return w.inner.GetModelName() }
func (w *admittedVLM) GetModelID() string   { return w.inner.GetModelID() }

type admittedASR struct {
	manager *Manager
	spec    Spec
	inner   asr.ASR
}

func WrapASR(manager *Manager, spec Spec, inner asr.ASR) asr.ASR {
	if inner == nil || manager == nil || !manager.config.Enabled {
		return inner
	}
	return &admittedASR{manager: manager, spec: spec, inner: inner}
}

func (w *admittedASR) Transcribe(
	ctx context.Context,
	audioBytes []byte,
	fileName string,
) (*asr.TranscriptionResult, error) {
	lease, err := w.manager.Acquire(ctx, w.spec)
	if err != nil {
		return nil, err
	}
	response, callErr := w.inner.Transcribe(lease.Context(), audioBytes, fileName)
	return response, finishLease(lease, callErr)
}

func (w *admittedASR) GetModelName() string { return w.inner.GetModelName() }
func (w *admittedASR) GetModelID() string   { return w.inner.GetModelID() }
