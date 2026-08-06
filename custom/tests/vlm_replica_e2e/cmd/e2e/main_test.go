package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyWorkflowSpanFailuresUsesLatestRetryResult(t *testing.T) {
	rows := []workflowSpanRow{
		{ID: 1, KnowledgeID: "recovered", Attempt: 1, Name: "postprocess.summary", Status: "failed"},
		{ID: 2, KnowledgeID: "recovered", Attempt: 1, Name: "postprocess.summary", Status: "done"},
		{ID: 3, KnowledgeID: "recovered", Attempt: 1, Name: "embedding", Status: "done"},

		{ID: 10, KnowledgeID: "latest-failed", Attempt: 1, Name: "docreader", Status: "failed"},
		{ID: 11, KnowledgeID: "latest-failed", Attempt: 2, Name: "docreader", Status: "done"},
		{ID: 12, KnowledgeID: "latest-failed", Attempt: 2, Name: "postprocess.summary", Status: "failed"},

		// Input order is not authoritative; ID is.
		{ID: 22, KnowledgeID: "out-of-order", Attempt: 3, Name: "multimodal", Status: "done"},
		{ID: 21, KnowledgeID: "out-of-order", Attempt: 3, Name: "multimodal", Status: "failed"},
	}

	historical, latest := classifyWorkflowSpanFailures(rows)

	require.Equal(t, []string{"postprocess.summary"}, historical["recovered"])
	require.Empty(t, latest["recovered"])

	require.Equal(t, []string{"docreader", "postprocess.summary"}, historical["latest-failed"])
	require.Equal(t, []string{"postprocess.summary"}, latest["latest-failed"])

	require.Equal(t, []string{"multimodal"}, historical["out-of-order"])
	require.Empty(t, latest["out-of-order"])
}
