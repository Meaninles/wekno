package embedding

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/Tencent/WeKnora/internal/models/utils"
	"github.com/panjf2000/ants/v2"
)

type batchEmbedder struct {
	pool *ants.Pool
}

func NewBatchEmbedder(pool *ants.Pool) EmbedderPooler {
	return &batchEmbedder{pool: pool}
}

type textEmbedding struct {
	text    string
	results []float32
}

func (e *batchEmbedder) BatchEmbedWithPool(ctx context.Context, model Embedder, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	if e == nil || e.pool == nil {
		return nil, fmt.Errorf("embedding worker pool is not configured")
	}
	// Create goroutine pool for concurrent processing of document chunks
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	setFirstError := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	getFirstError := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr
	}
	batchSizeStr := os.Getenv("BATCH_EMBED_SIZE")
	if batchSizeStr == "" {
		batchSizeStr = "5"
	}
	batchSize, err := strconv.Atoi(batchSizeStr)
	if err != nil {
		return nil, fmt.Errorf("parse BATCH_EMBED_SIZE: %w", err)
	}
	if batchSize <= 0 {
		return nil, fmt.Errorf("BATCH_EMBED_SIZE must be greater than zero")
	}
	textEmbeddings := utils.MapSlice(texts, func(text string) *textEmbedding {
		return &textEmbedding{text: text}
	})

	// Function to process each document chunk
	processChunk := func(texts []*textEmbedding) func() {
		return func() {
			defer wg.Done()
			// If an error has already occurred, don't continue processing
			if getFirstError() != nil {
				return
			}
			// Embed text
			embedding, err := model.BatchEmbed(workCtx, utils.MapSlice(texts, func(text *textEmbedding) string {
				return text.text
			}))
			if err != nil {
				setFirstError(err)
				return
			}
			if len(embedding) != len(texts) {
				setFirstError(fmt.Errorf(
					"embedding provider returned %d vectors for %d inputs",
					len(embedding),
					len(texts),
				))
				return
			}
			for i, text := range texts {
				if text == nil {
					continue
				}
				text.results = embedding[i]
			}
		}
	}

	// Submit all tasks to the goroutine pool
	for _, texts := range utils.ChunkSlice(textEmbeddings, batchSize) {
		wg.Add(1)
		err := e.pool.Submit(processChunk(texts))
		if err != nil {
			// Submit did not transfer ownership of this WaitGroup slot.
			wg.Done()
			setFirstError(fmt.Errorf("submit embedding batch: %w", err))
			break
		}
	}

	// Wait for all tasks to complete
	wg.Wait()

	// Check if any errors occurred
	if err := getFirstError(); err != nil {
		return nil, err
	}

	results := utils.MapSlice(textEmbeddings, func(text *textEmbedding) []float32 {
		return text.results
	})
	return results, nil
}
