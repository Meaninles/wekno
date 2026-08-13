package session

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/stretchr/testify/require"
)

type strictImageFileService struct {
	fileNames []string
}

func (s *strictImageFileService) CheckConnectivity(context.Context) error { return nil }

func (s *strictImageFileService) SaveFile(
	context.Context,
	*multipart.FileHeader,
	uint64,
	string,
) (string, error) {
	return "", fmt.Errorf("unexpected SaveFile call")
}

func (s *strictImageFileService) SaveBytes(
	_ context.Context,
	_ []byte,
	_ uint64,
	fileName string,
	_ bool,
) (string, error) {
	if err := plannedfile.ValidateSegment("file name", fileName); err != nil {
		return "", err
	}
	s.fileNames = append(s.fileNames, fileName)
	return "minio://test/" + fileName, nil
}

func (s *strictImageFileService) GetFile(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("unexpected GetFile call")
}

func (s *strictImageFileService) GetFileURL(context.Context, string) (string, error) {
	return "", fmt.Errorf("unexpected GetFileURL call")
}

func (s *strictImageFileService) DeleteFile(context.Context, string) error {
	return fmt.Errorf("unexpected DeleteFile call")
}

func (s *strictImageFileService) CopyFile(
	context.Context,
	string,
	uint64,
	string,
) (string, error) {
	return "", fmt.Errorf("unexpected CopyFile call")
}

func TestSaveImageAttachmentsUsesPlannedFileCompatibleNames(t *testing.T) {
	fileService := &strictImageFileService{}
	images := []ImageAttachment{
		{Data: "data:image/png;base64,iVBORw0KGgo="},
		{Data: "data:image/jpeg;base64,/9j/2Q=="},
	}

	err := SaveImageAttachments(context.Background(), fileService, images, 42, "")

	require.NoError(t, err)
	require.Len(t, fileService.fileNames, 2)
	require.Len(t, images, 2)
	for i, fileName := range fileService.fileNames {
		require.NotContains(t, fileName, "/")
		require.NotContains(t, fileName, `\`)
		require.NotEmpty(t, images[i].URL)
		require.True(t, strings.HasPrefix(images[i].URL, "minio://test/"))
	}
	require.True(t, strings.HasSuffix(fileService.fileNames[0], ".png"))
	require.True(t, strings.HasSuffix(fileService.fileNames[1], ".jpg"))
}
