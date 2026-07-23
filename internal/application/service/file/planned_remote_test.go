package file

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentyun/cos-go-sdk-v5"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
)

func requirePlannedKeyShape(t *testing.T, key, prefix string, tenant uint64, owner, ext string) {
	t.Helper()
	pattern := "^"
	if prefix != "" {
		pattern += regexp.QuoteMeta(prefix) + "/"
	}
	pattern += regexp.QuoteMeta(strconv.FormatUint(tenant, 10))
	pattern += "/" + regexp.QuoteMeta(owner) + `/[0-9a-f-]{36}` + regexp.QuoteMeta(ext) + "$"
	assert.Regexp(t, pattern, key)
}

func TestCOSPlannedPathContract(t *testing.T) {
	mainClient := new(cos.Client)
	tempClient := new(cos.Client)
	svc := &cosFileService{
		client: mainClient, bucketName: "main-bucket", region: "ap-shanghai", cosPathPrefix: "root/files",
		tempClient: tempClient, tempBucketName: "temp-bucket", tempRegion: "ap-beijing",
	}

	filePath, err := svc.ReserveFilePath(7, "knowledge-id", "report.pdf")
	require.NoError(t, err)
	key, err := plannedfile.ParseRegionPath(filePath, cosProvider, "main-bucket", "ap-shanghai", "root/files")
	require.NoError(t, err)
	requirePlannedKeyShape(t, key, "root/files", 7, "knowledge-id", ".pdf")

	tempPath, err := svc.ReserveBytesPath(7, "export.csv", true)
	require.NoError(t, err)
	tempKey, err := plannedfile.ParseRegionPath(tempPath, cosProvider, "temp-bucket", "ap-beijing", "exports")
	require.NoError(t, err)
	assert.Regexp(t, `^exports/7/[0-9a-f-]{36}\.csv$`, tempKey)
	client, parsedKey, err := svc.plannedBytesTarget(tempPath)
	require.NoError(t, err)
	assert.Same(t, tempClient, client)
	assert.Equal(t, tempKey, parsedKey)
	resolvedClient, resolvedKey, err := svc.resolveCosObject(tempPath)
	require.NoError(t, err)
	assert.Same(t, tempClient, resolvedClient)
	assert.Equal(t, tempKey, resolvedKey)

	copyPath, err := svc.ReserveCopyPath("cos://main-bucket/ap-shanghai/source/a.docx", 7, "copy-owner")
	require.NoError(t, err)
	copyKey, err := plannedfile.ParseRegionPath(copyPath, cosProvider, "main-bucket", "ap-shanghai", "root/files")
	require.NoError(t, err)
	requirePlannedKeyShape(t, copyKey, "root/files", 7, "copy-owner", ".docx")

	for _, badSource := range []string{
		"s3://main-bucket/source/a.docx",
		"cos://other-bucket/ap-shanghai/source/a.docx",
		"cos://main-bucket/ap-guangzhou/source/a.docx",
		"cos://main-bucket/ap-shanghai/source/../a.docx",
	} {
		_, err := svc.ReserveCopyPath(badSource, 7, "copy-owner")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCrossBackendCopy)
	}
	_, err = svc.plannedMainKey("cos://other-bucket/ap-shanghai/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.plannedMainKey("tos://main-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	for _, badDestination := range []string{
		"tos://main-bucket/root/files/7/exports/a.csv",
		"cos://other-bucket/ap-shanghai/root/files/7/exports/a.csv",
		"cos://main-bucket/ap-shanghai/root/files/../escape.csv",
	} {
		require.Error(t, svc.CommitBytesAtPath(context.Background(), nil, badDestination))
	}
	require.Error(t, svc.DeleteFile(context.Background(), "cos://other-bucket/ap-shanghai/root/files/a.csv"))
}

func TestTOSPlannedPathContract(t *testing.T) {
	mainClient := new(tos.ClientV2)
	tempClient := new(tos.ClientV2)
	svc := &tosFileService{
		client: mainClient, tempClient: tempClient, pathPrefix: "root/files",
		bucketName: "main-bucket", tempBucketName: "temp-bucket",
	}

	filePath, err := svc.ReserveFilePath(7, "knowledge-id", "report.pdf")
	require.NoError(t, err)
	key, err := svc.plannedMainKey(filePath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, key, "root/files", 7, "knowledge-id", ".pdf")

	tempPath, err := svc.ReserveBytesPath(7, "export.csv", true)
	require.NoError(t, err)
	client, bucket, tempKey, err := svc.plannedBytesTarget(tempPath)
	require.NoError(t, err)
	assert.Same(t, tempClient, client)
	assert.Equal(t, "temp-bucket", bucket)
	assert.Regexp(t, `^exports/7/[0-9a-f-]{36}\.csv$`, tempKey)

	copyPath, err := svc.ReserveCopyPath("tos://main-bucket/source/a.docx", 7, "copy-owner")
	require.NoError(t, err)
	copyKey, err := svc.plannedMainKey(copyPath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, copyKey, "root/files", 7, "copy-owner", ".docx")

	for _, badSource := range []string{
		"s3://main-bucket/source/a.docx",
		"tos://other-bucket/source/a.docx",
		"tos://main-bucket/source/../a.docx",
	} {
		_, err := svc.ReserveCopyPath(badSource, 7, "copy-owner")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCrossBackendCopy)
	}
	_, err = svc.plannedMainKey("tos://other-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.plannedMainKey("oss://main-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.clientForBoundBucket("unknown-bucket")
	require.Error(t, err)
	require.Error(t, svc.DeleteFile(context.Background(), "tos://unknown-bucket/root/files/a.csv"))
	for _, badDestination := range []string{
		"oss://main-bucket/root/files/7/exports/a.csv",
		"tos://other-bucket/root/files/7/exports/a.csv",
		"tos://main-bucket/root/files/../escape.csv",
	} {
		require.Error(t, svc.CommitBytesAtPath(context.Background(), nil, badDestination))
	}
}

