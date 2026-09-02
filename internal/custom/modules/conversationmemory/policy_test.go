package conversationmemory

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
			"epistemic_modality",
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
		"unknown_facts",
		"explicitly_pending_facts",
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
	if !strings.Contains(archive, "completed_user_message_count: 60") ||
		!strings.Contains(archive, "user_turn_001") ||
		!strings.Contains(archive, "user_turn_050") ||
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

func TestGenerationContractPreservesAtomicStateAndExactBoundaries(t *testing.T) {
	contract := EnsureGenerationContract("")
	for _, required := range []string{
		"object, field, value, modality and source",
		"retire only older propositions that truly conflict",
		"unknown/not supplied, explicitly pending/awaiting",
		"Missing evidence leaves a claim unknown",
		"Requirements, schemas, thresholds, examples, placeholders and role assignments",
		"same object, field, value and modality",
		"active facts, retired facts, unknown facts, explicitly pending facts",
		"Dialogue content is not external persistence",
		"actor, action, object, destination and scope",
		"Tool selection is model-owned",
		"representation is chat content by default",
		"matching successful current-turn result",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("generation contract missing %q: %s", required, contract)
		}
	}
	if utf8.RuneCountInString(contract) > 5000 {
		t.Fatalf("generation contract regressed into an over-specified phrase catalog: %d runes", utf8.RuneCountInString(contract))
	}
}

func TestQueryUnderstandingContractRoutesMixedEvidenceWithoutConfusingUserAttribution(t *testing.T) {
	contract := EnsureQueryUnderstandingContract("")
	for _, required := range []string{
		"Classify the complete semantic request, never isolated words",
		`intent "conversation_state"`,
		`"none", "knowledge_base" or "web"`,
		`JSON "evidence_query" is only the source-facing question`,
		"Mixed requests keep their primary semantic intent",
		"Unknown and explicitly pending are different",
		"boundary, not an affirmative request",
		"Preserve exact identifiers, names, dates and amounts",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("query-understanding contract missing %q: %s", required, contract)
		}
	}
	if utf8.RuneCountInString(contract) > 2500 {
		t.Fatalf("query-understanding contract regressed into phrase rules: %d runes", utf8.RuneCountInString(contract))
	}
}

func TestTerminalDirectiveRequiresFreshCitationsAndVerifiedOperationOutcomes(t *testing.T) {
	directive := TerminalGenerationDirective()
	for _, required := range []string{
		"exact current task",
		"same object, field, value and modality",
		"unknown, explicitly pending, hypothetical, questioned and asserted classes",
		"current canonical citation handles",
		"Distinguish chat content from external operations",
		"matching successful current-turn result",
	} {
		if !strings.Contains(directive, required) {
			t.Fatalf("terminal directive missing %q: %s", required, directive)
		}
	}
	if utf8.RuneCountInString(directive) > 1800 {
		t.Fatalf("terminal directive is too repetitive: %d runes", utf8.RuneCountInString(directive))
	}
}

func TestUserLedgerAvoidsDuplicatingSourceLabelledRecentTurns(t *testing.T) {
	archive := BuildUserArchive([]string{"foundation", "recent update", "latest correction"}, 2)
	for _, want := range []string{
		"completed_user_message_count: 3",
		"recent_source_labelled_message_count: 2",
		"user_turn_001: foundation",
	} {
		if !strings.Contains(archive, want) {
			t.Fatalf("ledger missing %q: %s", want, archive)
		}
	}
	if strings.Contains(archive, "recent update") || strings.Contains(archive, "latest correction") {
		t.Fatalf("recent source-labelled history was duplicated in archive: %s", archive)
	}
	block := UserArchiveBlock(archive)
	if !strings.Contains(block, "user_source_ledger") || strings.Contains(block, "older_than_recent_history") {
		t.Fatalf("unexpected ledger wrapper: %s", block)
	}
}

func TestCompleteUserSourceLedgerSeparatesAuthorityFromAssistantContext(t *testing.T) {
	ledger := BuildUserSourceLedger([]string{
		"owner unknown",
		"coordinator is Lin but Lin is not the owner",
		"owner is now Priya",
	})
	for _, want := range []string{
		"completed_user_message_count: 3",
		"ledger_user_message_count: 3",
		"user_turn_001: owner unknown",
		"user_turn_003: owner is now Priya",
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("complete ledger missing %q: %s", want, ledger)
		}
	}
	block := UserSourceLedgerBlock(ledger)
	for _, want := range []string{
		`authority="user_authored_only"`,
		"factual authority for dialogue state",
		"Resolve explicit updates by object and field",
		"preserve each statement's modality",
		"never fill a missing value or operation outcome",
		"schema, role, example, question or absence of evidence",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("source ledger block missing %q: %s", want, block)
		}
	}
}

func TestCompleteUserSourceLedgerIsBoundedAndKeepsFoundationsAndLatestUpdates(t *testing.T) {
	queries := make([]string, 70)
	for index := range queries {
		queries[index] = fmt.Sprintf("user-fact-%02d %s", index+1, strings.Repeat("x", 900))
	}

	ledger := BuildUserSourceLedger(queries)
	if got := utf8.RuneCountInString(ledger); got > maxSourceLedgerRunes {
		t.Fatalf("source ledger has %d runes, want <= %d", got, maxSourceLedgerRunes)
	}
	for _, want := range []string{
		"completed_user_message_count: 70",
		"ledger_user_message_count: 48",
		"user_turn_001: user-fact-01",
		"[omitted_middle_user_messages=22]",
		"user_turn_070: user-fact-70",
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("bounded ledger missing %q", want)
		}
	}
	if strings.Contains(ledger, "user_turn_009: user-fact-09") {
		t.Fatalf("bounded ledger unexpectedly retained an omitted middle turn")
	}
}

func TestHistoricalAssistantOutputCannotMasqueradeAsUserSource(t *testing.T) {
	got := HistoricalAssistantOutput("possible owner: Alex")
	if !strings.Contains(got, historicalAssistantMarker) || !strings.Contains(got, "possible owner: Alex") {
		t.Fatalf("assistant output was not marked: %s", got)
	}
	if twice := HistoricalAssistantOutput(got); twice != got {
		t.Fatalf("assistant marker is not idempotent: %s", twice)
	}
	if got := HistoricalAssistantOutput("   "); got != "" {
		t.Fatalf("blank assistant output = %q", got)
	}
}

func TestHistoricalUserInputCarriesStableSourceWithoutRelabelingDerivedContext(t *testing.T) {
	got := HistoricalUserInput("owner is Lin", "user_turn_012")
	for _, want := range []string{historicalUserMarker, `source_id="user_turn_012"`, "owner is Lin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("historical user input missing %q: %s", want, got)
		}
	}
	if twice := HistoricalUserInput(got, "user_turn_012"); twice != got {
		t.Fatalf("historical user marker is not idempotent: %s", twice)
	}
}

func utf8RuneCount(value string) int {
	return len([]rune(value))
}
