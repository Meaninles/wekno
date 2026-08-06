package questioncoverage

import "testing"

func TestAssessIsConservative(t *testing.T) {
	tests := []struct {
		content  string
		eligible bool
	}{
		{"", false},
		{"空白表单。", false},
		{"| | |\n|---|---|\n| | |", false},
		{"| A | 1 |\n|---|---|", true},
		{"审批由经理负责。", true},
		{"第六条", true},
		{"办理时限不得超过五个工作日。", true},
	}
	for _, tc := range tests {
		if got := Assess(tc.content); got.Eligible != tc.eligible {
			t.Fatalf("Assess(%q) = %+v, want eligible=%t", tc.content, got, tc.eligible)
		}
	}
}
