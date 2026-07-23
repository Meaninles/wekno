package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func effectiveAuxiliaryProvider(ctx context.Context, kb *types.KnowledgeBase) string {
	if kb != nil {
		if provider := strings.ToLower(strings.TrimSpace(kb.GetStorageProvider())); provider != "" {
			return provider
		}
	}
	if tenant, ok := types.TenantInfoFromContext(ctx); ok && tenant != nil && tenant.StorageEngineConfig != nil {
		if provider := strings.ToLower(strings.TrimSpace(tenant.StorageEngineConfig.DefaultProvider)); provider != "" {
			return provider
		}
	}
	return strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
}

func auxiliaryPathsFromKnowledge(knowledge *types.Knowledge) ([]string, error) {
	if knowledge == nil {
		return nil, nil
	}
	paths := make([]string, 0)
	if len(knowledge.ProcessingFanout) > 0 {
		plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout)
		if err != nil {
			return nil, fmt.Errorf("decode processing fanout for auxiliary cleanup: %w", err)
		}
		for _, image := range plan.Images {
			paths = append(paths, image.ImageURL)
		}
	}
	// LastFAQImportResult.FailedEntriesURL is a client-facing display URL and
	// may contain bearer query credentials. It is never an object path and must
	// not participate in storage deletion or ownership backfill.
	return uniqueNonEmptyStrings(paths), nil
}

func uniqueNonEmptyStrings(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

type auxiliaryPlannedFileService struct {
	base     interfaces.PlannedFileService
	registry *knowledgeaux.Registry
	owner    knowledgeaux.Object
}

func (s *auxiliaryPlannedFileService) CheckConnectivity(ctx context.Context) error {
	return s.base.CheckConnectivity(ctx)
}

func (s *auxiliaryPlannedFileService) SaveFile(
	context.Context, *multipart.FileHeader, uint64, string,
) (string, error) {
	return "", errors.New("tracked auxiliary service does not accept source-file writes")
}

func (s *auxiliaryPlannedFileService) SaveBytes(
	ctx context.Context,
	data []byte,
	tenantID uint64,
	fileName string,
	temp bool,
) (string, error) {
	if tenantID != s.owner.TenantID {
		return "", knowledgeaux.ErrInvalidObject
	}
	var (
		path string
		err  error
	)
	if s.owner.Kind == knowledgeaux.KindFanoutImage ||
		s.owner.Kind == knowledgeaux.KindCloneImage ||
		s.owner.Kind == knowledgeaux.KindSplitInput ||
		s.owner.Kind == knowledgeaux.KindSplitImage {
		// Image ownership is knowledge-scoped. This makes the physical key
		// independently prove tenant+knowledge ownership during migration and
		// prevents two documents from ever sharing a newly written image path.
		path, err = s.base.ReserveFilePath(tenantID, s.owner.KnowledgeID, fileName)
	} else {
		path, err = s.base.ReserveBytesPath(tenantID, fileName, temp)
	}
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary byte path: %w", err)
	}
	object := s.owner
	object.Path = path
	object, err = s.registry.Reserve(ctx, object, false, s.base)
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary byte ownership: %w", err)
	}
	if err := s.registry.CommitReserved(ctx, object, false, func() error {
		return s.base.CommitBytesAtPath(ctx, data, path)
	}); err != nil {
		return "", errors.Join(err, s.registry.Abort(ctx, object))
	}
	return path, nil
}

// SaveReader persists an already-staged bounded stream under the same durable
// ownership protocol as SaveBytes. Large split parts must use this path so an
// object-store upload never requires a second full in-memory copy.
func (s *auxiliaryPlannedFileService) SaveReader(
	ctx context.Context,
	reader io.ReadSeeker,
	size int64,
	contentType string,
	tenantID uint64,
	fileName string,
) (string, error) {
	if tenantID != s.owner.TenantID || reader == nil || size < 0 {
		return "", knowledgeaux.ErrInvalidObject
	}
	path, err := s.base.ReserveFilePath(tenantID, s.owner.KnowledgeID, fileName)
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary stream path: %w", err)
	}
	object := s.owner
	object.Path = path
	object, err = s.registry.Reserve(ctx, object, false, s.base)
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary stream ownership: %w", err)
	}
	if err := s.registry.CommitReserved(ctx, object, false, func() error {
		return filesvc.CommitReaderAtPath(ctx, s.base, reader, size, contentType, path)
	}); err != nil {
		return "", errors.Join(err, s.registry.Abort(ctx, object))
	}
	return path, nil
}

func (s *auxiliaryPlannedFileService) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return s.base.GetFile(ctx, path)
}

func (s *auxiliaryPlannedFileService) GetFileURL(ctx context.Context, path string) (string, error) {
	return s.base.GetFileURL(ctx, path)
}

