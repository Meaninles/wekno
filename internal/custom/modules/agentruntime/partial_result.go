package agentruntime

import (
	"github.com/Tencent/WeKnora/internal/custom/modules/usererrors"
	"gorm.io/gorm"
)

// This path performs no model generation and makes no claim of task completion.
// The same durable result feeds streaming, history and non-browser consumers.
func partialArtifacts(tx *gorm.DB, row *RunRecord, code string) (*ChatResult, error) {
	var files []Artifact
	if err := tx.Where("run_id = ? AND tenant_id = ? AND session_id = ? AND message_id = ? AND storage_state = ?", row.ID, row.TenantID, row.SessionID, row.MessageID, artifactStorageStateReady).Order("created_at, id").Find(&files).Error; err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	notice := "本次任务尚未全部完成，已保存的文件可先下载查看。"
	result := &ChatResult{RunID: row.ID, Status: "incomplete", FailureCode: code, ArtifactNotice: notice,
		Answer: notice + "\n\n" + usererrors.Classify(code, "").Message,
		Usage:  map[string]any{"model_requests": row.ModelRequests, "input_tokens": row.InputTokens, "output_tokens": row.OutputTokens}}
	for i := range files {
		file := sidecarArtifactFromRow(&files[i])
		result.Artifacts = append(result.Artifacts, *file)
	}
	return result, nil
}
