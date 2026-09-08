package agentruntime

import (
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
)

func TestPostgresFailureDeliversOnlyDurableScopedArtifactsWithoutMarkingSuccess(t *testing.T) {
	for _, kind := range []string{"general-agent", "document-processing-agent", "table-analysis", "custom-agent"} {
		t.Run(kind, func(t *testing.T) {
			db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{}, &Artifact{})
			require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text,
 content text, error_code varchar(40), knowledge_references jsonb, agent_steps jsonb, is_completed boolean,
 agent_duration_ms bigint, retrieval_stats jsonb, agent_tool_count integer, updated_at timestamptz, deleted_at timestamptz)`).Error)
			row := fixtureRun(t, db)
			row.Payload = json.RawMessage(`{"enable_artifacts":true,"runtime_config":{"agent_type":"` + kind + `"}}`)
			require.NoError(t, db.Save(row).Error)
			require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id,role) VALUES ('message-1','session-1','assistant')`).Error)
			for _, state := range []string{artifactStorageStateReady, artifactStorageStateUploading, artifactStorageStateCorrupt} {
				file := &Artifact{ID: state, TenantID: row.TenantID, UserID: row.UserID, RunID: row.ID, SessionID: row.SessionID, MessageID: row.MessageID,
					FileToken: state, FileName: "report.xlsx", SHA256: "digest", FileSize: 100, StorageState: state}
				require.NoError(t, db.Create(file).Error)
			}
			require.NoError(t, db.Create(&Artifact{ID: "other-tenant", TenantID: 99, RunID: row.ID, SessionID: row.SessionID, MessageID: row.MessageID, StorageState: artifactStorageStateReady}).Error)
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				return terminateRun(tx, row, "failed", "private connection details", "connection")
			}))
			require.Equal(t, "incomplete", row.Status)
			var result ChatResult
			require.NoError(t, json.Unmarshal(row.Result, &result))
			require.Equal(t, "incomplete", result.Status)
			require.Equal(t, "connection", result.FailureCode)
			require.Len(t, result.Artifacts, 1)
			require.Contains(t, result.Answer, "尚未全部完成")
			require.NotContains(t, result.Answer, "/artifacts/")
			require.Contains(t, result.Artifacts[0].DownloadURL, "/artifacts/ready/download")
			require.NotContains(t, result.Answer, "private")
			var msg struct {
				Content     string
				ErrorCode   string
				IsCompleted bool
			}
			require.NoError(t, db.Table("messages").First(&msg).Error)
			require.Equal(t, result.Answer, msg.Content)
			require.Empty(t, msg.ErrorCode, "Do not hide the persisted download behind a full-error renderer")
			var events []RunOutbox
			require.NoError(t, db.Where("run_id = ?", row.ID).Find(&events).Error)
			require.Len(t, events, 1)
			var terminal StreamEvent
			require.NoError(t, json.Unmarshal(events[0].Body, &terminal))
			require.Equal(t, "result", terminal.Type)
		})
	}
}