func (s *auxiliaryPlannedFileService) DeleteFile(ctx context.Context, path string) error {
	return s.base.DeleteFile(ctx, path)
}

func (s *auxiliaryPlannedFileService) CopyFile(
	ctx context.Context,
	srcPath string,
	tenantID uint64,
	knowledgeID string,
) (string, error) {
	return s.copyFromBoundService(ctx, s.base, srcPath, tenantID, knowledgeID, 0)
}

func (s *auxiliaryPlannedFileService) copyFromBoundService(
	ctx context.Context,
	sourceService interfaces.FileService,
	srcPath string,
	tenantID uint64,
	knowledgeID string,
	sourceSize int64,
) (string, error) {
	if tenantID != s.owner.TenantID || knowledgeID != s.owner.KnowledgeID {
		return "", knowledgeaux.ErrInvalidObject
	}
	sourceProvider, ok := sourceService.(storagebinding.BindingProvider)
	if !ok || sourceProvider == nil {
		return "", fmt.Errorf("copy auxiliary object: source lacks exact storage binding")
	}
	sourceBinding, err := sourceProvider.BindingForPath(srcPath)
	if err != nil {
		return "", fmt.Errorf("copy auxiliary object: derive source binding: %w", err)
	}
	fileName := "copy" + filepath.Ext(srcPath)
	path, err := s.base.ReserveFilePath(tenantID, knowledgeID, fileName)
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary copy path: %w", err)
	}
	object := s.owner
	object.Path = path
	object, err = s.registry.Reserve(ctx, object, false, s.base)
	if err != nil {
		return "", fmt.Errorf("reserve auxiliary copy ownership: %w", err)
	}
	if object.Binding == nil {
		return "", errors.Join(knowledgeaux.ErrBindingMissing, s.registry.Abort(ctx, object))
	}
	sameBinding := object.Binding.Fingerprint == sourceBinding.Fingerprint
	var staged *os.File
	if !sameBinding {
		staged, sourceSize, err = stageBoundSource(ctx, sourceService, srcPath, sourceSize)
		if err != nil {
			return "", errors.Join(fmt.Errorf("stage auxiliary copy source: %w", err), s.registry.Abort(ctx, object))
		}
		defer func() {
			name := staged.Name()
			_ = staged.Close()
			_ = os.Remove(name)
		}()
	}
	if err := s.registry.CommitReserved(ctx, object, false, func() error {
		if sameBinding {
			return s.base.CommitCopyAtPath(ctx, srcPath, path)
		}
		return filesvc.CommitReaderAtPath(
			ctx, s.base, staged, sourceSize, secutils.GetContentTypeByExt(filepath.Ext(srcPath)), path,
		)
	}); err != nil {
		return "", errors.Join(err, s.registry.Abort(ctx, object))
	}
	return path, nil
}

func (s *knowledgeService) plannedAuxiliaryFileService(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	kind string,
) (interfaces.FileService, error) {
	if s.auxObjects == nil {
		return nil, errors.New("planned auxiliary storage: registry is unavailable")
	}
	if kb == nil || knowledge == nil || knowledge.ProcessingGeneration == "" {
		return nil, knowledgeaux.ErrInvalidObject
	}
	base := s.resolveFileService(ctx, kb)
	planned, ok := base.(interfaces.PlannedFileService)
	if !ok || planned == nil {
		return nil, fmt.Errorf("planned auxiliary storage: provider %q does not support crash-safe writes",
			effectiveAuxiliaryProvider(ctx, kb))
	}
	return &auxiliaryPlannedFileService{
		base: planned, registry: s.auxObjects,
		owner: knowledgeaux.Object{
			TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
			KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
			FallbackProvider: effectiveAuxiliaryProvider(ctx, kb), Kind: kind,
		},
	}, nil
}

