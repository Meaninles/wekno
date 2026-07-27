// Package storagemigration provides the explicit, offline migration from the
// historical local:// store to the deployment-scoped MinIO/OBS namespace.
//
// It is intentionally not a startup hook. Operators first stop every app
// replica, run the audit, then run apply. Uploads are deterministic and
// idempotent; the database cutover is one locked transaction.
package storagemigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

const localTokenPattern = `local://[0-9]+/[A-Za-z0-9._/-]+`

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Mode string

const (
	ModeAudit Mode = "audit"
	ModeApply Mode = "apply"
)

type Config struct {
	Mode          Mode
	SourceBaseDir string
	Provider      string
	Bucket        string
	PathPrefix    string
	Concurrency   int
}

type Report struct {
	Mode                      Mode     `json:"mode"`
	Provider                  string   `json:"provider"`
	Bucket                    string   `json:"bucket"`
	PathPrefix                string   `json:"path_prefix"`
	ReferencedObjects         int      `json:"referenced_objects"`
	ExistingReferencedObjects int      `json:"existing_referenced_objects"`
	MissingHistoricalObjects  int      `json:"missing_historical_objects"`
	InvalidHistoricalRefs     int      `json:"invalid_historical_refs"`
	MissingRequiredObjects    int      `json:"missing_required_objects"`
	MissingRequiredPathHashes []string `json:"missing_required_path_hashes,omitempty"`
	ReferencedBytes           int64    `json:"referenced_bytes"`
	UploadedObjects           int      `json:"uploaded_objects"`
	AlreadyVerifiedObjects    int      `json:"already_verified_objects"`
	RewrittenColumns          int      `json:"rewritten_columns"`
	RewrittenRows             int64    `json:"rewritten_rows"`
	KnowledgeBasesSwitched    int64    `json:"knowledge_bases_switched"`
	TenantsSwitched           int64    `json:"tenants_switched"`
	AuxiliaryLedgerRowsSigned int64    `json:"auxiliary_ledger_rows_signed"`
	RemainingLocalRefColumns  int      `json:"remaining_local_ref_columns"`
	RemainingLocalReferences  int64    `json:"remaining_local_references"`
	SourceFilesRetained       bool     `json:"source_files_retained"`
}

type ProgressFunc func(done, total int, bytes int64)

type Migrator struct {
	db       *gorm.DB
	config   Config
	target   interfaces.StreamingPrivateObjectFileService
	uriRoot  string
	progress ProgressFunc
}

type refColumn struct {
	Table    string
	Column   string
	DataType string
	Count    int64
}

type sourceObject struct {
	Ref      string
	Path     string
	Relative string
	Size     int64
	Required bool
}

type uploadResult struct {
	uploaded bool
	size     int64
	err      error
}

