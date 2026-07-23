package plannedfile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBucketPathRoundTripAndStrictBinding(t *testing.T) {
	filePath, err := FormatBucketPath("s3", "bucket-a", "root/7/knowledge-1/object.pdf")
	require.NoError(t, err)
	require.Equal(t, "s3://bucket-a/root/7/knowledge-1/object.pdf", filePath)

	key, err := ParseBucketPath(filePath, "s3", "bucket-a", "root")
	require.NoError(t, err)
	require.Equal(t, "root/7/knowledge-1/object.pdf", key)

	for _, invalid := range []struct {
		path   string
		scheme string
		bucket string
		prefix string
	}{
		{filePath, "minio", "bucket-a", "root"},
		{filePath, "s3", "bucket-b", "root"},
		{filePath, "s3", "bucket-a", "other"},
		{"s3://bucket-a/root/../escape", "s3", "bucket-a", "root"},
		{"s3://bucket-a/root/%2F/escape", "s3", "", "root"},
	} {
		_, err := ParseBucketPath(invalid.path, invalid.scheme, invalid.bucket, invalid.prefix)
		require.Error(t, err)
	}
}

func TestRegionPathRoundTripAndStrictBinding(t *testing.T) {
	filePath, err := FormatRegionPath("cos", "bucket-a", "ap-shanghai", "root/7/object.pdf")
	require.NoError(t, err)
	key, err := ParseRegionPath(filePath, "cos", "bucket-a", "ap-shanghai", "root")
	require.NoError(t, err)
	require.Equal(t, "root/7/object.pdf", key)

	_, err = ParseRegionPath(filePath, "cos", "bucket-a", "ap-beijing", "root")
	require.Error(t, err)
	_, err = ParseRegionPath(filePath, "cos", "bucket-a", "../region", "root")
	require.Error(t, err)
}

func TestReservationKeyValidationHasNoIOAndRejectsHierarchyInput(t *testing.T) {
	key, err := FileKey("root/", 7, "knowledge-1", "report.pdf")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, "root/7/knowledge-1/"), key)
	require.True(t, strings.HasSuffix(key, ".pdf"), key)

	for _, tc := range []struct {
		prefix      string
		knowledgeID string
		fileName    string
	}{
		{"../escape", "knowledge-1", "report.pdf"},
		{"root", "../knowledge", "report.pdf"},
		{"root", "knowledge-1", "../report.pdf"},
		{"root", "knowledge-1", "report..pdf"},
	} {
		_, err := FileKey(tc.prefix, 7, tc.knowledgeID, tc.fileName)
		require.Error(t, err)
	}
	_, err = FileKey("root", 0, "knowledge-1", "report.pdf")
	require.Error(t, err)
}