func (s *knowledgeService) reserveCommitAndAdoptSourceFile(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	file *multipart.FileHeader,
	beforeAdopt func() error,
) error {
	if s.auxObjects == nil {
		return errors.New("create source file: auxiliary registry is unavailable")
	}
	base := s.resolveFileService(ctx, kb)
	planned, ok := base.(interfaces.PlannedFileService)
	if !ok || planned == nil {
		return fmt.Errorf("create source file: provider %q does not support crash-safe writes",
			effectiveAuxiliaryProvider(ctx, kb))
	}
	path, err := planned.ReserveFilePath(knowledge.TenantID, knowledge.ID, knowledge.FileName)
	if err != nil {
		return fmt.Errorf("reserve source file path: %w", err)
	}
	object := knowledgeaux.Object{
		TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
		Path: path, FallbackProvider: effectiveAuxiliaryProvider(ctx, kb), Kind: knowledgeaux.KindSourceFile,
	}
	object, err = s.auxObjects.Reserve(ctx, object, true, planned)
	if err != nil {
		return fmt.Errorf("reserve source file ownership: %w", err)
	}
	if err := s.auxObjects.CommitReserved(ctx, object, true, func() error {
		return planned.CommitFileAtPath(ctx, file, path)
	}); err != nil {
		return errors.Join(err, s.auxObjects.Abort(ctx, object))
	}
	knowledge.FilePath = path
	// The durable document plan needs the provider's final path, but must be
	// prepared before the knowledge INSERT becomes visible. This callback is
	// therefore deliberately placed after provider commit and before the
	// registry's atomic knowledge adoption transaction.
	if beforeAdopt != nil {
		if err := beforeAdopt(); err != nil {
			return errors.Join(fmt.Errorf("prepare source-file adoption: %w", err), s.auxObjects.Abort(ctx, object))
		}
	}
	creator, ok := s.repo.(interfaces.KnowledgeTransactionalCreator)
	if !ok || creator == nil {
		return errors.Join(
			errors.New("adopt committed source file: knowledge repository lacks transactional creation"),
			s.auxObjects.Abort(ctx, object),
		)
	}
	if err := s.auxObjects.AdoptReservedKnowledge(ctx, object, knowledge, creator); err != nil {
		return errors.Join(fmt.Errorf("adopt committed source file: %w", err), s.auxObjects.Abort(ctx, object))
	}
	return nil
}

func (s *knowledgeService) reserveCommitAndAdoptSourceCopy(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	sourceService interfaces.FileService,
	srcPath string,
	sourceSize int64,
) error {
	if s.auxObjects == nil {
		return errors.New("clone source file: auxiliary registry is unavailable")
	}
	base := s.resolveFileService(ctx, kb)
	planned, ok := base.(interfaces.PlannedFileService)
	if !ok || planned == nil {
		return fmt.Errorf("clone source file: provider %q does not support crash-safe writes",
			effectiveAuxiliaryProvider(ctx, kb))
	}
	sourceBindingProvider, ok := sourceService.(storagebinding.BindingProvider)
	if !ok || sourceBindingProvider == nil {
		return fmt.Errorf("clone source file: source service lacks exact storage binding")
	}
	sourceBinding, err := sourceBindingProvider.BindingForPath(srcPath)
	if err != nil {
		return fmt.Errorf("clone source file: derive source binding: %w", err)
	}
	path, err := planned.ReserveFilePath(knowledge.TenantID, knowledge.ID, knowledge.FileName)
	if err != nil {
		return fmt.Errorf("reserve clone source path: %w", err)
	}
	object := knowledgeaux.Object{
		TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
		Path: path, FallbackProvider: effectiveAuxiliaryProvider(ctx, kb), Kind: knowledgeaux.KindCloneSourceFile,
	}
	object, err = s.auxObjects.Reserve(ctx, object, true, planned)
	if err != nil {
		return fmt.Errorf("reserve clone source ownership: %w", err)
	}
	if object.Binding == nil {
		return errors.Join(knowledgeaux.ErrBindingMissing, s.auxObjects.Abort(ctx, object))
	}
	sameBinding := object.Binding.Fingerprint == sourceBinding.Fingerprint
	var staged *os.File
	if !sameBinding {
		staged, sourceSize, err = stageBoundSource(ctx, sourceService, srcPath, sourceSize)
		if err != nil {
			return errors.Join(fmt.Errorf("clone source file: stage source: %w", err), s.auxObjects.Abort(ctx, object))
		}
		defer func() {
			name := staged.Name()
			_ = staged.Close()
			_ = os.Remove(name)
		}()
	}
	if err := s.auxObjects.CommitReserved(ctx, object, true, func() error {
		if sameBinding {
			return planned.CommitCopyAtPath(ctx, srcPath, path)
		}
		return filesvc.CommitReaderAtPath(
			ctx, planned, staged, sourceSize, secutils.GetContentTypeByExt(filepath.Ext(srcPath)), path,
		)
	}); err != nil {
		return errors.Join(err, s.auxObjects.Abort(ctx, object))
	}
	knowledge.FilePath = path
	creator, ok := s.repo.(interfaces.KnowledgeTransactionalCreator)
	if !ok || creator == nil {
		return errors.Join(
			errors.New("adopt committed clone source: knowledge repository lacks transactional creation"),
			s.auxObjects.Abort(ctx, object),
		)
	}
	if err := s.auxObjects.AdoptReservedKnowledge(ctx, object, knowledge, creator); err != nil {
		return errors.Join(fmt.Errorf("adopt committed clone source: %w", err), s.auxObjects.Abort(ctx, object))
	}
	return nil
}

