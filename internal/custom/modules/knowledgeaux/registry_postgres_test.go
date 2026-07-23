package knowledgeaux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var postgresRegistrySequence atomic.Uint64

func openRegistryPostgresTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("WEKNORA_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not configured")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	schema := fmt.Sprintf("knowledgeaux_%d_%d", time.Now().UnixNano(), postgresRegistrySequence.Add(1))
	require.NoError(t, admin.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	})
	if strings.Contains(dsn, "://") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "search_path=" + schema
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
	))
	require.NoError(t, db.Create(&types.Tenant{ID: 7, Name: "tenant"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "kb"}).Error)
	return db
}

func createPostgresOwner(t *testing.T, db *gorm.DB, id, generation string) {
	t.Helper()
	require.NoError(t, db.Create(&types.Knowledge{
		ID: id, TenantID: 7, KnowledgeBaseID: "kb-1", Type: "file",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: generation,
	}).Error)
}

func TestPostgresKnowledgeLedgerPrefixLookupIsCollationIndependent(t *testing.T) {
	db := openRegistryPostgresTestDB(t)
	createPostgresOwner(t, db, "knowledge-1", "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.md", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"

	registered, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	// The production failure was locale-specific: the old
	// [prefix, prefix+U+FFFF) range returned zero rows on PostgreSQL even though
	// the exact dedup key existed. Exercise both the shared list path and the
	// worker's exact FileServiceForPath path against a real PostgreSQL schema.
	objects, err := registry.list(context.Background(), 7, "kb-1", []string{"knowledge-1"})
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, registered.Path, objects[0].object.Path)

	service, err := registry.FileServiceForPath(
		context.Background(), 7, "kb-1", "knowledge-1", registered.Path, "local",
	)
	require.NoError(t, err)
	require.Same(t, local, service)
}

func TestPostgresPersistentOwnershipMovesInKnowledgeTransaction(t *testing.T) {
	db := openRegistryPostgresTestDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{
		ID: "kb-2", TenantID: 7, Name: "target",
	}).Error)
	path := "local://7/knowledge-1/source.md"
	createPostgresOwner(t, db, "knowledge-1", "generation-1")
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("tenant_id = ? AND id = ?", 7, "knowledge-1").
		Update("file_path", path).Error)
	registry := testRegistry(db, map[string]*fakeFileService{
		"local": {provider: "local", failures: map[string]int{}},
	})
	object := objectFor(path, "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockActiveSharedSet(tx, 7, "kb-1", "kb-2"); err != nil {
			return err
		}
		result := tx.Model(&types.Knowledge{}).
			Where("tenant_id = ? AND id = ? AND knowledge_base_id = ?", 7, "knowledge-1", "kb-1").
			Updates(map[string]interface{}{
				"knowledge_base_id":     "kb-2",
				"processing_generation": "generation-2",
			})
		if result.Error != nil {
			return result.Error
		}
		require.EqualValues(t, 1, result.RowsAffected)
		_, err := TransferPersistentOwnershipTx(
			tx, 7, "knowledge-1", "kb-1", "kb-2", "generation-2", path,
		)
		return err
	}))

	var ownership types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&ownership).Error)
	require.Equal(t, "kb-2", ownership.ScopeID)
	var moved Object
	require.NoError(t, json.Unmarshal(ownership.Payload, &moved))
	require.Equal(t, "kb-2", moved.KnowledgeBaseID)
	require.Equal(t, "generation-2", moved.ProcessingGeneration)
}

func TestPostgresSharedCommitFenceAndExclusiveDelete(t *testing.T) {
	t.Run("two commits on one KB run concurrently", func(t *testing.T) {
		db := openRegistryPostgresTestDB(t)
		createPostgresOwner(t, db, "knowledge-1", "generation-1")
		createPostgresOwner(t, db, "knowledge-2", "generation-2")
		local := &fakeFileService{provider: "local", failures: map[string]int{}}
		registry := testRegistry(db, map[string]*fakeFileService{"local": local})
		second := objectFor("local://7/two.png", "generation-2", KindFanoutImage)
		second.KnowledgeID = "knowledge-2"
		objects := []Object{
			objectFor("local://7/one.png", "generation-1", KindFanoutImage),
			second,
		}
		for _, object := range objects {
			_, err := registry.Reserve(context.Background(), object, false)
			require.NoError(t, err)
		}
		started := make(chan string, 2)
		release := make(chan struct{})
		done := make(chan error, 2)
		for _, object := range objects {
			object := object
			go func() {
				done <- registry.CommitReserved(context.Background(), object, false, func() error {
					started <- object.Path
					<-release
					return nil
				})
			}()
		}
		for range 2 {
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("shared KB commits serialized unexpectedly")
			}
		}
		close(release)
		require.NoError(t, <-done)
		require.NoError(t, <-done)
	})

	t.Run("commit first makes delete wait", func(t *testing.T) {
		db := openRegistryPostgresTestDB(t)
		createPostgresOwner(t, db, "knowledge-1", "generation-1")
		deleteStarted := make(chan string, 1)
		local := &fakeFileService{provider: "local", failures: map[string]int{}, started: deleteStarted}
		registry := testRegistry(db, map[string]*fakeFileService{"local": local})
		object := objectFor("local://7/image.png", "generation-1", KindFanoutImage)
		object.FallbackProvider = "local"
		_, err := registry.Reserve(context.Background(), object, false)
		require.NoError(t, err)
		commitStarted := make(chan struct{})
		releaseCommit := make(chan struct{})
		commitDone := make(chan error, 1)
		go func() {
			commitDone <- registry.CommitReserved(context.Background(), object, false, func() error {
				close(commitStarted)
				<-releaseCommit
				return nil
			})
		}()
		<-commitStarted
		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- registry.DeletePaths(
				context.Background(), 7, "kb-1", "knowledge-1", "local", []string{object.Path},
			)
		}()
		select {
		case path := <-deleteStarted:
			t.Fatalf("delete escaped shared commit fence: %s", path)
		case <-time.After(100 * time.Millisecond):
		}
		close(releaseCommit)
		require.NoError(t, <-commitDone)
		require.NoError(t, <-deleteDone)
	})

	t.Run("delete first prevents commit callback", func(t *testing.T) {
		db := openRegistryPostgresTestDB(t)
		createPostgresOwner(t, db, "knowledge-1", "generation-1")
		deleteStarted := make(chan string, 1)
		releaseDelete := make(chan struct{})
		local := &fakeFileService{
			provider: "local", failures: map[string]int{}, started: deleteStarted, release: releaseDelete,
		}
		registry := testRegistry(db, map[string]*fakeFileService{"local": local})
		object := objectFor("local://7/image.png", "generation-1", KindFanoutImage)
		object.FallbackProvider = "local"
		_, err := registry.Reserve(context.Background(), object, false)
		require.NoError(t, err)
		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- registry.DeletePaths(
				context.Background(), 7, "kb-1", "knowledge-1", "local", []string{object.Path},
			)
		}()
		<-deleteStarted
		var commitCalls atomic.Int32
		commitDone := make(chan error, 1)
		go func() {
			commitDone <- registry.CommitReserved(context.Background(), object, false, func() error {
				commitCalls.Add(1)
				return nil
			})
		}()
		select {
		case err := <-commitDone:
			t.Fatalf("commit escaped exclusive delete fence: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(releaseDelete)
		require.NoError(t, <-deleteDone)
		require.ErrorIs(t, <-commitDone, ErrReservationLost)
		require.Zero(t, commitCalls.Load())
	})
}
