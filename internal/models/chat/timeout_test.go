package chat

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestWithLLMTimeout_NoParentDeadline_AppliesDefault(t *testing.T) {
	ctx, cancel := withLLMTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected deadline to be set when parent has none")
	}
	if remaining := time.Until(dl); remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("unexpected remaining duration: %v", remaining)
	}
}

func TestWithLLMTimeout_ShorterParentDeadline_Respected(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer parentCancel()

	ctx, cancel := withLLMTimeout(parent, 10*time.Second)
	defer cancel()

	dl, _ := ctx.Deadline()
	if remaining := time.Until(dl); remaining > 30*time.Millisecond {
		t.Fatalf("parent shorter deadline should be respected, got remaining=%v", remaining)
	}
}

func TestWithLLMTimeout_LongerParentDeadline_IsCappedPerCall(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer parentCancel()

	ctx, cancel := withLLMTimeout(parent, 50*time.Millisecond)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("deadline must be set")
	}
	if remaining := time.Until(dl); remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("long workflow deadline must be capped by the per-call timeout, got remaining=%v", remaining)
	}
}

func TestWithLLMTimeout_NonPositiveCeilingLeavesParentUnchanged(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer parentCancel()

	ctx, cancel := withLLMTimeout(parent, 0)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("parent deadline must remain visible")
	}
	if remaining := time.Until(dl); remaining < 5*time.Second {
		t.Fatalf("disabled ceiling must not shorten parent, got remaining=%v", remaining)
	}
}

func TestEnvDurationSeconds(t *testing.T) {
	const key = "WEKNORA_TEST_TIMEOUT_SECONDS"

	t.Run("unset returns fallback", func(t *testing.T) {
		os.Unsetenv(key)
		if got := envDurationSeconds(key, 7*time.Second); got != 7*time.Second {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("valid value parsed", func(t *testing.T) {
		t.Setenv(key, "42")
		if got := envDurationSeconds(key, 1*time.Second); got != 42*time.Second {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv(key, "not-a-number")
		if got := envDurationSeconds(key, 9*time.Second); got != 9*time.Second {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("non-positive falls back", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := envDurationSeconds(key, 9*time.Second); got != 9*time.Second {
			t.Fatalf("got %v", got)
		}
		t.Setenv(key, "-5")
		if got := envDurationSeconds(key, 9*time.Second); got != 9*time.Second {
			t.Fatalf("got %v", got)
		}
	})
}
