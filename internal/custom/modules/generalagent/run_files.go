package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

func originalInputFilesByID(items []OriginalInputFileSpec) map[string]OriginalInputFileSpec {
	out := make(map[string]OriginalInputFileSpec, len(items))
	for _, item := range items {
		if id := strings.TrimSpace(item.ID); id != "" {
			out[id] = item
		}
	}
	return out
}

// ResolveRunFile returns bytes only from the active run's registered artifact
// store or byte-verified original-input store. It intentionally does not accept
// URLs or filesystem paths.
func (s *Service) ResolveRunFile(
	ctx context.Context,
	runID string,
	sourceType string,
	sourceID string,
) ([]byte, string, string, error) {
	runID = strings.TrimSpace(runID)
	sourceType = strings.TrimSpace(sourceType)
	sourceID = strings.TrimSpace(sourceID)
	if runID == "" || sourceID == "" {
		return nil, "", "", fmt.Errorf("run_id and source_id are required")
	}
	run := lookupActiveRun(runID)
	if run == nil || run.runID != runID {
		return nil, "", "", fmt.Errorf("current agent run is no longer active")
	}

	switch sourceType {
	case "artifact":
		if run.client == nil {
			return nil, "", "", fmt.Errorf("artifact service is unavailable")
		}
		data, fileName, err := run.client.DownloadWithName(ctx, runID, sourceID)
		if err != nil {
			return nil, "", "", fmt.Errorf("read current-run artifact: %w", err)
		}
		if int64(len(data)) > originalInputMaxBytes() {
			return nil, "", "", fmt.Errorf("artifact exceeds ingestion limit")
		}
		return data, fileName, sha256Hex(data), nil
	case "input_file":
		spec, ok := run.originalInputFiles[sourceID]
		if !ok {
			return nil, "", "", fmt.Errorf("input file is not part of the current run")
		}
		if spec.Source != types.OriginalInputSourceChatUpload && spec.Source != types.OriginalInputSourceChatImage {
			return nil, "", "", fmt.Errorf("knowledge ingestion accepts only a current-turn uploaded attachment or a current-run artifact")
		}
		if strings.TrimSpace(spec.StorageURL) == "" {
			return nil, "", "", fmt.Errorf("input file has no trusted storage object")
		}
		fileService := resolveOriginalInputFileService(ctx, s.fileService)
		if fileService == nil {
			return nil, "", "", fmt.Errorf("original-input storage is unavailable")
		}
		reader, err := fileService.GetFile(ctx, spec.StorageURL)
		if err != nil {
			return nil, "", "", fmt.Errorf("read current-run input file: %w", err)
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, originalInputMaxBytes()+1))
		if err != nil {
			return nil, "", "", err
		}
		if int64(len(data)) > originalInputMaxBytes() {
			return nil, "", "", fmt.Errorf("input file exceeds ingestion limit")
		}
		sha := sha256Hex(data)
		if spec.FileSize >= 0 && int64(len(data)) != spec.FileSize {
			return nil, "", "", fmt.Errorf("input file size verification failed")
		}
		if expected := strings.ToLower(strings.TrimSpace(spec.SHA256)); expected == "" || sha != expected {
			return nil, "", "", fmt.Errorf("input file sha256 verification failed")
		}
		return data, spec.FileName, sha, nil
	default:
		return nil, "", "", fmt.Errorf("source_type must be artifact or input_file")
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
