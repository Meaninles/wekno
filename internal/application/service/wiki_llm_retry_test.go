package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestWikiLLMRetryDelay(t *testing.T) {
	halfWindow := func(window time.Duration) time.Duration { return window / 2 }
	policy := wikiqueue.DefaultRetryPolicy()
	policy.Jitter = halfWindow

	t.Run("honors Retry-After and adds only positive jitter", func(t *testing.T) {
		err := &chat.HTTPStatusError{
			StatusCode:        http.StatusTooManyRequests,
			RetryAfter:        40 * time.Second,
			RetryAfterPresent: true,
		}
		got, providerDirected := policy.Delay(err, 1)
		if !providerDirected || got != 42*time.Second {
			t.Fatalf("wikiLLMRetryDelay() = (%s, %v), want (42s, true)", got, providerDirected)
		}
	})

	t.Run("Retry-After zero has an anti-storm floor", func(t *testing.T) {
		err := &chat.HTTPStatusError{
			StatusCode:        http.StatusTooManyRequests,
			RetryAfterPresent: true,
		}
		zeroJitterPolicy := policy
		zeroJitterPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
		got, providerDirected := zeroJitterPolicy.Delay(err, 1)
		if !providerDirected || got != policy.MinDelay {
			t.Fatalf("RetryPolicy.Delay() = (%s, %v), want (%s, true)", got, providerDirected, policy.MinDelay)
		}
	})

	t.Run("large Retry-After is never shortened", func(t *testing.T) {
		err := &chat.HTTPStatusError{
			StatusCode:        http.StatusServiceUnavailable,
			RetryAfter:        24 * time.Hour,
			RetryAfterPresent: true,
		}
		zeroJitterPolicy := policy
		zeroJitterPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
		got, providerDirected := zeroJitterPolicy.Delay(err, 1)
		if !providerDirected || got != 24*time.Hour {
			t.Fatalf("RetryPolicy.Delay() = (%s, %v), want (24h, true)", got, providerDirected)
		}
	})

	t.Run("fallback is exponential with additive jitter", func(t *testing.T) {
		gotFirst, directedFirst := policy.Delay(errors.New("temporary timeout"), 1)
		gotSecond, directedSecond := policy.Delay(errors.New("temporary timeout"), 2)
		if directedFirst || directedSecond {
			t.Fatal("fallback delay incorrectly reported as provider-directed")
		}
		if gotFirst != 18*time.Second+750*time.Millisecond {
			t.Fatalf("first fallback = %s, want 18.75s", gotFirst)
		}
		if gotSecond != 37*time.Second+500*time.Millisecond {
			t.Fatalf("second fallback = %s, want 37.5s", gotSecond)
		}
	})
}

func TestGenerateWithTemplateUsesRetryAfterWithoutSleeping(t *testing.T) {
	model := &retrySequenceChatModel{
		errs: []error{
			&chat.HTTPStatusError{
				StatusCode:        http.StatusTooManyRequests,
				RetryAfter:        23 * time.Second,
				RetryAfterPresent: true,
			},
			&chat.HTTPStatusError{StatusCode: http.StatusServiceUnavailable},
		},
		response: "recovered",
	}
	var waits []time.Duration
	retryPolicy := wikiqueue.DefaultRetryPolicy()
	retryPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
	retryPolicy.Wait = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	service := &wikiIngestService{
		llmRetryPolicy: &retryPolicy,
	}

	got, err := service.generateWithTemplate(context.Background(), model, "{{.Value}}", map[string]string{"Value": "prompt"})
	if err != nil {
		t.Fatalf("generateWithTemplate() error = %v", err)
	}
	if got != "recovered" {
		t.Fatalf("generateWithTemplate() = %q, want recovered", got)
	}
	if model.calls != 3 {
		t.Fatalf("Chat() calls = %d, want 3", model.calls)
	}
	wantWaits := []time.Duration{23 * time.Second, 30 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits = %v, want %v", waits, wantWaits)
	}
	for i := range wantWaits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("waits[%d] = %s, want %s", i, waits[i], wantWaits[i])
		}
	}
}

func TestGenerateWithTemplateRetryWaitPreservesCancellation(t *testing.T) {
	model := &retrySequenceChatModel{
		errs: []error{&chat.HTTPStatusError{StatusCode: http.StatusTooManyRequests}},
	}
	retryPolicy := wikiqueue.DefaultRetryPolicy()
	retryPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
	retryPolicy.Wait = func(context.Context, time.Duration) error { return context.Canceled }
	service := &wikiIngestService{
		llmRetryPolicy: &retryPolicy,
	}

	_, err := service.generateWithTemplate(context.Background(), model, "prompt", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("generateWithTemplate() error = %v, want errors.Is(context.Canceled)", err)
	}
	if model.calls != 1 {
		t.Fatalf("Chat() calls = %d, want 1 after cancelled wait", model.calls)
	}
}

func TestResolveWikiIngestParallelism(t *testing.T) {
	tests := []struct {
		name       string
		config     *types.WikiConfig
		wantMap    int
		wantReduce int
	}{
		{name: "nil uses conservative defaults", wantMap: 2, wantReduce: 2},
		{name: "zero uses conservative defaults", config: &types.WikiConfig{}, wantMap: 2, wantReduce: 2},
		{
			name:       "explicit overrides remain authoritative",
			config:     &types.WikiConfig{IngestMapParallel: 7, IngestReduceParallel: 9},
			wantMap:    7,
			wantReduce: 9,
		},
		{
			name:       "partial override preserves other default",
			config:     &types.WikiConfig{IngestMapParallel: 4},
			wantMap:    4,
			wantReduce: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMap, gotReduce := wikiqueue.ResolveIngestParallelism(tt.config)
			if gotMap != tt.wantMap || gotReduce != tt.wantReduce {
				t.Fatalf("resolveWikiIngestParallelism() = (%d, %d), want (%d, %d)", gotMap, gotReduce, tt.wantMap, tt.wantReduce)
			}
		})
	}
}

type retrySequenceChatModel struct {
	errs     []error
	response string
	calls    int
}

func (m *retrySequenceChatModel) Chat(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (*types.ChatResponse, error) {
	call := m.calls
	m.calls++
	if call < len(m.errs) && m.errs[call] != nil {
		return nil, m.errs[call]
	}
	return &types.ChatResponse{Content: m.response}, nil
}

func (m *retrySequenceChatModel) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *retrySequenceChatModel) GetModelName() string { return "retry-sequence" }
func (m *retrySequenceChatModel) GetModelID() string   { return "retry-sequence" }
