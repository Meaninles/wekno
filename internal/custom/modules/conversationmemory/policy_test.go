package conversationmemory

import (
	"strings"
	"testing"
)

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
				t.Fatalf("domain-specific term %q leaked into contract: %s", forbidden, value)
			}
		}
	}
}

func TestHistoricalAssistantOutputCannotMasqueradeAsUserSource(t *testing.T) {
	got := HistoricalAssistantOutput("possible owner: Alex")
	if !strings.Contains(got, historicalAssistantMarker) ||
		!strings.Contains(got, `authority="model_output_not_evidence"`) ||
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
