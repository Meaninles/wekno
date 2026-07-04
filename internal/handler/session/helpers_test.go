package session

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestTagScopesFromMentionedItems(t *testing.T) {
	scopes := tagScopesFromMentionedItems([]MentionedItemRequest{
		{Type: "tag", ID: "tag-1", KBID: "kb-1"},
		{Type: "tag", ID: "tag-2", KBID: "kb-1"},
		{Type: "tag", ID: "tag-3", KBID: "kb-2"},
		{Type: "tag", ID: "orphan", KBID: ""},
	})
	assert.Len(t, scopes, 2)
	byKB := make(map[string][]string)
	for _, scope := range scopes {
		byKB[scope.KnowledgeBaseID] = scope.TagIDs
	}
	assert.ElementsMatch(t, []string{"tag-1", "tag-2"}, byKB["kb-1"])
	assert.Equal(t, []string{"tag-3"}, byKB["kb-2"])
}

func TestMergeTagScopesFromRequestIDs_SingleKB(t *testing.T) {
	scopes := mergeTagScopesFromRequestIDs(
		[]types.TagScope{{KnowledgeBaseID: "kb-1", TagIDs: []string{"tag-1"}}},
		[]string{"tag-2"},
		[]string{"kb-1"},
	)
	assert.Len(t, scopes, 1)
	assert.ElementsMatch(t, []string{"tag-1", "tag-2"}, scopes[0].TagIDs)
}

func TestMergeTagScopesFromRequestIDs_OrphanWithSingleKB(t *testing.T) {
	scopes := mergeTagScopesFromRequestIDs(nil, []string{"tag-9"}, []string{"kb-1"})
	assert.Len(t, scopes, 1)
	assert.Equal(t, "kb-1", scopes[0].KnowledgeBaseID)
	assert.Equal(t, []string{"tag-9"}, scopes[0].TagIDs)
}

func TestMergeTagScopesFromRequestIDs_AmbiguousKBIgnored(t *testing.T) {
	scopes := mergeTagScopesFromRequestIDs(nil, []string{"tag-9"}, []string{"kb-1", "kb-2"})
	assert.Empty(t, scopes)
}

func TestExtractImageURLsAndOCRTextPrefersStoredURL(t *testing.T) {
	urls, text := extractImageURLsAndOCRText([]ImageAttachment{
		{
			Data:    "data:image/jpeg;base64,very-large-payload",
			URL:     "local://10002/chat-images/image.jpg",
			Caption: "图片内容描述",
		},
	})

	assert.Equal(t, []string{"local://10002/chat-images/image.jpg"}, urls)
	assert.Equal(t, "图片内容描述", text)
}

func TestExtractImageURLsAndOCRTextFallsBackToDataWhenURLMissing(t *testing.T) {
	urls, text := extractImageURLsAndOCRText([]ImageAttachment{
		{
			Data:    "data:image/png;base64,inline",
			Caption: "inline caption",
		},
	})

	assert.Equal(t, []string{"data:image/png;base64,inline"}, urls)
	assert.Equal(t, "inline caption", text)
}
