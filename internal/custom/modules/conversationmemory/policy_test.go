package conversationmemory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
		"current user message is the only active task",
		"classify every requested proposition by authority",
		"external claim exists only in assistant history, retrieve it again",
		"object/field/value/modality/source propositions",
		"retire only true conflicts",
		"asserted, unknown, explicitly pending, questioned, proposed, hypothetical and negated",
		"speech-act modality separate from proposition polarity",
		"licenses neither polarity",
		"current/latest label selects the newest active user value",
		"User facts and current evidence are closed sets",
		"only fields explicitly unresolved in user text or named by the requested bounded schema",
		"Rewrites, translations, summaries and handoffs",
		"actor/action/object/destination/scope",
		"Tool choice is model-owned",
		"deep-read only when returned evidence is truncated",
		"Citation handles are request-local",
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
		"Decompose the complete requested output into propositions",
		`evidence_need="none" only when every requested proposition`,
		"primary intent is conversation_state",
		"historical_assistant_output has no factual authority",
		"prior handles have expired",
		"Source availability does not create evidence need",
		"Analysis or explanation is dialogue-grounded",
		"evidence_query is the complete source-facing question",
		"Preserve identity-bearing object names, versions, identifiers, requested attributes",
		"Unknown, pending, proposed, questioned, hypothetical, negated and completed are distinct",
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
		"requested language, count, form and scope",
		"closed-source three-valued calculus",
		"same object, field, relation, value and modality",
		"current/latest selects the newest active user value",
		"do not complete absent fields",
		"rewrite, draft, summary or handoff",
		"Boundaries prove permission scope, not operation outcomes",
		"matching request-local citation handles",
		"constrain(output, P) permits neither polarity",
		"Do not say that you will search or answer later",
		"no final-answer or final-response tool exists",
	} {
		if !strings.Contains(directive, required) {
			t.Fatalf("terminal directive missing %q: %s", required, directive)
		}
	}
	if utf8.RuneCountInString(directive) > 1800 {
		t.Fatalf("terminal directive is too repetitive: %d runes", utf8.RuneCountInString(directive))
	}
}

func TestHistoricalAssistantOutputCannotMasqueradeAsUserSource(t *testing.T) {
	got := HistoricalAssistantOutput("possible owner: Alex")
	if !strings.Contains(got, historicalAssistantMarker) ||
		!strings.Contains(got, `factual_authority="none"`) ||
		!strings.Contains(got, `current_evidence_required_if_reused="true"`) ||
		!strings.Contains(got, "possible owner: Alex") {
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

func TestCurrentTaskIsNotDuplicated(t *testing.T) {
	task := strings.Repeat("source original ", 1000)
	if got := AppendCurrentTurnDirective(task, task); got != task {
		t.Fatal("task duplicated or changed")
	}
}