func TestOSSPlannedPathContract(t *testing.T) {
	mainClient := new(oss.Client)
	tempClient := new(oss.Client)
	svc := &ossFileService{
		client: mainClient, tempClient: tempClient, pathPrefix: "root/files/",
		bucketName: "main-bucket", tempBucketName: "temp-bucket",
	}

	filePath, err := svc.ReserveFilePath(7, "knowledge-id", "report.pdf")
	require.NoError(t, err)
	key, err := svc.plannedMainKey(filePath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, key, "root/files", 7, "knowledge-id", ".pdf")

	tempPath, err := svc.ReserveBytesPath(7, "export.csv", true)
	require.NoError(t, err)
	client, bucket, tempKey, err := svc.plannedBytesTarget(tempPath)
	require.NoError(t, err)
	assert.Same(t, tempClient, client)
	assert.Equal(t, "temp-bucket", bucket)
	assert.Regexp(t, `^exports/7/[0-9a-f-]{36}\.csv$`, tempKey)

	copyPath, err := svc.ReserveCopyPath("oss://main-bucket/source/a.docx", 7, "copy-owner")
	require.NoError(t, err)
	copyKey, err := svc.plannedMainKey(copyPath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, copyKey, "root/files", 7, "copy-owner", ".docx")

	for _, badSource := range []string{
		"s3://main-bucket/source/a.docx",
		"oss://other-bucket/source/a.docx",
		"oss://main-bucket/source/../a.docx",
	} {
		_, err := svc.ReserveCopyPath(badSource, 7, "copy-owner")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCrossBackendCopy)
	}
	_, err = svc.plannedMainKey("oss://other-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.plannedMainKey("tos://main-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.clientForBoundBucket("unknown-bucket")
	require.Error(t, err)
	require.Error(t, svc.DeleteFile(context.Background(), "oss://unknown-bucket/root/files/a.csv"))
	for _, badDestination := range []string{
		"tos://main-bucket/root/files/7/exports/a.csv",
		"oss://other-bucket/root/files/7/exports/a.csv",
		"oss://main-bucket/root/files/../escape.csv",
	} {
		require.Error(t, svc.CommitBytesAtPath(context.Background(), nil, badDestination))
	}
}

func TestKS3PlannedPathContract(t *testing.T) {
	svc := &ks3FileService{bucketName: "main-bucket", pathPrefix: "root/files"}

	filePath, err := svc.ReserveFilePath(7, "knowledge-id", "report.pdf")
	require.NoError(t, err)
	key, err := svc.plannedMainKey(filePath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, key, "root/files", 7, "knowledge-id", ".pdf")

	bytesPath, err := svc.ReserveBytesPath(7, "export.csv", true)
	require.NoError(t, err)
	bytesKey, err := svc.plannedMainKey(bytesPath)
	require.NoError(t, err)
	assert.Regexp(t, `^root/files/7/exports/[0-9a-f-]{36}\.csv$`, bytesKey)

	copyPath, err := svc.ReserveCopyPath("ks3://main-bucket/source/a.docx", 7, "copy-owner")
	require.NoError(t, err)
	copyKey, err := svc.plannedMainKey(copyPath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, copyKey, "root/files", 7, "copy-owner", ".docx")

	for _, badSource := range []string{
		"s3://main-bucket/source/a.docx",
		"ks3://other-bucket/source/a.docx",
		"ks3://main-bucket/source/../a.docx",
	} {
		_, err := svc.ReserveCopyPath(badSource, 7, "copy-owner")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCrossBackendCopy)
	}
	_, err = svc.plannedMainKey("ks3://other-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.plannedMainKey("oss://main-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	require.Error(t, svc.DeleteFile(context.Background(), "ks3://unknown-bucket/root/files/a.csv"))
	for _, badDestination := range []string{
		"oss://main-bucket/root/files/7/exports/a.csv",
		"ks3://other-bucket/root/files/7/exports/a.csv",
		"ks3://main-bucket/root/files/../escape.csv",
	} {
		require.Error(t, svc.CommitBytesAtPath(context.Background(), nil, badDestination))
	}
}

