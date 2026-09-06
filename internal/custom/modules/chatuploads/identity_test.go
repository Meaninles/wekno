package chatuploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type duplicateOriginal struct {
	interfaces.KnowledgeService
	row   *types.Knowledge
	calls int
}

func (d *duplicateOriginal) CreateKnowledgeFromFile(context.Context, string, *multipart.FileHeader, map[string]string, *bool, string, []string, string, *types.KnowledgeProcessOverrides) (*types.Knowledge, error) {
	d.calls++
	copy := *d.row
	return &copy, &types.DuplicateKnowledgeError{}
}

func uploadFile(t *testing.T, name, content string) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	f, err := w.CreateFormFile("file", name)
	require.NoError(t, err)
	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	r := httptest.NewRequest("POST", "/", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	require.NoError(t, r.ParseMultipartForm(1024))
	t.Cleanup(func() { _ = r.MultipartForm.RemoveAll() })
	return r.MultipartForm.File["file"][0]
}

func TestRepairPostgresDuplicateUploadRecovery(t *testing.T) {
	db := testsupport.Postgres(t, &types.KnowledgeBase{}, &types.Knowledge{}, &types.Session{})
	session := &types.Session{ID: uuid.NewString(), TenantID: 7, UserID: "owner"}
	require.NoError(t, db.Create(session).Error)
	kb := &types.KnowledgeBase{ID: privateKBID(session.ID), TenantID: 7, Name: "private", IsTemporary: true, ChatSessionID: session.ID, ChatOwnerID: session.UserID}
	require.NoError(t, db.Create(kb).Error)
	first, second := uuid.NewString(), uuid.NewString()
	digest := sha256.Sum256([]byte("same file"))
	metadata, err := json.Marshal(map[string]string{"chat_upload_id": first, "chat_upload_sha256": hex.EncodeToString(digest[:]), "ingestion": "preserve"})
	require.NoError(t, err)
	row := &types.Knowledge{ID: uuid.NewString(), TenantID: 7, KnowledgeBaseID: kb.ID, FileName: "original.txt", FileSize: 9, Metadata: metadata, ParseStatus: types.ParseStatusProcessing}
	require.NoError(t, db.Create(row).Error)
	creator := &duplicateOriginal{row: row}
	s := NewService(db, sessionStore{db: db}, creator, nil, nil)
	ctx := actor(7, "owner")
	accepted, err := s.Upload(ctx, session.ID, second, uploadFile(t, "renamed.txt", "same file"), nil)
	require.NoError(t, err)
	require.Equal(t, row.ID, accepted.ID)
	require.ElementsMatch(t, []string{first, second}, uploadIDs(accepted))
	require.Equal(t, "preserve", accepted.GetMetadata()["ingestion"])
	// Simulate losing that response and retrying from a fresh service instance.
	s = NewService(db, sessionStore{db: db}, creator, nil, nil)
	recovered, err := s.Upload(ctx, session.ID, second, uploadFile(t, "renamed.txt", "same file"), nil)
	require.NoError(t, err)
	require.Equal(t, accepted.ID, recovered.ID)
	require.Equal(t, 1, creator.calls, "accepted identities must not re-enter ingestion")
	for _, file := range []*multipart.FileHeader{uploadFile(t, "renamed.txt", "other txt"), uploadFile(t, "other.txt", "same file")} {
		_, err = s.Upload(ctx, session.ID, second, file, nil)
		require.ErrorContains(t, err, "bound to another file")
	}
	_, err = s.Upload(actor(7, "another-user"), session.ID, second, uploadFile(t, "renamed.txt", "same file"), nil)
	require.Error(t, err)
	_, err = s.Upload(actor(8, "owner"), session.ID, second, uploadFile(t, "renamed.txt", "same file"), nil)
	require.Error(t, err)
	// Metadata written after our source snapshot cannot be overwritten by binding.
	require.NoError(t, db.Model(row).UpdateColumn("metadata", gorm.Expr("metadata::jsonb || ?::jsonb", `{"new_ingestion":"keep"}`)).Error)
	third := uuid.NewString()
	_, err = s.Upload(ctx, session.ID, third, uploadFile(t, "third.txt", "same file"), nil)
	require.NoError(t, err)
	listed, err := s.List(ctx, session.ID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.ElementsMatch(t, []string{first, second, third}, uploadIDs(listed[0]))
	require.Equal(t, "keep", listed[0].GetMetadata()["new_ingestion"])
}
