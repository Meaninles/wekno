// Package legacysourcemigration rehomes completed document source files from
// historical OBS keys into the current deployment-private knowledge namespace.
//
// The migration is deliberately row-fenced and online-safe: it verifies the
// legacy bytes, writes and verifies a deterministic destination, then atomically
// creates the knowledge:aux_object ledger and switches knowledges.file_path.
// Historical objects are never deleted by this package.
package legacysourcemigration

import (
	"context"
	"crypto/md5"
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
	"strconv"
	"strings"
	"sync"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

type Mode string

const (
	ModeAudit Mode = "audit"
	ModeApply Mode = "apply"

	migrationLockClass = 824742609
	migrationLockID    = 182736452
	maxFailureDetails  = 100
)

var safeExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,12}$`)

type GroupStats struct {
	Documents          int   `json:"documents"`
	Bytes              int64 `json:"bytes"`
	SourceVerified     int   `json:"source_verified"`
	Uploaded           int   `json:"uploaded"`
	AlreadyVerified    int   `json:"already_verified"`
	LedgerOnly         int   `json:"ledger_only"`
	PathsSwitched      int   `json:"paths_switched"`
	LedgersCreated     int   `json:"ledgers_created"`
	AssignedGeneration int   `json:"assigned_generation"`
	Failed             int   `json:"failed"`
}

type KnowledgeBaseStats struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	Name            string `json:"name"`
	GroupStats
}

type Failure struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	KnowledgeID     string `json:"knowledge_id"`
	FileType        string `json:"file_type"`
	SourcePathHash  string `json:"source_path_hash"`
	Stage           string `json:"stage"`
	Error           string `json:"error"`
}

type Report struct {
	Mode                    Mode                           `json:"mode"`
	Provider                string                         `json:"provider"`
	Bucket                  string                         `json:"bucket"`
	PathPrefix              string                         `json:"path_prefix"`
	CandidateDocuments      int                            `json:"candidate_documents"`
	CandidateBytes          int64                          `json:"candidate_bytes"`
	RehomeCandidates        int                            `json:"rehome_candidates"`
	LedgerOnlyCandidates    int                            `json:"ledger_only_candidates"`
	SourceVerified          int                            `json:"source_verified"`
	SourceVerifiedBytes     int64                          `json:"source_verified_bytes"`
	UploadedObjects         int                            `json:"uploaded_objects"`
	AlreadyVerifiedObjects  int                            `json:"already_verified_objects"`
	PathsSwitched           int                            `json:"paths_switched"`
	LedgersCreated          int                            `json:"ledgers_created"`
	AssignedGenerations     int                            `json:"assigned_generations"`
	FailedDocuments         int                            `json:"failed_documents"`
	RemainingCandidates     int                            `json:"remaining_candidates"`
	SourceObjectsRetained   bool                           `json:"source_objects_retained"`
	ByFileType              map[string]*GroupStats         `json:"by_file_type"`
	ByKnowledgeBase         map[string]*KnowledgeBaseStats `json:"by_knowledge_base"`
	Failures                []Failure                      `json:"failures,omitempty"`
	FailureDetailsTruncated bool                           `json:"failure_details_truncated,omitempty"`
}

type ProgressFunc func(done, total int, verifiedBytes int64)

type Migrator struct {
	db       *gorm.DB
	mode     Mode
	tempDir  string
	target   interfaces.StreamingPrivateObjectFileService
	binding  storagebinding.Binding
	registry *knowledgeaux.Registry
	progress ProgressFunc
}

type candidate struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	KnowledgeBaseName    string
	FileName             string
	FileType             string
	FileSize             int64
	FileHash             string
	FilePath             string
	ProcessingGeneration string
}

type migrationPlan struct {
	Destination string
	LedgerOnly  bool
}

type stagedSource struct {
	File   *os.File
	Size   int64
	MD5    string
	SHA256 string
}

type outcome struct {
	plan               migrationPlan
	sourceVerified     bool
	uploaded           bool
	alreadyVerified    bool
	ledgerCreated      bool
	pathSwitched       bool
	assignedGeneration bool
	stage              string
}

// NewFromEnv builds the one-shot production migrator without probing or
// provisioning storage. Connectivity is checked explicitly at Run time.
func NewFromEnv(db *gorm.DB, mode Mode) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("legacy source migration database is required")
	}
	if db.Dialector.Name() != "postgres" {
		return nil, fmt.Errorf(
			"legacy source migration requires PostgreSQL, got %q",
			db.Dialector.Name(),
		)
	}
	if mode != ModeAudit && mode != ModeApply {
		return nil, fmt.Errorf("unsupported legacy source migration mode %q", mode)
	}
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("STORAGE_TYPE")), "obs") {
		return nil, errors.New("legacy source migration requires global STORAGE_TYPE=obs")
	}

	base, provider, err := filesvc.NewReadOnlyFileServiceFromStorageConfig(
		"obs",
		nil,
		strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR")),
	)
	if err != nil {
		return nil, fmt.Errorf("build global OBS migration service: %w", err)
	}
	if provider != "obs" {
		return nil, fmt.Errorf("global migration service resolved unexpected provider %q", provider)
	}
	target, ok := base.(interfaces.StreamingPrivateObjectFileService)
	if !ok || target == nil {
		return nil, errors.New("global OBS service lacks streaming private-object support")
	}
	bindingProvider, ok := base.(storagebinding.BindingProvider)
	if !ok || bindingProvider == nil {
		return nil, errors.New("global OBS service lacks a durable storage binding")
	}
	probe, err := target.ReservePrivateObjectPath("__legacy_source_migration_probe__")
	if err != nil {
		return nil, fmt.Errorf("reserve migration binding probe: %w", err)
	}
	binding, err := bindingProvider.BindingForPath(probe)
	if err != nil {
		return nil, fmt.Errorf("derive migration target binding: %w", err)
	}
	if binding.Provider != storagebinding.ProviderOBS ||
		binding.ConfigSource != storagebinding.ConfigSourceGlobal ||
		binding.CredentialScope != storagebinding.CredentialScopeGlobal {
		return nil, errors.New("legacy source migration target must be the global OBS profile")
	}
	if binding.PathPrefix == "" ||
		!strings.Contains(binding.PathPrefix, "__weknora_private_knowledge_objects_v1__") {
		return nil, errors.New("legacy source migration refuses a non-private or empty OBS prefix")
	}

	tempDir := strings.TrimSpace(os.Getenv("CUSTOM_LEGACY_SOURCE_MIGRATION_TEMP_DIR"))
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return newMigrator(db, mode, tempDir, target, binding)
}

func newMigrator(
	db *gorm.DB,
	mode Mode,
	tempDir string,
	target interfaces.StreamingPrivateObjectFileService,
	binding storagebinding.Binding,
) (*Migrator, error) {
	if db == nil || target == nil {
		return nil, errors.New("legacy source migration dependencies are incomplete")
	}
	if mode != ModeAudit && mode != ModeApply {
		return nil, fmt.Errorf("unsupported legacy source migration mode %q", mode)
	}
	normalized, err := storagebinding.Normalize(binding)
	if err != nil {
		return nil, fmt.Errorf("normalize legacy source migration binding: %w", err)
	}
	if normalized.Provider != storagebinding.ProviderOBS {
		return nil, errors.New("legacy source migration target must be OBS")
	}
	tempDir = strings.TrimSpace(tempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	info, err := os.Stat(tempDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("legacy source migration temp directory %q is unavailable", tempDir)
	}
	return &Migrator{
		db:       db,
		mode:     mode,
		tempDir:  tempDir,
		target:   target,
		binding:  normalized,
		registry: knowledgeaux.New(db, target),
	}, nil
}

func (m *Migrator) SetProgress(progress ProgressFunc) {
	if m != nil {
		m.progress = progress
	}
}

func (m *Migrator) Run(ctx context.Context) (Report, error) {
	report := Report{
		Mode:                  m.mode,
		Provider:              string(m.binding.Provider),
		Bucket:                m.binding.Bucket,
		PathPrefix:            m.binding.PathPrefix,
		SourceObjectsRetained: true,
		ByFileType:            make(map[string]*GroupStats),
		ByKnowledgeBase:       make(map[string]*KnowledgeBaseStats),
	}
	if err := m.target.CheckConnectivity(ctx); err != nil {
		return report, fmt.Errorf("legacy source migration OBS connectivity: %w", err)
	}
	candidates, err := m.listCandidates(ctx)
	if err != nil {
		return report, err
	}
	report.CandidateDocuments = len(candidates)
	for _, item := range candidates {
		report.CandidateBytes += item.FileSize
	}

	var unlock func()
	if m.mode == ModeApply {
		unlock, err = m.acquireMigrationLock(ctx)
		if err != nil {
			return report, err
		}
		defer unlock()
	}

	for index, item := range candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, processErr := m.processOne(ctx, item)
		m.recordOutcome(&report, item, result, processErr)
		if m.progress != nil {
			m.progress(index+1, len(candidates), report.SourceVerifiedBytes)
		}
	}

	remaining, err := m.countCandidates(ctx)
	if err != nil {
		return report, err
	}
	report.RemainingCandidates = remaining
	if report.FailedDocuments != 0 {
		return report, fmt.Errorf(
			"legacy source migration completed with %d failed document(s)",
			report.FailedDocuments,
		)
	}
	if m.mode == ModeApply && report.RemainingCandidates != 0 {
		return report, fmt.Errorf(
			"legacy source migration left %d eligible document(s) without source ownership",
			report.RemainingCandidates,
		)
	}
	return report, nil
}

func (m *Migrator) processOne(ctx context.Context, item candidate) (out outcome, err error) {
	out.stage = "plan"
	out.plan, err = m.planCandidate(item)
	if err != nil {
		return out, err
	}

	out.stage = "source_verify"
	staged, err := m.stageSource(ctx, item)
	if err != nil {
		return out, err
	}
	defer staged.closeAndRemove()
	out.sourceVerified = true

	if m.mode == ModeAudit {
		if !out.plan.LedgerOnly {
			if err := m.target.VerifyPrivateObject(
				ctx,
				out.plan.Destination,
				staged.Size,
				staged.SHA256,
			); err == nil {
				out.alreadyVerified = true
			}
		}
		return out, nil
	}

	if !out.plan.LedgerOnly {
		out.stage = "target_upload"
		if err := m.target.VerifyPrivateObject(
			ctx,
			out.plan.Destination,
			staged.Size,
			staged.SHA256,
		); err == nil {
			out.alreadyVerified = true
		} else {
			contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(item.FileName)))
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			if err := m.target.CommitPrivateObjectStreamAtPath(
				ctx,
				staged.File,
				staged.Size,
				out.plan.Destination,
				contentType,
				staged.SHA256,
			); err != nil {
				return out, fmt.Errorf("upload deterministic destination: %w", err)
			}
			out.uploaded = true
			if err := staged.verifyUnchanged(); err != nil {
				return out, err
			}
			if err := m.target.VerifyPrivateObject(
				ctx,
				out.plan.Destination,
				staged.Size,
				staged.SHA256,
			); err != nil {
				return out, fmt.Errorf("verify deterministic destination: %w", err)
			}
		}
	}

	out.stage = "atomic_cutover"
	adopted, err := m.registry.AdoptMigratedSource(
		ctx,
		item.FilePath,
		item.ProcessingGeneration,
		knowledgeaux.Object{
			TenantID:             item.TenantID,
			KnowledgeBaseID:      item.KnowledgeBaseID,
			KnowledgeID:          item.ID,
			ProcessingGeneration: item.ProcessingGeneration,
			Path:                 out.plan.Destination,
			FallbackProvider:     "obs",
			Kind:                 knowledgeaux.KindSourceFile,
		},
		m.target,
	)
	if err != nil {
		return out, fmt.Errorf("atomically adopt migrated source: %w", err)
	}
	out.ledgerCreated = true
	out.pathSwitched = out.plan.Destination != item.FilePath
	out.assignedGeneration =
		item.ProcessingGeneration == "" && adopted.ProcessingGeneration != ""

	out.stage = "cutover_verify"
	if err := m.verifyCutover(ctx, item, adopted); err != nil {
		return out, err
	}
	return out, nil
}

func (m *Migrator) planCandidate(item candidate) (migrationPlan, error) {
	if _, err := plannedfile.ParseBucketPath(
		item.FilePath,
		"obs",
		m.binding.Bucket,
		"",
	); err != nil {
		return migrationPlan{}, fmt.Errorf("legacy OBS source is outside the configured bucket: %w", err)
	}

	bindingProvider, ok := m.target.(storagebinding.BindingProvider)
	if !ok || bindingProvider == nil {
		return migrationPlan{}, errors.New("migration target lost its binding provider")
	}
	if binding, err := bindingProvider.BindingForPath(item.FilePath); err == nil {
		if validOwnerPath(item, item.FilePath, binding) {
			return migrationPlan{Destination: item.FilePath, LedgerOnly: true}, nil
		}
	}

	digest := sha256.Sum256([]byte(strings.TrimSpace(item.FilePath)))
	objectName := "legacy-source-" + hex.EncodeToString(digest[:])
	ext := strings.ToLower(filepath.Ext(item.FileName))
	if safeExtension.MatchString(ext) {
		objectName += ext
	}
	destination, err := m.target.ReservePrivateObjectPath(
		strconv.FormatUint(item.TenantID, 10),
		item.ID,
		objectName,
	)
	if err != nil {
		return migrationPlan{}, fmt.Errorf("reserve deterministic destination: %w", err)
	}
	binding, err := bindingProvider.BindingForPath(destination)
	if err != nil {
		return migrationPlan{}, fmt.Errorf("bind deterministic destination: %w", err)
	}
	if !validOwnerPath(item, destination, binding) {
		return migrationPlan{}, errors.New("deterministic destination escaped the knowledge owner namespace")
	}
	return migrationPlan{Destination: destination}, nil
}

func validOwnerPath(item candidate, filePath string, binding storagebinding.Binding) bool {
	key, err := plannedfile.ParseBucketPath(
		filePath,
		string(binding.Provider),
		binding.Bucket,
		binding.PathPrefix,
	)
	if err != nil {
		return false
	}
	relative := strings.TrimPrefix(key, strings.Trim(binding.PathPrefix, "/")+"/")
	parts := strings.Split(relative, "/")
	return len(parts) == 3 &&
		parts[0] == strconv.FormatUint(item.TenantID, 10) &&
		parts[1] == item.ID &&
		plannedfile.ValidateSegment("source object", parts[2]) == nil
}

func (m *Migrator) stageSource(ctx context.Context, item candidate) (*stagedSource, error) {
	source, err := m.target.GetFile(ctx, item.FilePath)
	if err != nil {
		return nil, fmt.Errorf("open legacy source object: %w", err)
	}
	defer source.Close()

	file, err := os.CreateTemp(m.tempDir, "weknora-legacy-source-*")
	if err != nil {
		return nil, fmt.Errorf("create bounded migration staging file: %w", err)
	}
	staged := &stagedSource{File: file}
	cleanup := true
	defer func() {
		if cleanup {
			staged.closeAndRemove()
		}
	}()

	md5Hash := md5.New()
	shaHash := sha256.New()
	size, err := io.Copy(file, io.TeeReader(source, io.MultiWriter(md5Hash, shaHash)))
	if err != nil {
		return nil, fmt.Errorf("stream legacy source object: %w", err)
	}
	staged.Size = size
	staged.MD5 = hex.EncodeToString(md5Hash.Sum(nil))
	staged.SHA256 = hex.EncodeToString(shaHash.Sum(nil))
	if item.FileSize <= 0 || staged.Size != item.FileSize {
		return nil, fmt.Errorf(
			"legacy source size mismatch: database=%d object=%d",
			item.FileSize,
			staged.Size,
		)
	}
	if !isMD5(item.FileHash) || !strings.EqualFold(item.FileHash, staged.MD5) {
		return nil, errors.New("legacy source MD5 does not match knowledges.file_hash")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind staged legacy source: %w", err)
	}
	cleanup = false
	return staged, nil
}

func (s *stagedSource) verifyUnchanged() error {
	if s == nil || s.File == nil {
		return errors.New("staged legacy source is unavailable")
	}
	if _, err := s.File.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind staged source after upload: %w", err)
	}
	md5Hash := md5.New()
	shaHash := sha256.New()
	size, err := io.Copy(io.MultiWriter(md5Hash, shaHash), s.File)
	if err != nil {
		return fmt.Errorf("rehash staged source after upload: %w", err)
	}
	if size != s.Size ||
		hex.EncodeToString(md5Hash.Sum(nil)) != s.MD5 ||
		hex.EncodeToString(shaHash.Sum(nil)) != s.SHA256 {
		return errors.New("staged source changed during target upload")
	}
	_, err = s.File.Seek(0, io.SeekStart)
	return err
}

func (s *stagedSource) closeAndRemove() {
	if s == nil || s.File == nil {
		return
	}
	name := s.File.Name()
	_ = s.File.Close()
	_ = os.Remove(name)
	s.File = nil
}

func isMD5(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != md5.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (m *Migrator) listCandidates(ctx context.Context) ([]candidate, error) {
	knowledgeIDExpr := "p.payload->>'knowledge_id'"
	kindExpr := "p.payload->>'kind'"
	if m.db.Dialector.Name() == "sqlite" {
		knowledgeIDExpr = "json_extract(p.payload, '$.knowledge_id')"
		kindExpr = "json_extract(p.payload, '$.kind')"
	}
	query := fmt.Sprintf(`
		SELECT
			k.id,
			k.tenant_id,
			k.knowledge_base_id,
			kb.name AS knowledge_base_name,
			k.file_name,
			LOWER(COALESCE(NULLIF(k.file_type, ''), 'unknown')) AS file_type,
			k.file_size,
			k.file_hash,
			k.file_path,
			k.processing_generation
		FROM knowledges k
		JOIN knowledge_bases kb
		  ON kb.id = k.knowledge_base_id
		 AND kb.tenant_id = k.tenant_id
		 AND kb.deleted_at IS NULL
		JOIN tenants t
		  ON t.id = k.tenant_id
		 AND t.deleted_at IS NULL
		WHERE k.deleted_at IS NULL
		  AND k.parse_status = ?
		  AND k.type <> ?
		  AND k.file_path LIKE 'obs://%%'
		  AND NOT EXISTS (
			SELECT 1
			FROM task_pending_ops p
			WHERE p.tenant_id = k.tenant_id
			  AND p.task_type = ?
			  AND p.scope = ?
			  AND p.scope_id = k.knowledge_base_id
			  AND p.op = 'owned'
			  AND %s = k.id
			  AND %s IN (?, ?)
		  )
		ORDER BY k.tenant_id, k.knowledge_base_id, k.id
	`, knowledgeIDExpr, kindExpr)
	var rows []candidate
	if err := m.db.WithContext(ctx).Raw(
		query,
		types.ParseStatusCompleted,
		types.KnowledgeTypeManual,
		knowledgeaux.TaskType,
		types.TaskScopeKnowledgeBase,
		knowledgeaux.KindSourceFile,
		knowledgeaux.KindCloneSourceFile,
	).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list legacy OBS source candidates: %w", err)
	}
	return rows, nil
}

func (m *Migrator) countCandidates(ctx context.Context) (int, error) {
	rows, err := m.listCandidates(ctx)
	return len(rows), err
}

func (m *Migrator) verifyCutover(
	ctx context.Context,
	item candidate,
	adopted knowledgeaux.Object,
) error {
	var owner struct {
		FilePath             string
		ProcessingGeneration string
		ParseStatus          string
	}
	if err := m.db.WithContext(ctx).
		Table("knowledges").
		Select("file_path", "processing_generation", "parse_status").
		Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL",
			item.TenantID,
			item.KnowledgeBaseID,
			item.ID,
		).
		Take(&owner).Error; err != nil {
		return fmt.Errorf("verify cutover knowledge row: %w", err)
	}
	if strings.TrimSpace(owner.FilePath) != adopted.Path ||
		strings.TrimSpace(owner.ProcessingGeneration) != adopted.ProcessingGeneration ||
		owner.ParseStatus != types.ParseStatusCompleted {
		return errors.New("verify cutover knowledge row: migrated identity is not durable")
	}

	var rows []*types.TaskPendingOp
	if err := m.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = 'owned'",
			item.TenantID,
			knowledgeaux.TaskType,
			types.TaskScopeKnowledgeBase,
			item.KnowledgeBaseID,
		).
		Find(&rows).Error; err != nil {
		return fmt.Errorf("verify cutover source ledger: %w", err)
	}
	matches := 0
	for _, row := range rows {
		var object knowledgeaux.Object
		if err := json.Unmarshal(row.Payload, &object); err != nil {
			continue
		}
		if object.KnowledgeID != item.ID ||
			object.Kind != knowledgeaux.KindSourceFile ||
			strings.TrimSpace(object.Path) != adopted.Path {
			continue
		}
		if object.TenantID != item.TenantID ||
			object.KnowledgeBaseID != item.KnowledgeBaseID ||
			object.ProcessingGeneration != adopted.ProcessingGeneration ||
			object.Binding == nil {
			return errors.New("verify cutover source ledger: persisted identity differs")
		}
		normalized, err := storagebinding.Normalize(*object.Binding)
		if err != nil || adopted.Binding == nil ||
			normalized.Fingerprint != adopted.Binding.Fingerprint {
			return errors.New("verify cutover source ledger: storage binding differs")
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf(
			"verify cutover source ledger: expected one exact ownership row, found %d",
			matches,
		)
	}
	return nil
}

func (m *Migrator) groupStats(report *Report, item candidate) (*GroupStats, *KnowledgeBaseStats) {
	fileType := normalizeFileType(item)
	byType := report.ByFileType[fileType]
	if byType == nil {
		byType = &GroupStats{}
		report.ByFileType[fileType] = byType
	}
	key := strconv.FormatUint(item.TenantID, 10) + "/" + item.KnowledgeBaseID
	byKB := report.ByKnowledgeBase[key]
	if byKB == nil {
		byKB = &KnowledgeBaseStats{
			TenantID:        item.TenantID,
			KnowledgeBaseID: item.KnowledgeBaseID,
			Name:            item.KnowledgeBaseName,
		}
		report.ByKnowledgeBase[key] = byKB
	}
	return byType, byKB
}

func (m *Migrator) recordOutcome(
	report *Report,
	item candidate,
	result outcome,
	processErr error,
) {
	byType, byKB := m.groupStats(report, item)
	// Candidate totals are incremented here exactly once per processed row.
	byType.Documents++
	byType.Bytes += item.FileSize
	byKB.Documents++
	byKB.Bytes += item.FileSize

	if result.plan.LedgerOnly {
		report.LedgerOnlyCandidates++
		byType.LedgerOnly++
		byKB.LedgerOnly++
	} else if result.plan.Destination != "" {
		report.RehomeCandidates++
	}
	if result.sourceVerified {
		report.SourceVerified++
		report.SourceVerifiedBytes += item.FileSize
		byType.SourceVerified++
		byKB.SourceVerified++
	}
	if result.uploaded {
		report.UploadedObjects++
		byType.Uploaded++
		byKB.Uploaded++
	}
	if result.alreadyVerified {
		report.AlreadyVerifiedObjects++
		byType.AlreadyVerified++
		byKB.AlreadyVerified++
	}
	if result.pathSwitched {
		report.PathsSwitched++
		byType.PathsSwitched++
		byKB.PathsSwitched++
	}
	if result.ledgerCreated {
		report.LedgersCreated++
		byType.LedgersCreated++
		byKB.LedgersCreated++
	}
	if result.assignedGeneration {
		report.AssignedGenerations++
		byType.AssignedGeneration++
		byKB.AssignedGeneration++
	}
	if processErr == nil {
		return
	}
	report.FailedDocuments++
	byType.Failed++
	byKB.Failed++
	if len(report.Failures) >= maxFailureDetails {
		report.FailureDetailsTruncated = true
		return
	}
	report.Failures = append(report.Failures, Failure{
		TenantID:        item.TenantID,
		KnowledgeBaseID: item.KnowledgeBaseID,
		KnowledgeID:     item.ID,
		FileType:        normalizeFileType(item),
		SourcePathHash:  pathHash(item.FilePath),
		Stage:           result.stage,
		Error:           scrubError(processErr, item),
	})
}

func normalizeFileType(item candidate) string {
	value := strings.ToLower(strings.TrimSpace(item.FileType))
	if value == "" || value == "unknown" {
		value = strings.TrimPrefix(strings.ToLower(filepath.Ext(item.FileName)), ".")
	}
	if value == "" {
		return "unknown"
	}
	return value
}

func pathHash(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(digest[:])
}

func scrubError(err error, item candidate) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if item.FilePath != "" {
		message = strings.ReplaceAll(
			message,
			item.FilePath,
			"<source:"+pathHash(item.FilePath)+">",
		)
	}
	if item.FileName != "" {
		message = strings.ReplaceAll(message, item.FileName, "<file-name-redacted>")
	}
	return message
}

func (m *Migrator) acquireMigrationLock(ctx context.Context) (func(), error) {
	if m.db.Dialector.Name() != "postgres" {
		return func() {}, nil
	}
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
		`SELECT pg_try_advisory_lock($1, $2)`,
		migrationLockClass,
		migrationLockID,
	).Scan(&locked); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !locked {
		_ = conn.Close()
		return nil, errors.New("another legacy source migration owns the PostgreSQL lock")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(
				releaseCtx,
				`SELECT pg_advisory_unlock($1, $2)`,
				migrationLockClass,
				migrationLockID,
			)
			_ = conn.Close()
		})
	}, nil
}
