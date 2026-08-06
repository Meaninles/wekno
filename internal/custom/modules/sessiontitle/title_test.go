package sessiontitle

import "testing"

func TestNormalizeModelTitleStripsThinkingAndLabels(t *testing.T) {
	got := NormalizeModelTitle("<think>内部推理</think>\n标题：采购管理办法\n多余内容")
	if got != "采购管理办法" {
		t.Fatalf("NormalizeModelTitle = %q", got)
	}
}

func TestNormalizeModelTitleRejectsThinkingOnly(t *testing.T) {
	if got := NormalizeModelTitle("<think>内部推理</think>"); got != "" {
		t.Fatalf("NormalizeModelTitle = %q, want empty", got)
	}
}

func TestFallbackNeverReturnsEmpty(t *testing.T) {
	if got := Fallback("  一个小项目30万，买大模型API服务，应该怎么走采购流程？  "); got == "" || got == "新会话" {
		t.Fatalf("Fallback = %q", got)
	}
}
