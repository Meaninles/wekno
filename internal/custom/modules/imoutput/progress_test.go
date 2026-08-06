package imoutput

import (
	"testing"
	"time"
)

func TestFormatRunWaitingUsesStableFiveSecondBuckets(t *testing.T) {
	tests := []struct {
		elapsed time.Duration
		want    string
	}{
		{elapsed: -time.Second, want: "正在处理，请稍候"},
		{elapsed: 0, want: "正在处理，请稍候"},
		{elapsed: 4999 * time.Millisecond, want: "正在处理，请稍候"},
		{elapsed: 5 * time.Second, want: "正在处理，请稍候 · 已用 5 秒"},
		{elapsed: 9999 * time.Millisecond, want: "正在处理，请稍候 · 已用 5 秒"},
		{elapsed: 65 * time.Second, want: "正在处理，请稍候 · 已用 65 秒"},
	}

	for _, test := range tests {
		if got := FormatRunWaiting(test.elapsed); got != test.want {
			t.Fatalf("FormatRunWaiting(%s) = %q, want %q", test.elapsed, got, test.want)
		}
	}
}
