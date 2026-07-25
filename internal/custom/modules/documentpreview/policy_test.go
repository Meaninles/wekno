package documentpreview

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnnotateUploadMetadataCannotBeOverridden(t *testing.T) {
	metadata := AnnotateUploadMetadata(
		map[string]string{MetadataModeKey: ModeOriginal, "caller": "kept"},
		"xlsx",
		true,
		false,
	)
	require.Equal(t, "kept", metadata["caller"])
	require.Equal(t, ModePagedChunks, metadata[MetadataModeKey])
	require.Equal(t, "heavy_structure", metadata[MetadataReasonKey])
}

func TestDecideRequiresStructuralAttestationForOffice(t *testing.T) {
	unverified := Decide("docx", 256*1024, 10, nil)
	require.Equal(t, ModePagedChunks, unverified.Mode)
	require.Equal(t, "unverified_structure", unverified.Reason)

	verified := Decide("docx", 256*1024, 10, map[string]string{
		MetadataModeKey: ModeOriginal,
	})
	require.True(t, AllowsOriginal(verified))
}

func TestDecideRejectsOversizeAndUnboundedDecoders(t *testing.T) {
	oversize := Decide("pdf", 25*mebibyte, 10, nil)
	require.Equal(t, "file_too_large", oversize.Reason)
	require.False(t, AllowsOriginal(oversize))

	webp := Decide("webp", 128*1024, 10, map[string]string{
		MetadataModeKey: ModeOriginal,
	})
	require.Equal(t, "unbounded_decoder", webp.Reason)
}

func TestDecideRejectsComplexDocumentsWithTooManyOrUnknownChunks(t *testing.T) {
	metadata := map[string]string{MetadataModeKey: ModeOriginal}

	largeWorkbook := Decide("xlsx", 64*1024, MaxComplexDocumentChunks+1, metadata)
	require.Equal(t, ModePagedChunks, largeWorkbook.Mode)
	require.Equal(t, "too_many_chunks", largeWorkbook.Reason)
	require.Equal(t, MaxComplexDocumentChunks+1, largeWorkbook.ChunkCount)

	unknownChunkCount := Decide("pptx", 64*1024, -1, metadata)
	require.Equal(t, ModePagedChunks, unknownChunkCount.Mode)
	require.Equal(t, "chunk_count_unavailable", unknownChunkCount.Reason)
}
