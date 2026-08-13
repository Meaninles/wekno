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

func TestExtractImageURLsAndOCRTextPrefersValidatedRequestData(t *testing.T) {
	urls, text := extractImageURLsAndOCRText([]ImageAttachment{
		{
			Data:    "data:image/jpeg;base64,very-large-payload",
			URL:     "minio://private-bucket/10002/exports/image.jpg",
			Caption: "图片内容描述",
		},
	})

	assert.Equal(t, []string{"data:image/jpeg;base64,very-large-payload"}, urls)
	assert.Equal(t, "图片内容描述", text)
}

func TestExtractImageURLsAndOCRTextFallsBackToStoredURLWhenDataMissing(t *testing.T) {
	urls, text := extractImageURLsAndOCRText([]ImageAttachment{
		{
			URL:     "local://10002/exports/image.png",
			Caption: "inline caption",
		},
	})

	assert.Equal(t, []string{"local://10002/exports/image.png"}, urls)
	assert.Equal(t, "inline caption", text)
}

func TestImageModelInputDoesNotChangePersistedMessageImage(t *testing.T) {
	images := []ImageAttachment{
		{
			Data:    "data:image/png;base64,inline",
			URL:     "minio://private-bucket/10002/exports/image.png",
			Caption: "image caption",
		},
	}

	urls, caption := extractImageURLsAndOCRText(images)
	persisted := convertImageAttachments(images)

	assert.Equal(t, []string{"data:image/png;base64,inline"}, urls)
	assert.Equal(t, "image caption", caption)
	assert.Equal(t, types.MessageImages{{
		URL:     "minio://private-bucket/10002/exports/image.png",
		Caption: "image caption",
	}}, persisted)
}

func TestBuildQARequestWithoutImagesIsUnchanged(t *testing.T) {
	req := (&qaRequestContext{
		query:            "plain text",
		session:          &types.Session{},
		assistantMessage: &types.Message{},
	}).buildQARequest()

	assert.Equal(t, "plain text", req.Query)
	assert.Nil(t, req.ImageURLs)
	assert.Empty(t, req.ImageDescription)
}
