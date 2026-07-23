package service

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type splitArchiveTestEntry struct {
	name    string
	content []byte
	method  uint16
}

func splitArchiveTestManifest(t *testing.T, content []byte) documentsplit.Manifest {
	t.Helper()
	partDigest := sha256.Sum256(content)
	return documentsplit.Manifest{
		SchemaVersion: 1, PlannerVersion: "documentsplit-v1",
		Strategy: "pdf-pages", TargetRatio: 0.75,
		PartCount: 1, TotalPartBytes: int64(len(content)),
		Source: documentsplit.ManifestSource{
			FileName: "large.pdf", FileType: "pdf", SizeBytes: 123,
			SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Parts: []documentsplit.ManifestPart{{
			Index: 0, FileName: "part-000001.pdf", FileType: "pdf",
			SizeBytes: int64(len(content)),
			SHA256:    hex.EncodeToString(partDigest[:]),
			Locator:   json.RawMessage(`{"kind":"pages","page_start":1,"page_end":2}`),
			Metrics:   json.RawMessage(`{"pages":2}`),
		}},
	}
}

func writeSplitArchiveForTest(
	t *testing.T, manifest []byte, entries ...splitArchiveTestEntry,
) (*os.File, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "split.zip")
	file, err := os.Create(path)
	require.NoError(t, err)
	writer := zip.NewWriter(file)
	manifestHeader := &zip.FileHeader{Name: "manifest.json", Method: zip.Store}
	manifestWriter, err := writer.CreateHeader(manifestHeader)
	require.NoError(t, err)
	_, err = manifestWriter.Write(manifest)
	require.NoError(t, err)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method}
		target, createErr := writer.CreateHeader(header)
		require.NoError(t, createErr)
		_, writeErr := target.Write(entry.content)
		require.NoError(t, writeErr)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, file.Sync())
	info, err := file.Stat()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	return file, info.Size()
}

func validateSplitArchiveForTest(
	t *testing.T, file *os.File, size int64,
) (documentsplit.Manifest, map[int]*zip.File, error) {
	t.Helper()
	return validateSplitArchive(
		file, size,
		&types.Knowledge{FileSize: 123},
		&types.DocumentSplitResult{PlannerVersion: "documentsplit-v1", PartCount: 1},
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"large.pdf", "pdf", 100, 12,
	)
}

func TestValidateSplitArchiveAcceptsVerifiedStoredPart(t *testing.T) {
	content := []byte("synthetic-pdf-part")
	manifest := splitArchiveTestManifest(t, content)
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	file, size := writeSplitArchiveForTest(t, manifestBytes, splitArchiveTestEntry{
		name: "parts/part-000001.pdf", content: content, method: zip.Store,
	})

	got, entries, err := validateSplitArchiveForTest(t, file, size)
	require.NoError(t, err)
	require.Equal(t, 1, got.PartCount)
	staged, err := stageAndVerifySplitEntry(entries[0], got.Parts[0])
	require.NoError(t, err)
	stagedName := staged.Name()
	require.NoError(t, staged.Close())
	require.NoError(t, os.Remove(stagedName))
}

func TestValidateSplitArchiveRejectsTrailingManifestJSON(t *testing.T) {
	content := []byte("part")
	manifest := splitArchiveTestManifest(t, content)
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestBytes = append(manifestBytes, []byte(`{"unexpected":true}`)...)
	file, size := writeSplitArchiveForTest(t, manifestBytes, splitArchiveTestEntry{
		name: "parts/part-000001.pdf", content: content, method: zip.Store,
	})
	_, _, err = validateSplitArchiveForTest(t, file, size)
	require.ErrorContains(t, err, "manifest")
}

func TestValidateSplitArchiveRejectsUnsafeOrMismatchedParts(t *testing.T) {
	content := []byte("part")
	tests := []struct {
		name        string
		mutate      func(*documentsplit.Manifest)
		entryName   string
		entryMethod uint16
	}{
		{
			name: "path traversal", entryName: "../parts/part-000001.pdf",
			entryMethod: zip.Store,
		},
		{
			name: "extension mismatch",
			mutate: func(manifest *documentsplit.Manifest) {
				manifest.Parts[0].FileName = "part-000001.exe"
			},
			entryName: "parts/part-000001.exe", entryMethod: zip.Store,
		},
		{
			name:      "compressed transport",
			entryName: "parts/part-000001.pdf", entryMethod: zip.Deflate,
		},
		{
			name: "invalid locator",
			mutate: func(manifest *documentsplit.Manifest) {
				manifest.Parts[0].Locator = json.RawMessage(`[]`)
			},
			entryName: "parts/part-000001.pdf", entryMethod: zip.Store,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := splitArchiveTestManifest(t, content)
			if test.mutate != nil {
				test.mutate(&manifest)
			}
			manifestBytes, err := json.Marshal(manifest)
			require.NoError(t, err)
			file, size := writeSplitArchiveForTest(t, manifestBytes, splitArchiveTestEntry{
				name: test.entryName, content: content, method: test.entryMethod,
			})
			_, _, err = validateSplitArchiveForTest(t, file, size)
			require.Error(t, err)
		})
	}
}

func TestStageAndVerifySplitEntryRejectsContentHashMismatch(t *testing.T) {
	content := []byte("part")
	manifest := splitArchiveTestManifest(t, content)
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	file, size := writeSplitArchiveForTest(t, manifestBytes, splitArchiveTestEntry{
		name: "parts/part-000001.pdf", content: []byte("evil"), method: zip.Store,
	})
	// Keep the manifest size aligned so archive structure validation succeeds;
	// the staging boundary must still catch the content substitution.
	got, entries, err := validateSplitArchiveForTest(t, file, size)
	require.NoError(t, err)
	_, err = stageAndVerifySplitEntry(entries[0], got.Parts[0])
	require.ErrorContains(t, err, "hash or size mismatch")
}