func stageBoundSource(
	ctx context.Context,
	service interfaces.FileService,
	sourcePath string,
	expectedSize int64,
) (*os.File, int64, error) {
	if service == nil {
		return nil, 0, errors.New("source service is unavailable")
	}
	reader, err := service.GetFile(ctx, sourcePath)
	if err != nil {
		return nil, 0, err
	}
	defer reader.Close()
	limit := secutils.GetMaxFileSize()
	if expectedSize > 0 {
		limit = expectedSize
	}
	if limit <= 0 {
		return nil, 0, errors.New("source size limit is invalid")
	}
	staged, err := filesvc.NewBoundCopyStage()
	if err != nil {
		return nil, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			name := staged.Name()
			_ = staged.Close()
			_ = os.Remove(name)
		}
	}()
	written, err := io.Copy(staged, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if written > limit {
		return nil, 0, fmt.Errorf("source exceeds bounded copy limit of %d bytes", limit)
	}
	if expectedSize > 0 && written != expectedSize {
		return nil, 0, fmt.Errorf("source size mismatch: expected %d, read %d", expectedSize, written)
	}
	if err := staged.Sync(); err != nil {
		return nil, 0, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	keep = true
	return staged, written, nil
}

func (s *knowledgeService) registerFAQObject(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	path string,
	reference string,
	kind string,
) error {
	if s.auxObjects == nil {
		return errors.New("register FAQ object: auxiliary object registry is unavailable")
	}
	registered, err := s.auxObjects.Register(ctx, knowledgeaux.Object{
		TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
		Path: path, Reference: reference, FallbackProvider: effectiveAuxiliaryProvider(ctx, kb), Kind: kind,
	}, s.resolveFileService(ctx, kb))
	if err == nil && knowledge.ProcessingGeneration == "" {
		knowledge.ProcessingGeneration = registered.ProcessingGeneration
	}
	return err
}

func (s *knowledgeService) rollbackUnregisteredAuxiliary(
	ctx context.Context,
	kb *types.KnowledgeBase,
	tenantID uint64,
	paths []string,
) error {
	if s.auxObjects == nil {
		return errors.New("rollback auxiliary object: registry is unavailable")
	}
	return s.auxObjects.DeleteUnregistered(
		ctx, tenantID, effectiveAuxiliaryProvider(ctx, kb), uniqueNonEmptyStrings(paths),
	)
}

func (s *knowledgeService) cleanupDerivedKnowledgeAuxiliary(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	legacyPaths []string,
) error {
	if s.auxObjects == nil {
		return errors.New("cleanup knowledge auxiliary objects: registry is unavailable")
	}
	return s.auxObjects.CleanupDerived(
		ctx,
		knowledge.TenantID,
		knowledge.KnowledgeBaseID,
		knowledge.ID,
		effectiveAuxiliaryProvider(ctx, kb),
		uniqueNonEmptyStrings(legacyPaths),
	)
}

func (s *knowledgeService) cleanupDerivedKnowledgeAuxiliaryWithinMoveFence(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	legacyPaths []string,
) error {
	if s.auxObjects == nil {
		return errors.New("cleanup knowledge auxiliary objects: registry is unavailable")
	}
	return s.auxObjects.CleanupDerivedWithinMoveFence(
		ctx,
		knowledge.TenantID,
		knowledge.KnowledgeBaseID,
		knowledge.ID,
		effectiveAuxiliaryProvider(ctx, kb),
		uniqueNonEmptyStrings(legacyPaths),
	)
}

func (s *knowledgeService) cleanupKnowledgeAuxiliaryForDelete(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	legacyPaths []string,
) error {
	if s.auxObjects == nil {
		return errors.New("cleanup knowledge auxiliary objects for delete: registry is unavailable")
	}
	return s.auxObjects.CleanupForDelete(
		ctx,
		knowledge.TenantID,
		knowledge.KnowledgeBaseID,
		knowledge.ID,
		effectiveAuxiliaryProvider(ctx, kb),
		uniqueNonEmptyStrings(legacyPaths),
	)
}

func (s *knowledgeService) auxiliaryFileServiceForPath(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledgeBaseID string,
	knowledgeID string,
	path string,
) (interfaces.FileService, error) {
	if s.auxObjects == nil {
		return nil, errors.New("resolve auxiliary file service: registry is unavailable")
	}
	knowledgeBaseID = strings.TrimSpace(knowledgeBaseID)
	if knowledgeBaseID == "" {
		return nil, errors.New("resolve auxiliary file service: knowledge base identity is unavailable")
	}
	tenantID := types.MustTenantIDFromContext(ctx)
	return s.auxObjects.FileServiceForPath(
		ctx, tenantID, knowledgeBaseID, knowledgeID, path, effectiveAuxiliaryProvider(ctx, kb),
	)
}
