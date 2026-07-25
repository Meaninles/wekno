package embedding

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/stretchr/testify/require"
)

type batchTestEmbedder struct {
	calls atomic.Int32
	fn    func(context.Context, []string) ([][]float32, error)
}

func (e *batchTestEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	result, err := e.BatchEmbed(ctx, []string{text})
	if err != nil || len(result) == 0 {
		return nil, err
	}
	return result[0], nil
}

func (e *batchTestEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	return e.fn(ctx, texts)
}

func (e *batchTestEmbedder) BatchEmbedWithPool(
	ctx context.Context,
	_ Embedder,
	texts []string,
) ([][]float32, error) {
	return e.BatchEmbed(ctx, texts)
}

func (*batchTestEmbedder) GetModelName() string { return "batch-test" }
func (*batchTestEmbedder) GetDimensions() int   { return 1 }
func (*batchTestEmbedder) GetModelID() string   { return "batch-test-id" }

func newBatchTestPool(t *testing.T, size int) *ants.Pool {
	t.Helper()
	pool, err := ants.NewPool(size)
	require.NoError(t, err)
	t.Cleanup(pool.Release)
	return pool
}

func TestBatchEmbedWithPoolPreservesInputOrder(t *testing.T) {
	t.Setenv("BATCH_EMBED_SIZE", "2")
	model := &batchTestEmbedder{fn: func(_ context.Context, texts []string) ([][]float32, error) {
		// Make completion order differ from input order.
		if texts[0] == "0" {
			time.Sleep(10 * time.Millisecond)
		}
		result := make([][]float32, len(texts))
		for i, text := range texts {
			value, err := strconv.Atoi(text)
			require.NoError(t, err)
			result[i] = []float32{float32(value)}
		}
		return result, nil
	}}

	result, err := NewBatchEmbedder(newBatchTestPool(t, 4)).BatchEmbedWithPool(
		context.Background(),
		model,
		[]string{"0", "1", "2", "3", "4"},
	)
	require.NoError(t, err)
	require.Equal(t, [][]float32{{0}, {1}, {2}, {3}, {4}}, result)
	require.EqualValues(t, 3, model.calls.Load())
}

func TestBatchEmbedWithPoolRejectsProviderCardinalityMismatch(t *testing.T) {
	t.Setenv("BATCH_EMBED_SIZE", "2")
	model := &batchTestEmbedder{fn: func(context.Context, []string) ([][]float32, error) {
		return [][]float32{{1}}, nil
	}}

	result, err := NewBatchEmbedder(newBatchTestPool(t, 2)).BatchEmbedWithPool(
		context.Background(),
		model,
		[]string{"one", "two"},
	)
	require.Nil(t, result)
	require.ErrorContains(t, err, "returned 1 vectors for 2 inputs")
}

func TestBatchEmbedWithPoolFirstErrorIsRaceFreeAndCancelsPeers(t *testing.T) {
	t.Setenv("BATCH_EMBED_SIZE", "1")
	expected := errors.New("provider rejected batch")
	model := &batchTestEmbedder{fn: func(ctx context.Context, texts []string) ([][]float32, error) {
		if texts[0] == "fail" {
			return nil, expected
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := NewBatchEmbedder(newBatchTestPool(t, 8)).BatchEmbedWithPool(
		ctx,
		model,
		[]string{"wait-1", "wait-2", "fail", "wait-3"},
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.True(t, errors.Is(err, expected) || errors.Is(err, context.Canceled), err)
}

func TestBatchEmbedWithPoolRejectsInvalidBatchSize(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid"} {
		t.Run(strings.ReplaceAll(value, "-", "negative-"), func(t *testing.T) {
			t.Setenv("BATCH_EMBED_SIZE", value)
			model := &batchTestEmbedder{fn: func(context.Context, []string) ([][]float32, error) {
				return nil, fmt.Errorf("must not be called")
			}}
			result, err := NewBatchEmbedder(newBatchTestPool(t, 1)).BatchEmbedWithPool(
				context.Background(),
				model,
				[]string{"one"},
			)
			require.Nil(t, result)
			require.Error(t, err)
			require.Zero(t, model.calls.Load())
		})
	}
}

func TestBatchEmbedWithPoolWaitsForAcceptedWorkOnSubmitFailure(t *testing.T) {
	t.Setenv("BATCH_EMBED_SIZE", "1")
	pool := newBatchTestPool(t, 1)
	pool.Release()
	model := &batchTestEmbedder{fn: func(context.Context, []string) ([][]float32, error) {
		return [][]float32{{1}}, nil
	}}

	result, err := NewBatchEmbedder(pool).BatchEmbedWithPool(
		context.Background(),
		model,
		[]string{"one"},
	)
	require.Nil(t, result)
	require.ErrorContains(t, err, "submit embedding batch")
}