func TestOBSPlannedPathContractAndProxyCompatibility(t *testing.T) {
	svc := &obsFileService{
		bucketName: "main-bucket", pathPrefix: "root/files", proxyDomain: "https://files.example.test",
	}

	filePath, err := svc.ReserveFilePath(7, "knowledge-id", "report.pdf")
	require.NoError(t, err)
	key, err := svc.plannedMainKey(filePath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, key, "root/files", 7, "knowledge-id", ".pdf")
	legacyPath, err := svc.legacyOBSPath(filePath)
	require.NoError(t, err)
	assert.Equal(t, "https://files.example.test/"+key, legacyPath)
	parsedLegacyKey, err := svc.parseObsFilePath(legacyPath)
	require.NoError(t, err)
	assert.Equal(t, key, parsedLegacyKey)
	parsedCanonicalKey, err := svc.parseObsFilePath(filePath)
	require.NoError(t, err)
	assert.Equal(t, key, parsedCanonicalKey)

	bytesPath, err := svc.ReserveBytesPath(7, "export.csv", false)
	require.NoError(t, err)
	bytesKey, err := svc.plannedMainKey(bytesPath)
	require.NoError(t, err)
	assert.Regexp(t, `^root/files/7/[0-9a-f-]{36}\.csv$`, bytesKey)
	tempPath, err := svc.ReserveBytesPath(7, "export.csv", true)
	require.NoError(t, err)
	tempKey, err := svc.plannedMainKey(tempPath)
	require.NoError(t, err)
	assert.Regexp(t, `^root/files/temp/7/[0-9a-f-]{36}\.csv$`, tempKey)

	copyPath, err := svc.ReserveCopyPath("obs://main-bucket/source/a.docx", 7, "copy-owner")
	require.NoError(t, err)
	copyKey, err := svc.plannedMainKey(copyPath)
	require.NoError(t, err)
	requirePlannedKeyShape(t, copyKey, "root/files", 7, "copy-owner", ".docx")

	for _, badSource := range []string{
		"s3://main-bucket/source/a.docx",
		"obs://other-bucket/source/a.docx",
		"obs://main-bucket/source/../a.docx",
	} {
		_, err := svc.ReserveCopyPath(badSource, 7, "copy-owner")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCrossBackendCopy)
	}
	_, err = svc.plannedMainKey("obs://other-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	_, err = svc.plannedMainKey("oss://main-bucket/root/files/7/k/a.pdf")
	require.Error(t, err)
	require.Error(t, svc.DeleteFile(context.Background(), "obs://unknown-bucket/root/files/a.csv"))
	for _, badDestination := range []string{
		"oss://main-bucket/root/files/7/a.csv",
		"obs://other-bucket/root/files/7/a.csv",
		"obs://main-bucket/root/files/../escape.csv",
	} {
		require.Error(t, svc.CommitBytesAtPath(context.Background(), nil, badDestination))
	}
}

type capturedHTTPRequest struct {
	method string
	path   string
	body   []byte
}

type captureHTTPClient struct {
	mu       sync.Mutex
	requests []capturedHTTPRequest
}

func (c *captureHTTPClient) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, capturedHTTPRequest{method: req.Method, path: req.URL.Path, body: body})
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    req,
	}, nil
}

func TestOBSCommitBytesUsesExactReservedKeyAndIsRetrySafe(t *testing.T) {
	capture := new(captureHTTPClient)
	client := awss3.New(awss3.Options{
		Region:           "cn-test-1",
		EndpointResolver: &obsEndpointResolver{url: "https://obs.example.test"},
		Credentials:      credentials.NewStaticCredentialsProvider("access", "secret", ""),
		UsePathStyle:     true,
		HTTPClient:       capture,
	})
	svc := &obsFileService{client: client, bucketName: "main-bucket", pathPrefix: "root/files"}
	filePath, err := svc.ReserveBytesPath(7, "payload.bin", false)
	require.NoError(t, err)
	key, err := svc.plannedMainKey(filePath)
	require.NoError(t, err)

	payload := []byte{0, 1, 2, 3, 255}
	require.NoError(t, svc.CommitBytesAtPath(context.Background(), payload, filePath))
	require.NoError(t, svc.CommitBytesAtPath(context.Background(), payload, filePath))

	capture.mu.Lock()
	requests := append([]capturedHTTPRequest(nil), capture.requests...)
	capture.mu.Unlock()
	require.Len(t, requests, 2)
	for _, request := range requests {
		assert.Equal(t, http.MethodPut, request.method)
		assert.Equal(t, "/main-bucket/"+key, request.path)
		assert.Equal(t, payload, request.body)
	}
}

func TestRemoteProvidersImplementPlannedFileService(t *testing.T) {
	var services = []interfaces.PlannedFileService{
		(*cosFileService)(nil),
		(*tosFileService)(nil),
		(*ossFileService)(nil),
		(*ks3FileService)(nil),
		(*obsFileService)(nil),
	}
	assert.Len(t, services, 5)
}