func NewFromEnv(db *gorm.DB, mode Mode, concurrency int) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("storage migration database is required")
	}
	if db.Dialector.Name() != "postgres" {
		return nil, fmt.Errorf("storage migration requires PostgreSQL, got %q", db.Dialector.Name())
	}
	if mode != ModeAudit && mode != ModeApply {
		return nil, fmt.Errorf("unsupported storage migration mode %q", mode)
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	if concurrency > 8 {
		return nil, fmt.Errorf("storage migration concurrency %d exceeds the safety ceiling 8", concurrency)
	}

	baseDir := strings.TrimSpace(os.Getenv("CUSTOM_STORAGE_MIGRATION_SOURCE_BASE_DIR"))
	if baseDir == "" {
		baseDir = strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	}
	if baseDir == "" {
		return nil, errors.New("CUSTOM_STORAGE_MIGRATION_SOURCE_BASE_DIR or LOCAL_STORAGE_BASE_DIR is required")
	}
	absoluteBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve local migration source: %w", err)
	}
	info, err := os.Stat(absoluteBase)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("local migration source %q is unavailable or not a directory", absoluteBase)
	}

	provider := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
	prefix, err := objectnamespace.KnowledgePrefixFromEnv(provider)
	if err != nil {
		return nil, err
	}
	var (
		bucket string
		base   interfaces.FileService
	)
	switch provider {
	case "minio":
		bucket = strings.TrimSpace(os.Getenv("MINIO_BUCKET_NAME"))
		if bucket == "" ||
			strings.TrimSpace(os.Getenv("MINIO_ENDPOINT")) == "" ||
			strings.TrimSpace(os.Getenv("MINIO_ACCESS_KEY_ID")) == "" ||
			strings.TrimSpace(os.Getenv("MINIO_SECRET_ACCESS_KEY")) == "" {
			return nil, errors.New("incomplete global MinIO migration target")
		}
		base, err = filesvc.NewMinioFileServiceWithPathPrefix(
			strings.TrimSpace(os.Getenv("MINIO_ENDPOINT")),
			strings.TrimSpace(os.Getenv("MINIO_ACCESS_KEY_ID")),
			strings.TrimSpace(os.Getenv("MINIO_SECRET_ACCESS_KEY")),
			bucket,
			strings.EqualFold(strings.TrimSpace(os.Getenv("MINIO_USE_SSL")), "true"),
			prefix,
		)
	case "obs":
		bucket = strings.TrimSpace(os.Getenv("OBS_BUCKET_NAME"))
		if bucket == "" ||
			strings.TrimSpace(os.Getenv("OBS_ENDPOINT")) == "" ||
			strings.TrimSpace(os.Getenv("OBS_ACCESS_KEY")) == "" ||
			strings.TrimSpace(os.Getenv("OBS_SECRET_KEY")) == "" {
			return nil, errors.New("incomplete global OBS migration target")
		}
		region := strings.TrimSpace(os.Getenv("OBS_REGION"))
		if region == "" {
			region = "cn-north-4"
		}
		base, err = filesvc.NewObsFileService(
			strings.TrimSpace(os.Getenv("OBS_ENDPOINT")),
			region,
			strings.TrimSpace(os.Getenv("OBS_ACCESS_KEY")),
			strings.TrimSpace(os.Getenv("OBS_SECRET_KEY")),
			bucket,
			prefix,
		)
	default:
		return nil, fmt.Errorf("storage migration target must be minio or obs, got %q", provider)
	}
	if err != nil {
		return nil, err
	}
	// The migrated binding must be identical to the process-wide service that
	// app replicas will inject later. A direct/direct binding cannot be
	// reconstructed after this one-shot process exits.
	base = filesvc.MarkGlobalStorageService(base)
	target, ok := base.(interfaces.StreamingPrivateObjectFileService)
	if !ok {
		return nil, fmt.Errorf("storage migration target %q lacks streaming private commits", provider)
	}
	return &Migrator{
		db: db,
		config: Config{
			Mode: mode, SourceBaseDir: absoluteBase, Provider: provider,
			Bucket: bucket, PathPrefix: prefix, Concurrency: concurrency,
		},
		target:  target,
		uriRoot: provider + "://" + bucket + "/" + strings.Trim(prefix, "/") + "/",
	}, nil
}

func (m *Migrator) SetProgress(progress ProgressFunc) {
	if m != nil {
		m.progress = progress
	}
}

