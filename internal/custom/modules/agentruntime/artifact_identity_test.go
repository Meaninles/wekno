package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestPostgresArtifactSessionUsesTerminalOwnerWithoutAccountRights(t *testing.T) {
	db := testsupport.Postgres(t, &Artifact{})
	require.NoError(t, db.Exec(`CREATE TABLE sessions (id VARCHAR(36) PRIMARY KEY,
		tenant_id BIGINT, user_id VARCHAR(512), deleted_at TIMESTAMP)`).Error)
	service := &Service{db: db}
	for _, tc := range []struct {
		name, session, user string
		principal           types.Principal
	}{
		{"web", "web-session", "real-account", types.Principal{Type: types.PrincipalWebUser, ID: "real-account"}},
		{"embed", "embed-session", "embed-channel", types.EmbedSessionPrincipal(7, "channel", "embed-session")},
		{"external", "external-session", "system-7", types.Principal{Type: types.PrincipalAPIExternalUser, ID: "7:" + strings.Repeat("v", 200)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := types.WithPrincipal(context.WithValue(context.Background(), types.UserIDContextKey, tc.user), tc.principal)
			owner := types.SessionOwnerIDFromContext(ctx)
			require.NoError(t, db.Exec(`INSERT INTO sessions VALUES (?, 7, ?, NULL)`, tc.session, owner).Error)
			meta := ArtifactUploadMetadata{TenantID: 7, UserID: owner, SessionID: tc.session}
			require.NoError(t, service.validateArtifactSession(ctx, meta))
			require.NoError(t, db.Create(&Artifact{TenantID: 7, UserID: owner, SessionID: tc.session}).Error)
			if tc.name != "web" {
				meta.UserID = tc.user
				require.ErrorContains(t, service.validateArtifactSession(ctx, meta), "artifact session is missing")
			}
			meta.UserID = owner
			meta.SessionID = "other-session"
			require.Error(t, service.validateArtifactSession(ctx, meta))
			meta.SessionID = tc.session
			meta.TenantID = 8
			require.Error(t, service.validateArtifactSession(ctx, meta))
		})
	}
}
