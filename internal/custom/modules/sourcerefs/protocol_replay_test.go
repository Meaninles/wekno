package sourcerefs

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// Optional replay of a captured answer and its exact registered handles. This
// checks protocol preservation, never whether the answer's claims are correct.
func TestCitationProtocolReplay(t *testing.T) {
	path := os.Getenv("WEKNORA_CITATION_REPLAY")
	if path == "" {
		t.Skip("no captured citation protocol input")
	}
	var input struct {
		Answer string                `json:"answer"`
		Refs   []*types.SearchResult `json:"refs"`
	}
	encoded, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &input))
	answer, refs, report := FilterAnswerCitations(input.Answer, input.Refs)
	strip := func(value string) string {
		return transformOutsideMarkdownCode(value, func(part string) string {
			part = groupedCitationAliasRE.ReplaceAllString(part, "")
			return canonicalSourceTagRE.ReplaceAllString(part, "")
		})
	}
	require.Equal(t, strip(input.Answer), strip(answer), "citation projection changed prose")
	output, err := json.MarshalIndent(map[string]any{"answer": answer, "references": refs, "report": report}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".result.json", output, 0600))
	t.Logf("registered=%d cited=%d unknown=%v", report.AvailableCount, len(refs), report.UnknownIDs)
}
