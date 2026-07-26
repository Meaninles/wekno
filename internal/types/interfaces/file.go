package interfaces

import (
	"context"
	"io"
	"mime/multipart"
)

// FileService is the interface for file services.
// FileService provides methods to save, retrieve, and delete files.
type FileService interface {
	// CheckConnectivity verifies that the storage backend is reachable and
	// properly configured (e.g. bucket exists, credentials valid).
	CheckConnectivity(ctx context.Context) error
	// SaveFile saves a file.
	SaveFile(ctx context.Context, file *multipart.FileHeader, tenantID uint64, knowledgeID string) (string, error)
	// SaveBytes saves bytes data to a file and returns the file path.
	// If temp is true, the file will be saved to a temporary storage that may auto-expire.
	SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error)
	// GetFile retrieves a file.
	GetFile(ctx context.Context, filePath string) (io.ReadCloser, error)
	// GetFileURL returns a download URL for the file (if supported by the storage backend).
	GetFileURL(ctx context.Context, filePath string) (string, error)
	// DeleteFile deletes a file.
	DeleteFile(ctx context.Context, filePath string) error
	// CopyFile copies an existing stored object to a NEW object owned by
	// (tenantID, knowledgeID), returning the new provider:// path. The copy is
	// independent: deleting the source never affects it. Returns ErrCrossBackendCopy
	// when srcPath belongs to a different storage provider than this service.
	CopyFile(ctx context.Context, srcPath string, tenantID uint64, knowledgeID string) (string, error)
}

// PlannedFileService is the crash-safe write extension used by the knowledge
// auxiliary ownership ledger. Reserve methods only allocate and validate the
// final provider path; they MUST NOT write an object. Commit methods write
// exactly that path and must be safe to retry. A commit error is treated as an
// uncertain outcome, so callers keep the pre-existing durable intent until an
// exact DeleteFile succeeds.
//
// This remains a separate interface so non-knowledge test doubles and external
// integrations are not silently given weaker semantics. Knowledge-owned write
// paths require this interface and fail closed when a provider lacks it.
type PlannedFileService interface {
	FileService
	ReserveFilePath(tenantID uint64, knowledgeID string, fileName string) (string, error)
	CommitFileAtPath(ctx context.Context, file *multipart.FileHeader, filePath string) error
	ReserveBytesPath(tenantID uint64, fileName string, temp bool) (string, error)
	CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error
	ReserveCopyPath(srcPath string, tenantID uint64, knowledgeID string) (string, error)
	CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error
}

// PrivateObjectFileService is the object-storage contract for durable,
// access-controlled application artifacts. Unlike the historical SaveBytes
// path (which some providers use for publicly renderable images), commits made
// through this interface MUST remain private. The caller supplies an exact,
// validated hierarchy so it can record the intent before performing remote
// I/O and safely retry an uncertain upload.
//
// Implementations must store sha256 as private object metadata and Verify must
// validate both the object length and that metadata without downloading the
// whole object.
type PrivateObjectFileService interface {
	FileService
	ReservePrivateObjectPath(segments ...string) (string, error)
	CommitPrivateObjectAtPath(
		ctx context.Context,
		data []byte,
		filePath string,
		contentType string,
		sha256 string,
	) error
	VerifyPrivateObject(ctx context.Context, filePath string, size int64, sha256 string) error
}

// StreamingPrivateObjectFileService is the bounded-memory extension used by
// the one-shot local-to-object-store migration. The caller hashes the source
// first, then reopens it and supplies the exact size and digest. Implementations
// must write the digest as private metadata so VerifyPrivateObject can prove the
// result without downloading multi-gigabyte source documents again.
type StreamingPrivateObjectFileService interface {
	PrivateObjectFileService
	CommitPrivateObjectStreamAtPath(
		ctx context.Context,
		reader io.Reader,
		size int64,
		filePath string,
		contentType string,
		sha256 string,
	) error
}
