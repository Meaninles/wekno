package file

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func multipartHeader(t *testing.T, fileName string, data []byte) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", fileName)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	reader := multipart.NewReader(&body, writer.Boundary())
	form, err := reader.ReadForm(int64(len(data) + 4096))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, form.RemoveAll()) })
	require.Len(t, form.File["file"], 1)
	return form.File["file"][0]
}

func readStored(t *testing.T, svc interfaces.FileService, filePath string) []byte {
	t.Helper()
	r, err := svc.GetFile(context.Background(), filePath)
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

func TestPlannedLocalReserveCommitAndCopyAreIdempotent(t *testing.T) {
	baseDir := t.TempDir()
	legacy := NewLocalFileService(baseDir, "")
	planned, ok := legacy.(interfaces.PlannedFileService)
	require.True(t, ok)

	filePath, err := planned.ReserveFilePath(7, "knowledge-1", "report.pdf")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(filePath, "local://7/knowledge-1/"))
	entries, err := os.ReadDir(baseDir)
	require.NoError(t, err)
	require.Empty(t, entries, "Reserve must not create a directory or object")

	header := multipartHeader(t, "report.pdf", []byte("complete-upload"))
	require.NoError(t, planned.CommitFileAtPath(context.Background(), header, filePath))
	require.NoError(t, planned.CommitFileAtPath(context.Background(), header, filePath))
	require.Equal(t, []byte("complete-upload"), readStored(t, legacy, filePath))

	bytesPath, err := planned.ReserveBytesPath(7, "export.json", false)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(bytesPath, "local://7/exports/"))
	require.NoError(t, planned.CommitBytesAtPath(context.Background(), []byte("v1"), bytesPath))
	require.NoError(t, planned.CommitBytesAtPath(context.Background(), []byte("v2"), bytesPath))
	require.Equal(t, []byte("v2"), readStored(t, legacy, bytesPath))

	copyPath, err := planned.ReserveCopyPath(filePath, 7, "knowledge-2")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(copyPath, "local://7/knowledge-2/"))
	require.NoError(t, planned.CommitCopyAtPath(context.Background(), filePath, copyPath))
	require.NoError(t, planned.CommitCopyAtPath(context.Background(), filePath, copyPath))
	require.NoError(t, legacy.DeleteFile(context.Background(), filePath))
	require.Equal(t, []byte("complete-upload"), readStored(t, legacy, copyPath))
}

func TestPlannedLocalConcurrentCommitNeverExposesMixedFile(t *testing.T) {
	legacy := NewLocalFileService(t.TempDir(), "")
	planned := legacy.(interfaces.PlannedFileService)
	filePath, err := planned.ReserveBytesPath(7, "race.bin", false)
	require.NoError(t, err)
	a := bytes.Repeat([]byte{'a'}, 128*1024)
	b := bytes.Repeat([]byte{'b'}, 128*1024)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, data := range [][]byte{a, b} {
		data := data
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- planned.CommitBytesAtPath(context.Background(), data, filePath)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	got := readStored(t, legacy, filePath)
	require.True(t, bytes.Equal(got, a) || bytes.Equal(got, b), "atomic replacement exposed mixed content")
}

func TestPlannedLocalRejectsTraversalAndCrossProvider(t *testing.T) {
	planned := NewLocalFileService(t.TempDir(), "").(interfaces.PlannedFileService)
	_, err := planned.ReserveFilePath(7, "../knowledge", "report.pdf")
	require.Error(t, err)
	_, err = planned.ReserveFilePath(7, "knowledge", "../report.pdf")
	require.Error(t, err)
	_, err = planned.ReserveCopyPath("s3://bucket/key.pdf", 7, "knowledge")
	require.ErrorIs(t, err, ErrCrossBackendCopy)
	require.Error(t, planned.CommitBytesAtPath(
		context.Background(), []byte("escape"), "local://../../outside.txt",
	))
}

func TestPlannedDummyCommitAndCopyAreIdempotent(t *testing.T) {
	legacy := NewDummyFileService()
	planned := legacy.(interfaces.PlannedFileService)
	srcPath, err := planned.ReserveBytesPath(9, "source.txt", false)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(srcPath, "dummy://9/exports/"))
	require.NoError(t, planned.CommitBytesAtPath(context.Background(), []byte("dummy-data"), srcPath))
	require.NoError(t, planned.CommitBytesAtPath(context.Background(), []byte("dummy-data"), srcPath))

	dstPath, err := planned.ReserveCopyPath(srcPath, 9, "knowledge-2")
	require.NoError(t, err)
	require.NoError(t, planned.CommitCopyAtPath(context.Background(), srcPath, dstPath))
	require.NoError(t, planned.CommitCopyAtPath(context.Background(), srcPath, dstPath))
	require.Equal(t, []byte("dummy-data"), readStored(t, legacy, dstPath))
	require.NoError(t, legacy.DeleteFile(context.Background(), srcPath))
	require.NoError(t, legacy.DeleteFile(context.Background(), srcPath))
}

func TestPlannedDummyRejectsTraversalAndCrossProvider(t *testing.T) {
	planned := NewDummyFileService().(interfaces.PlannedFileService)
	_, err := planned.ReserveFilePath(1, "../knowledge", "a.txt")
	require.Error(t, err)
	_, err = planned.ReserveBytesPath(1, "../../a.txt", false)
	require.Error(t, err)
	_, err = planned.ReserveCopyPath("local://1/a.txt", 1, "knowledge")
	require.ErrorIs(t, err, ErrCrossBackendCopy)
	require.Error(t, planned.CommitBytesAtPath(context.Background(), nil, "dummy://../../escape"))
}

