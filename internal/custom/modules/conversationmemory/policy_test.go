package conversationmemory

import (
	"strings"
	"testing"
)

func TestCurrentTurnContextPreservesCrossDomainAndAdversarialUserText(t *testing.T) {
	queries := []string{
		"把面试状态更新为待复核，只记录，不发邮件。",
		"Record the incident as mitigated; do not restart any service.",
		"请分析为什么这个命令不能执行，并给出排障思路。",
		"什么是“不安装代理”模式？请从产品手册检索后说明。",
		"不要检索，只根据我刚才给的信息汇总。",
		"修改发布方案的回滚章节，但不要修改文件，也不要执行部署。",
	}
	for _, query := range queries {
		got := AppendCurrentTurnDirective("CURRENT", query)
		for _, required := range []string{
			turnSemanticMarker,
			"semantic_dimensions",
			"source_fragments",
			"current_user_message",
		} {
			if !strings.Contains(got, required) {
				t.Fatalf("context for %q missing %q: %s", query, required, got)
			}
		}
		if strings.Contains(got, `"state_maintenance":true`) ||
			strings.Contains(got, `"external_evidence_requested":true`) {
			t.Fatalf("finite phrase classifier changed runtime context for %q: %s", query, got)
		}
	}
}

func TestTerminalDirectiveDoesNotBranchOnScenarioPhrases(t *testing.T) {
	queries := []string{
		"分析为什么不能执行",
		"不要检索",
		"修改方案，但不要修改文件",
		"What does 'do not install' mean? Search the manual.",
	}
	want := TerminalGenerationDirective()
	for _, query := range queries[1:] {
		if got := TerminalGenerationDirective(); got != want {
			t.Fatalf("scenario phrase changed terminal behavior for %q", query)
		}
	}
}

func TestAppendCurrentTurnDirectiveUsesGenericUserDerivedStructure(t *testing.T) {
	query := "18万元是当前预算；更新值班状态：旧负责人作废，新负责人为Lin，升级窗口待确认；不要发送通知。"
	got := AppendCurrentTurnDirective("CURRENT", query)
	for _, required := range []string{
		turnSemanticMarker,
		"semantic_dimensions",
		"source_fragments",
		"active_facts",
		"retired_facts",
		"unknown_pending_facts",
		"source_attribution",
		"output_scope",
		"action_boundaries",
		"18万元是当前预算",
		"旧负责人作废",
		"升级窗口待确认",
		"不要发送通知",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("directive missing %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{
		"required_claim", "reference_answer", "judge", "case_id", "max_response_chars",
	} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("directive leaked Eval concept %q: %s", forbidden, got)
		}
	}
}

func TestCurrentTurnSourceFragmentIsExactAndJsonRemainsValidWhenBounded(t *testing.T) {
	query := "  保留标点：A&B，<原话>；以及前后空格。  "
	got := AppendCurrentTurnDirective("CURRENT", query)
	if !strings.Contains(got, `"text":"  保留标点：A&B，<原话>；以及前后空格。  "`) {
		t.Fatalf("current source fragment was not preserved exactly: %s", got)
	}
	long := strings.Repeat("界", maxCurrentSourceRunes+100)
	bounded := AppendCurrentTurnDirective("CURRENT", long)
	if !strings.Contains(bounded, `"truncated":true`) || strings.Contains(bounded, "…[truncated]") {
		t.Fatalf("bounded source must remain valid JSON without invented text: %s", bounded)
	}
}

func TestLongHistoryArchiveRetainsFoundationsAndRecentUpdates(t *testing.T) {
	queries := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		queries = append(queries, "用户事实-"+string(rune('A'+index%26)))
	}
	archive := BuildUserArchive(queries, 10)
	if !strings.Contains(archive, "earlier_user_message_01") ||
		!strings.Contains(archive, "earlier_user_message_50") ||
		!strings.Contains(archive, "omitted_middle_user_messages") {
		t.Fatalf("archive did not preserve bounded chronology: %s", archive)
	}
	if utf8RuneCount(archive) > maxArchiveRunes+20 {
		t.Fatalf("archive exceeded bound: %d", utf8RuneCount(archive))
	}
}

func TestUserArchiveKeepsSourceTextWithoutHTMLRewriting(t *testing.T) {
	archive := BuildUserArchive([]string{"保留 <A&B> 与 \"原话\"", "recent"}, 1)
	if !strings.Contains(archive, `保留 <A&B> 与 "原话"`) {
		t.Fatalf("archive changed user-authored source text: %s", archive)
	}
	if strings.Contains(archive, "&lt;") || strings.Contains(archive, "&amp;") {
		t.Fatalf("archive HTML-escaped user source text: %s", archive)
	}
}

func TestGenerationContractsAreIdempotentAndDomainNeutral(t *testing.T) {
	first := EnsureGenerationContract("base")
	if got := EnsureGenerationContract(first); got != first {
		t.Fatal("generation contract is not idempotent")
	}
	queryContract := EnsureQueryUnderstandingContract("base")
	if got := EnsureQueryUnderstandingContract(queryContract); got != queryContract {
		t.Fatal("rewrite contract is not idempotent")
	}
	for _, value := range []string{first, queryContract} {
		for _, forbidden := range []string{"采购", "培训", "required_claim", "case_id", "reference answer"} {
			if strings.Contains(strings.ToLower(value), strings.ToLower(forbidden)) {
				t.Fatalf("domain/Eval-specific term %q leaked into contract: %s", forbidden, value)
			}
		}
	}
}

func utf8RuneCount(value string) int {
	return len([]rune(value))
}