func (m *Migrator) Run(ctx context.Context) (Report, error) {
	report := Report{
		Mode: m.config.Mode, Provider: m.config.Provider, Bucket: m.config.Bucket,
		PathPrefix: m.config.PathPrefix, SourceFilesRetained: true,
	}
	if err := m.target.CheckConnectivity(ctx); err != nil {
		return report, fmt.Errorf("storage migration target connectivity: %w", err)
	}
	columns, allRefs, err := m.scanAllReferences(ctx)
	if err != nil {
		return report, err
	}
	requiredRefs, err := m.scanRequiredReferences(ctx)
	if err != nil {
		return report, err
	}
	objects, missing, invalidHistorical, missingRequired, err := m.inspectSources(allRefs, requiredRefs)
	if err != nil {
		return report, err
	}
	report.ReferencedObjects = len(allRefs)
	report.ExistingReferencedObjects = len(objects)
	report.MissingHistoricalObjects = missing
	report.InvalidHistoricalRefs = invalidHistorical
	report.MissingRequiredObjects = len(missingRequired)
	for _, ref := range missingRequired {
		if len(report.MissingRequiredPathHashes) >= 50 {
			break
		}
		report.MissingRequiredPathHashes = append(
			report.MissingRequiredPathHashes,
			scrubPath(ref),
		)
	}
	for _, object := range objects {
		report.ReferencedBytes += object.Size
	}
	if len(missingRequired) > 0 {
		return report, fmt.Errorf(
			"storage migration blocked: %d currently required local objects are missing",
			len(missingRequired),
		)
	}
	if m.config.Mode == ModeAudit {
		report.RemainingLocalRefColumns = len(columns)
		for _, column := range columns {
			report.RemainingLocalReferences += column.Count
		}
		return report, nil
	}

	unlock, err := m.acquireMigrationLock(ctx)
	if err != nil {
		return report, err
	}
	defer unlock()
	if err := m.requireOffline(ctx); err != nil {
		return report, err
	}

	uploaded, verified, err := m.uploadAll(ctx, objects)
	report.UploadedObjects = uploaded
	report.AlreadyVerifiedObjects = verified
	if err != nil {
		return report, err
	}
	switchedKBs, switchedTenants, ledgerRows, rewrittenColumns, rewrittenRows, err :=
		m.rewriteReferences(ctx, columns)
	report.KnowledgeBasesSwitched = switchedKBs
	report.TenantsSwitched = switchedTenants
	report.AuxiliaryLedgerRowsSigned = ledgerRows
	report.RewrittenColumns = rewrittenColumns
	report.RewrittenRows = rewrittenRows
	if err != nil {
		return report, err
	}
	remainingColumns, _, err := m.scanAllReferences(ctx)
	if err != nil {
		return report, err
	}
	report.RemainingLocalRefColumns = len(remainingColumns)
	for _, column := range remainingColumns {
		report.RemainingLocalReferences += column.Count
	}
	if report.RemainingLocalReferences != 0 {
		return report, fmt.Errorf(
			"storage migration cutover left %d local references in %d columns",
			report.RemainingLocalReferences,
			report.RemainingLocalRefColumns,
		)
	}
	return report, nil
}