func TestPlannedMinioAndS3OfflinePathContracts(t *testing.T) {
	tests := []struct {
		name       string
		service    interfaces.PlannedFileService
		filePrefix string
		bytePrefix string
		tempPrefix string
		wrongPath  string
		outsideNS  string
	}{
		{
			name: "minio", service: &minioFileService{bucketName: "bucket-a", pathPrefix: "root/"},
			filePrefix: "minio://bucket-a/root/7/knowledge-1/",
			bytePrefix: "minio://bucket-a/root/7/exports/",
			tempPrefix: "minio://bucket-a/root/temp/7/",
			wrongPath:  "minio://bucket-b/root/7/exports/evil.txt",
			outsideNS:  "minio://bucket-a/other/7/exports/evil.txt",
		},
		{
			name: "s3", service: &s3FileService{bucketName: "bucket-a", pathPrefix: "root/"},
			filePrefix: "s3://bucket-a/root/7/knowledge-1/",
			bytePrefix: "s3://bucket-a/root/7/exports/",
			tempPrefix: "s3://bucket-a/root/7/exports/",
			wrongPath:  "s3://bucket-b/root/7/exports/evil.txt",
			outsideNS:  "s3://bucket-a/other/7/exports/evil.txt",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filePath, err := tc.service.ReserveFilePath(7, "knowledge-1", "report.pdf")
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(filePath, tc.filePrefix), filePath)
			bytesPath, err := tc.service.ReserveBytesPath(7, "export.json", false)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(bytesPath, tc.bytePrefix), bytesPath)
			tempPath, err := tc.service.ReserveBytesPath(7, "export.json", true)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(tempPath, tc.tempPrefix), tempPath)
			copyPath, err := tc.service.ReserveCopyPath(filePath, 7, "knowledge-2")
			require.NoError(t, err)
			assert.Contains(t, copyPath, "/7/knowledge-2/")

			_, err = tc.service.ReserveFilePath(7, "../knowledge", "report.pdf")
			require.Error(t, err)
			_, err = tc.service.ReserveBytesPath(7, "../../report.pdf", false)
			require.Error(t, err)
			_, err = tc.service.ReserveCopyPath("local://7/source.pdf", 7, "knowledge-2")
			require.ErrorIs(t, err, ErrCrossBackendCopy)
			require.Error(t, tc.service.CommitBytesAtPath(context.Background(), nil, tc.wrongPath))
			require.Error(t, tc.service.CommitBytesAtPath(context.Background(), nil, tc.outsideNS))
		})
	}
}

func TestOriginalInputSameNameConcurrentReservationsAreUnique(t *testing.T) {
	const (
		reservations = 512
		tenantID     = uint64(10000)
		fileName     = "replica_document_input.txt"
		prefix       = "weknora/__weknora_claude_sdk_original_inputs_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/"
	)
	services := []struct {
		name       string
		service    interfaces.PlannedFileService
		pathPrefix string
	}{
		{
			name:       "minio",
			service:    &minioFileService{bucketName: "private", pathPrefix: prefix},
			pathPrefix: "minio://private/" + prefix + "temp/10000/",
		},
		{
			name:       "obs",
			service:    &obsFileService{bucketName: "private", pathPrefix: strings.TrimSuffix(prefix, "/")},
			pathPrefix: "obs://private/" + prefix + "temp/10000/",
		},
	}

	for _, tc := range services {
		t.Run(tc.name, func(t *testing.T) {
			results := make(chan string, reservations)
			errors := make(chan error, reservations)
			var wg sync.WaitGroup
			for range reservations {
				wg.Add(1)
				go func() {
					defer wg.Done()
					reserved, err := tc.service.ReserveBytesPath(tenantID, fileName, true)
					if err != nil {
						errors <- err
						return
					}
					results <- reserved
				}()
			}
			wg.Wait()
			close(results)
			close(errors)

			for err := range errors {
				require.NoError(t, err)
			}
			seen := make(map[string]struct{}, reservations)
			for reserved := range results {
				require.True(t, strings.HasPrefix(reserved, tc.pathPrefix), reserved)
				require.NotContains(t, reserved, "replica_document_input")
				objectID, ext := strings.TrimSuffix(path.Base(reserved), path.Ext(reserved)), path.Ext(reserved)
				require.Equal(t, ".txt", ext)
				parsed, err := uuid.Parse(objectID)
				require.NoError(t, err)
				require.NotEqual(t, uuid.Nil, parsed)
				_, duplicate := seen[reserved]
				require.False(t, duplicate, "concurrent reservation returned duplicate object path %s", reserved)
				seen[reserved] = struct{}{}
			}
			require.Len(t, seen, reservations)
		})
	}
}

func TestPlannedRemoteReserveRejectsTraversalPrefixWithoutNetwork(t *testing.T) {
	services := []interfaces.PlannedFileService{
		&minioFileService{bucketName: "bucket-a", pathPrefix: "../escape/"},
		&s3FileService{bucketName: "bucket-a", pathPrefix: "../escape/"},
	}
	for _, service := range services {
		_, err := service.ReserveFilePath(7, "knowledge-1", "report.pdf")
		require.Error(t, err)
	}
}
