package chatuploads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const bindingPrefix = "chat_upload_binding:"

type fileIdentity struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func uploadIdentity(k *types.Knowledge, id string) (fileIdentity, error) {
	metadata := k.GetMetadata()
	if metadata["chat_upload_id"] == id {
		return fileIdentity{k.FileName, k.FileSize, metadata["chat_upload_sha256"]}, nil
	}
	var identity fileIdentity
	err := json.Unmarshal([]byte(metadata[bindingPrefix+id]), &identity)
	return identity, err
}

func uploadIDs(k *types.Knowledge) []string {
	metadata := k.GetMetadata()
	ids := []string{metadata["chat_upload_id"]}
	for key := range metadata {
		if strings.HasPrefix(key, bindingPrefix) {
			ids = append(ids, strings.TrimPrefix(key, bindingPrefix))
		}
	}
	sort.Strings(ids)
	return ids
}

// Content deduplication may return an existing original. Persist this request's
// identity before returning success so a lost HTTP response remains recoverable.
// Merge only our metadata entry; ingestion can update unrelated metadata.
func (s *Service) bindDuplicate(ctx context.Context, session *types.Session, k *types.Knowledge, id string, identity fileIdentity) error {
	if k.TenantID != session.TenantID || k.KnowledgeBaseID != privateKBID(session.ID) ||
		k.FileSize != identity.Size || k.GetMetadata()["chat_upload_sha256"] != identity.SHA256 {
		return fmt.Errorf("duplicate original identity does not match")
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	entry, err := json.Marshal(map[string]string{bindingPrefix + id: string(payload)})
	if err != nil {
		return err
	}
	result := s.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND tenant_id = ? AND knowledge_base_id = ?", k.ID, session.TenantID, privateKBID(session.ID)).
		UpdateColumn("metadata", gorm.Expr("COALESCE(metadata::jsonb, '{}'::jsonb) || ?::jsonb", string(entry)))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("duplicate original no longer exists")
	}
	return s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", k.ID, session.TenantID).Take(k).Error
}