func (m *Migrator) scanAllReferences(
	ctx context.Context,
) ([]refColumn, map[string]struct{}, error) {
	type candidate struct {
		TableName  string
		ColumnName string
		DataType   string
	}
	var candidates []candidate
	if err := m.db.WithContext(ctx).Raw(`
		SELECT c.table_name, c.column_name, c.data_type
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = 'public'
		  AND t.table_type = 'BASE TABLE'
		  AND c.data_type IN ('text', 'character varying', 'json', 'jsonb')
		ORDER BY c.table_name, c.ordinal_position
	`).Scan(&candidates).Error; err != nil {
		return nil, nil, fmt.Errorf("scan storage reference columns: %w", err)
	}

	refs := make(map[string]struct{})
	columns := make([]refColumn, 0)
	for _, candidate := range candidates {
		// Final Agent artifacts have their own state machine, retention policy,
		// and private namespace. Mixing their legacy rows into the knowledge
		// namespace would defeat both access isolation and the artifact
		// migrator's corruption handling.
		if candidate.TableName == "custom_general_agent_artifacts" &&
			candidate.ColumnName == "file_path" {
			continue
		}
		if !safeIdentifier.MatchString(candidate.TableName) ||
			!safeIdentifier.MatchString(candidate.ColumnName) {
			return nil, nil, errors.New("database contains an unsafe storage reference identifier")
		}
		table := quoteIdentifier(candidate.TableName)
		column := quoteIdentifier(candidate.ColumnName)
		var count int64
		countSQL := fmt.Sprintf(
			`SELECT count(*) FROM %s WHERE %s::text LIKE '%%local://%%'`,
			table,
			column,
		)
		if err := m.db.WithContext(ctx).Raw(countSQL).Scan(&count).Error; err != nil {
			return nil, nil, fmt.Errorf(
				"count local references in %s.%s: %w",
				candidate.TableName,
				candidate.ColumnName,
				err,
			)
		}
		if count == 0 {
			continue
		}
		columns = append(columns, refColumn{
			Table: candidate.TableName, Column: candidate.ColumnName,
			DataType: candidate.DataType, Count: count,
		})
		pathSQL := fmt.Sprintf(`
			SELECT DISTINCT match[1] AS path
			FROM (
				SELECT regexp_matches(%s::text, ?, 'g') AS match
				FROM %s
				WHERE %s::text LIKE '%%local://%%'
			) refs
		`, column, table, column)
		rows, err := m.db.WithContext(ctx).Raw(pathSQL, localTokenPattern).Rows()
		if err != nil {
			return nil, nil, fmt.Errorf(
				"scan local paths in %s.%s: %w",
				candidate.TableName,
				candidate.ColumnName,
				err,
			)
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return nil, nil, err
			}
			refs[strings.TrimSpace(path)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	return columns, refs, nil
}

func (m *Migrator) scanRequiredReferences(ctx context.Context) (map[string]struct{}, error) {
	type spec struct {
		table     string
		column    string
		predicate string
	}
	specs := []spec{
		{
			table: "knowledges", column: "file_path",
			predicate: "deleted_at IS NULL AND parse_status NOT IN ('failed', 'cancelled', 'deleting')",
		},
		{
			table: "knowledges", column: "processing_fanout",
			predicate: "deleted_at IS NULL AND parse_status NOT IN ('failed', 'cancelled', 'deleting')",
		},
		{
			table: "knowledges", column: "last_faq_import_result",
			predicate: "deleted_at IS NULL AND parse_status NOT IN ('failed', 'cancelled', 'deleting')",
		},
		{table: "chunks", column: "image_info", predicate: "deleted_at IS NULL"},
	}
	required := make(map[string]struct{})
	for _, item := range specs {
		var exists bool
		if err := m.db.WithContext(ctx).Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = ? AND column_name = ?
			)
		`, item.table, item.column).Scan(&exists).Error; err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		query := fmt.Sprintf(`
			SELECT DISTINCT match[1] AS path
			FROM (
				SELECT regexp_matches(%s::text, ?, 'g') AS match
				FROM %s
				WHERE %s AND %s::text LIKE '%%local://%%'
			) refs
		`,
			quoteIdentifier(item.column),
			quoteIdentifier(item.table),
			item.predicate,
			quoteIdentifier(item.column),
		)
		rows, err := m.db.WithContext(ctx).Raw(query, localTokenPattern).Rows()
		if err != nil {
			return nil, fmt.Errorf("scan required paths from %s.%s: %w", item.table, item.column, err)
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return nil, err
			}
			required[strings.TrimSpace(path)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return required, nil
}

func (m *Migrator) inspectSources(
	refs map[string]struct{},
	required map[string]struct{},
) ([]sourceObject, int, int, []string, error) {
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	objects := make([]sourceObject, 0, len(keys))
	missing := 0
	invalidHistorical := 0
	missingRequired := make([]string, 0)
	for _, ref := range keys {
		relative := strings.TrimPrefix(ref, "local://")
		if relative == ref || plannedfile.ValidateKey(relative, "") != nil {
			if _, ok := required[ref]; ok {
				missingRequired = append(missingRequired, ref)
			} else {
				invalidHistorical++
			}
			continue
		}
		candidate := filepath.Join(m.config.SourceBaseDir, filepath.FromSlash(relative))
		safePath, err := secutils.SafePathUnderBase(m.config.SourceBaseDir, candidate)
		if err != nil {
			return nil, 0, 0, nil, fmt.Errorf("unsafe local object %s: %w", scrubPath(ref), err)
		}
		info, err := os.Lstat(safePath)
		if err != nil {
			if os.IsNotExist(err) {
				missing++
				if _, ok := required[ref]; ok {
					missingRequired = append(missingRequired, ref)
				}
				continue
			}
			return nil, 0, 0, nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, 0, 0, nil, fmt.Errorf("local object %s is not a regular file", scrubPath(ref))
		}
		_, isRequired := required[ref]
		objects = append(objects, sourceObject{
			Ref: ref, Path: safePath, Relative: relative, Size: info.Size(), Required: isRequired,
		})
	}
	return objects, missing, invalidHistorical, missingRequired, nil
}

func (m *Migrator) uploadAll(
	ctx context.Context,
	objects []sourceObject,
) (int, int, error) {
	if len(objects) == 0 {
		return 0, 0, nil
	}
	jobs := make(chan sourceObject)
	results := make(chan uploadResult)
	var workers sync.WaitGroup
	for index := 0; index < m.config.Concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for object := range jobs {
				uploaded, err := m.uploadOne(ctx, object)
				select {
				case results <- uploadResult{uploaded: uploaded, size: object.Size, err: err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, object := range objects {
			select {
			case jobs <- object:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	var uploaded, verified, done int
	var processedBytes int64
	var firstErr error
	for result := range results {
		done++
		processedBytes += result.size
		if result.uploaded {
			uploaded++
		} else if result.err == nil {
			verified++
		}
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		if m.progress != nil {
			m.progress(done, len(objects), processedBytes)
		}
	}
	if firstErr != nil {
		return uploaded, verified, firstErr
	}
	if done != len(objects) {
		return uploaded, verified, fmt.Errorf(
			"storage migration upload stopped after %d/%d objects",
			done,
			len(objects),
		)
	}
	return uploaded, verified, nil
}

func (m *Migrator) uploadOne(ctx context.Context, object sourceObject) (bool, error) {
	file, err := os.Open(object.Path)
	if err != nil {
		return false, fmt.Errorf("open local object %s: %w", scrubPath(object.Ref), err)
	}
	defer file.Close()
	infoBefore, err := file.Stat()
	if err != nil {
		return false, err
	}
	digest, size, err := hashAndRewind(file)
	if err != nil {
		return false, fmt.Errorf("hash local object %s: %w", scrubPath(object.Ref), err)
	}
	if size != object.Size || size != infoBefore.Size() {
		return false, fmt.Errorf("local object %s changed during audit", scrubPath(object.Ref))
	}
	segments := strings.Split(object.Relative, "/")
	destination, err := m.target.ReservePrivateObjectPath(segments...)
	if err != nil {
		return false, fmt.Errorf("reserve target for %s: %w", scrubPath(object.Ref), err)
	}
	if !strings.HasPrefix(destination, m.uriRoot) {
		return false, fmt.Errorf("target escaped deployment namespace for %s", scrubPath(object.Ref))
	}
	if err := m.target.VerifyPrivateObject(ctx, destination, size, digest); err == nil {
		return false, nil
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(object.Path)))
	err = m.target.CommitPrivateObjectStreamAtPath(
		ctx,
		file,
		size,
		destination,
		contentType,
		digest,
	)
	if err != nil {
		return false, fmt.Errorf("upload local object %s: %w", scrubPath(object.Ref), err)
	}
	// The OBS client may read the seekable source multiple times for SigV4
	// signing, the actual upload and retries, leaving the file offset at an
	// arbitrary position. Rewind and re-hash to prove the source did not
	// change while the object was being uploaded. This replaces the former
	// TeeReader + counting reader wrapper that hid the *os.File seekability
	// from the S3 SDK and forced PutObject to fail with
	// "request stream is not seekable".
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = m.target.DeleteFile(ctx, destination)
		return false, fmt.Errorf("local object %s rewind after upload: %w", scrubPath(object.Ref), err)
	}
	postDigest, postSize, err := hashAndRewind(file)
	if err != nil || postSize != size || postDigest != digest {
		_ = m.target.DeleteFile(ctx, destination)
		return false, fmt.Errorf("local object %s changed while uploading", scrubPath(object.Ref))
	}
	if err := m.target.VerifyPrivateObject(ctx, destination, size, digest); err != nil {
		return false, fmt.Errorf("verify migrated object %s: %w", scrubPath(object.Ref), err)
	}
	return true, nil
}

func (m *Migrator) acquireMigrationLock(ctx context.Context) (func(), error) {
	sqlDB, err := m.db.DB()
	if err != nil {
		return nil, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err := conn.QueryRowContext(
		ctx,
		`SELECT pg_try_advisory_lock(824742609, 182736451)`,
	).Scan(&locked); err != nil {
		conn.Close()
		return nil, err
	}
	if !locked {
		conn.Close()
		return nil, errors.New("another storage migration already owns the PostgreSQL lock")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(
				releaseCtx,
				`SELECT pg_advisory_unlock(824742609, 182736451)`,
			)
			_ = conn.Close()
		})
	}, nil
}

func (m *Migrator) requireOffline(ctx context.Context) error {
	var activeInstances int64
	if err := m.db.WithContext(ctx).Raw(`
		SELECT count(*)
		FROM custom_document_queue_instances
		WHERE state <> 'stopped'
		  AND last_heartbeat_at > now() - interval '45 seconds'
	`).Scan(&activeInstances).Error; err != nil {
		return fmt.Errorf("verify application replicas stopped: %w", err)
	}
	if activeInstances != 0 {
		return fmt.Errorf(
			"storage migration apply requires every app replica stopped; %d recent coordinator heartbeat(s) remain",
			activeInstances,
		)
	}
	var activeWorkflows int64
	if err := m.db.WithContext(ctx).Raw(`
		SELECT count(*)
		FROM custom_document_queue_workflows
		WHERE state IN ('preparing', 'queued', 'leased', 'waiting_external')
	`).Scan(&activeWorkflows).Error; err != nil {
		return fmt.Errorf("verify document workflows drained: %w", err)
	}
	if activeWorkflows != 0 {
		return fmt.Errorf(
			"storage migration apply requires a drained document queue; %d workflow(s) are non-terminal",
			activeWorkflows,
		)
	}
	return nil
}

func (m *Migrator) rewriteReferences(
	ctx context.Context,
	columns []refColumn,
) (int64, int64, int64, int, int64, error) {
	bindingProvider, ok := m.target.(storagebinding.BindingProvider)
	if !ok {
		return 0, 0, 0, 0, 0, errors.New("storage migration target does not expose a durable binding")
	}
	probe, err := m.target.ReservePrivateObjectPath("__storage_migration_binding_probe__")
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	targetBinding, err := bindingProvider.BindingForPath(probe)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("derive migrated storage binding: %w", err)
	}
	rawBinding, err := json.Marshal(targetBinding)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}

	var switchedKBs, switchedTenants, ledgerRows, rewrittenRows int64
	rewrittenColumns := 0
	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tables := make(map[string]struct{})
		for _, column := range columns {
			tables[column.Table] = struct{}{}
		}
		tables["knowledge_bases"] = struct{}{}
		tables["tenants"] = struct{}{}
		tables["task_pending_ops"] = struct{}{}
		tableNames := make([]string, 0, len(tables))
		for table := range tables {
			tableNames = append(tableNames, table)
		}
		sort.Strings(tableNames)
		for _, table := range tableNames {
			if !safeIdentifier.MatchString(table) {
				return errors.New("unsafe table name in storage cutover")
			}
			if err := tx.Exec("LOCK TABLE " + quoteIdentifier(table) + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				return fmt.Errorf("lock %s for storage cutover: %w", table, err)
			}
		}

		for _, column := range columns {
			table := quoteIdentifier(column.Table)
			name := quoteIdentifier(column.Column)
			cast := ""
			switch column.DataType {
			case "json":
				cast = "::json"
			case "jsonb":
				cast = "::jsonb"
			case "text", "character varying":
			default:
				return fmt.Errorf("unsupported storage reference column type %q", column.DataType)
			}
			query := fmt.Sprintf(
				`UPDATE %s SET %s = replace(%s::text, 'local://', ?)%s WHERE %s::text LIKE '%%local://%%'`,
				table,
				name,
				name,
				cast,
				name,
			)
			result := tx.Exec(query, m.uriRoot)
			if result.Error != nil {
				return fmt.Errorf("rewrite %s.%s: %w", column.Table, column.Column, result.Error)
			}
			if result.RowsAffected > 0 {
				rewrittenColumns++
				rewrittenRows += result.RowsAffected
			}
		}

		// The ownership ledger stores both the provider path and a signed,
		// non-secret physical binding. Prefix replacement alone would leave a
		// MinIO/OBS path signed as local, and every replica would correctly
		// reject it. Re-sign the binding and the path-derived dedup key in the
		// same cutover transaction.
		ledgerResult := tx.Exec(`
			UPDATE task_pending_ops
			SET payload = jsonb_set(
					jsonb_set(
						payload,
						'{storage_binding}',
						CAST(? AS jsonb),
						true
					),
					'{fallback_provider}',
					to_jsonb(CAST(? AS text)),
					true
				),
				dedup_key = payload->>'knowledge_id' || ':' ||
					encode(digest(btrim(payload->>'path'), 'sha256'), 'hex')
			WHERE task_type = 'knowledge:aux_object'
			  AND payload->>'path' LIKE ?
		`, string(rawBinding), m.config.Provider, m.uriRoot+"%")
		if ledgerResult.Error != nil {
			return fmt.Errorf("re-sign knowledge auxiliary storage ledger: %w", ledgerResult.Error)
		}
		ledgerRows = ledgerResult.RowsAffected

		kbResult := tx.Exec(`
			UPDATE knowledge_bases
			SET storage_provider_config = jsonb_set(
				COALESCE(storage_provider_config, '{}'::jsonb),
				'{provider}',
				to_jsonb(CAST(? AS text)),
				true
			)
			WHERE COALESCE(storage_provider_config->>'provider', '') IN ('', 'local', '__pending_env__')
		`, m.config.Provider)
		if kbResult.Error != nil {
			return fmt.Errorf("switch knowledge-base storage provider: %w", kbResult.Error)
		}
		switchedKBs = kbResult.RowsAffected

		targetConfig := map[string]any{
			"bucket_name": m.config.Bucket,
			"path_prefix": m.config.PathPrefix,
		}
		if m.config.Provider == "minio" {
			targetConfig["mode"] = "docker"
			targetConfig["use_ssl"] = strings.EqualFold(
				strings.TrimSpace(os.Getenv("MINIO_USE_SSL")),
				"true",
			)
		}
		rawConfig, err := json.Marshal(targetConfig)
		if err != nil {
			return err
		}
		tenantResult := tx.Exec(`
			UPDATE tenants
			SET storage_engine_config = jsonb_set(
				jsonb_set(
					COALESCE(storage_engine_config, '{}'::jsonb) - 'local',
					'{default_provider}',
					to_jsonb(CAST(? AS text)),
					true
				),
				ARRAY[CAST(? AS text)],
				CAST(? AS jsonb),
				true
			)
			WHERE deleted_at IS NULL
			  AND (
				COALESCE(storage_engine_config->>'default_provider', '') IN ('', 'local')
				OR EXISTS (
					SELECT 1 FROM knowledge_bases kb
					WHERE kb.tenant_id = tenants.id
					  AND kb.deleted_at IS NULL
					  AND kb.storage_provider_config->>'provider' = ?
				)
			  )
		`, m.config.Provider, m.config.Provider, string(rawConfig), m.config.Provider)
		if tenantResult.Error != nil {
			return fmt.Errorf("switch tenant storage provider: %w", tenantResult.Error)
		}
		switchedTenants = tenantResult.RowsAffected
		return nil
	})
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return switchedKBs, switchedTenants, ledgerRows, rewrittenColumns, rewrittenRows, nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func scrubPath(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return "path_sha256=" + hex.EncodeToString(digest[:])
}

func hashAndRewind(file *os.File) (string, int64, error) {
	if file == nil {
		return "", 0, errors.New("source file is nil")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	atomic.AddInt64(&r.count, int64(n))
	return n, err
}

var _ io.Reader = (*countingReader)(nil)
