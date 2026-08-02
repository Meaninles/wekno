package knowledgefolders

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	knowledgeworkflowfilter "github.com/Tencent/WeKnora/internal/custom/modules/knowledgeworkflowfilter"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db                   *gorm.DB
	knowledgeService     interfaces.KnowledgeService
	knowledgeBaseService interfaces.KnowledgeBaseService
	kbShareService       interfaces.KBShareService
}

func NewService(
	db *gorm.DB,
	knowledgeService interfaces.KnowledgeService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	kbShareService interfaces.KBShareService,
) *Service {
	return &Service{
		db:                   db,
		knowledgeService:     knowledgeService,
		knowledgeBaseService: knowledgeBaseService,
		kbShareService:       kbShareService,
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("knowledge folders: database is unavailable")
	}
	db := s.db.WithContext(ctx)
	if err := db.AutoMigrate(&Folder{}, &FolderClosure{}, &FolderStats{}); err != nil {
		return fmt.Errorf("migrate knowledge folder tables: %w", err)
	}
	if !db.Migrator().HasColumn(&types.Knowledge{}, "FolderID") {
		if err := db.Migrator().AddColumn(&types.Knowledge{}, "FolderID"); err != nil {
			return fmt.Errorf("add knowledges.folder_id: %w", err)
		}
	}
	if err := db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_knowledges_folder_scope ON knowledges (tenant_id, knowledge_base_id, folder_id, deleted_at, created_at)",
	).Error; err != nil {
		return fmt.Errorf("create knowledges folder index: %w", err)
	}
	if err := s.ensureStatsTriggers(ctx); err != nil {
		return err
	}
	if err := s.ReconcileAll(ctx); err != nil {
		return fmt.Errorf("reconcile knowledge folder statistics: %w", err)
	}
	return nil
}

func tenantIDFromContext(ctx context.Context) (uint64, error) {
	tenantID, ok := ctx.Value(types.TenantIDContextKey).(uint64)
	if !ok || tenantID == 0 {
		return 0, errors.New("knowledge folders: tenant id is missing")
	}
	return tenantID, nil
}

func userIDFromContext(ctx context.Context) string {
	userID, _ := ctx.Value(types.UserIDContextKey).(string)
	return strings.TrimSpace(userID)
}

func normalizeFolderName(raw string) (name, normalized string, err error) {
	name = strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`+"\x00") ||
		!utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxFolderNameRunes {
		return "", "", ErrFolderNameInvalid
	}
	return name, strings.ToLower(name), nil
}

func normalizePage(page, pageSize int) (int, int, error) {
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	if page < 1 || pageSize < 1 {
		return 0, 0, ErrInvalidPage
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize, nil
}

func pathFor(parentPath, name string) string {
	if strings.TrimSpace(parentPath) == "" {
		return name
	}
	return parentPath + "/" + name
}

func (s *Service) scopedFolder(tx *gorm.DB, tenantID uint64, kbID, folderID string) *gorm.DB {
	return tx.Where(
		"tenant_id = ? AND knowledge_base_id = ? AND id = ?",
		tenantID, strings.TrimSpace(kbID), strings.TrimSpace(folderID),
	)
}

func (s *Service) findFolder(
	tx *gorm.DB,
	tenantID uint64,
	kbID, folderID string,
	lock bool,
) (*Folder, error) {
	if strings.TrimSpace(folderID) == "" {
		return nil, nil
	}
	query := s.scopedFolder(tx, tenantID, kbID, folderID)
	if lock && tx.Dialector != nil && tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var folder Folder
	if err := query.First(&folder).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrFolderNotFound
		}
		return nil, err
	}
	return &folder, nil
}

func (s *Service) CreateFolder(
	ctx context.Context,
	kbID string,
	req CreateFolderRequest,
) (*FolderView, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	name, normalized, err := normalizeFolderName(req.Name)
	if err != nil {
		return nil, err
	}
	var created Folder
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		parent, err := s.findFolder(tx, tenantID, kbID, req.ParentID, true)
		if err != nil {
			return err
		}
		depth := 1
		parentPath := ""
		if parent != nil {
			depth = parent.Depth + 1
			parentPath = parent.Path
		}
		if depth > MaxFolderDepth {
			return ErrFolderDepth
		}
		now := time.Now()
		created = Folder{
			ID:              uuid.NewString(),
			TenantID:        tenantID,
			KnowledgeBaseID: kbID,
			ParentID:        strings.TrimSpace(req.ParentID),
			Name:            name,
			NormalizedName:  normalized,
			Description:     strings.TrimSpace(req.Description),
			Path:            pathFor(parentPath, name),
			Depth:           depth,
			SortOrder:       req.SortOrder,
			CreatedBy:       userIDFromContext(ctx),
			UpdatedBy:       userIDFromContext(ctx),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := tx.Create(&created).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrFolderNameExists
			}
			return err
		}
		stats := FolderStats{
			FolderID:        created.ID,
			TenantID:        tenantID,
			KnowledgeBaseID: kbID,
			UpdatedAt:       now,
		}
		if err := tx.Create(&stats).Error; err != nil {
			return err
		}
		closures := []FolderClosure{{
			TenantID: tenantID, KnowledgeBaseID: kbID,
			AncestorID: created.ID, DescendantID: created.ID, Depth: 0,
		}}
		if parent != nil {
			var ancestors []FolderClosure
			if err := tx.Where(
				"tenant_id = ? AND knowledge_base_id = ? AND descendant_id = ?",
				tenantID, kbID, parent.ID,
			).Find(&ancestors).Error; err != nil {
				return err
			}
			for _, ancestor := range ancestors {
				closures = append(closures, FolderClosure{
					TenantID: tenantID, KnowledgeBaseID: kbID,
					AncestorID: ancestor.AncestorID, DescendantID: created.ID,
					Depth: ancestor.Depth + 1,
				})
			}
			if err := tx.Model(&FolderStats{}).
				Where("folder_id = ?", parent.ID).
				Updates(map[string]any{
					"direct_child_folder_count": gorm.Expr("direct_child_folder_count + 1"),
					"updated_at":                now,
				}).Error; err != nil {
				return err
			}
		}
		return tx.Create(&closures).Error
	})
	if err != nil {
		return nil, err
	}
	return s.GetFolder(ctx, kbID, created.ID)
}

func (s *Service) EnsurePath(
	ctx context.Context,
	kbID, parentID string,
	segments []string,
) (string, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return "", err
	}
	currentParent := strings.TrimSpace(parentID)
	if currentParent != "" {
		if _, err := s.GetFolder(ctx, kbID, currentParent); err != nil {
			return "", err
		}
	}
	for _, raw := range segments {
		name, normalized, err := normalizeFolderName(raw)
		if err != nil {
			return "", err
		}
		created, err := s.CreateFolder(ctx, kbID, CreateFolderRequest{
			ParentID: currentParent,
			Name:     name,
		})
		if err == nil {
			currentParent = created.ID
			continue
		}
		if !errors.Is(err, ErrFolderNameExists) {
			return "", err
		}
		var existing Folder
		if findErr := s.db.WithContext(ctx).Where(
			"tenant_id = ? AND knowledge_base_id = ? AND parent_id = ? AND normalized_name = ?",
			tenantID, kbID, currentParent, normalized,
		).First(&existing).Error; findErr != nil {
			return "", findErr
		}
		currentParent = existing.ID
	}
	return currentParent, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") || strings.Contains(message, "duplicate")
}

func (s *Service) GetFolder(ctx context.Context, kbID, folderID string) (*FolderView, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	folder, err := s.findFolder(s.db.WithContext(ctx), tenantID, kbID, folderID, false)
	if err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, ErrFolderNotFound
	}
	var stats FolderStats
	if err := s.db.WithContext(ctx).Where("folder_id = ?", folder.ID).First(&stats).Error; err != nil {
		return nil, err
	}
	return &FolderView{Folder: *folder, Stats: stats}, nil
}

func (s *Service) ListNodes(
	ctx context.Context,
	kbID, parentID string,
	page, pageSize int,
	filter types.KnowledgeListFilter,
) (*NodePage, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	page, pageSize, err = normalizePage(page, pageSize)
	if err != nil {
		return nil, err
	}
	var current *FolderView
	if strings.TrimSpace(parentID) != "" {
		current, err = s.GetFolder(ctx, kbID, parentID)
		if err != nil {
			return nil, err
		}
	}
	folderQuery := s.db.WithContext(ctx).Model(&Folder{}).
		Where("tenant_id = ? AND knowledge_base_id = ? AND parent_id = ?", tenantID, kbID, strings.TrimSpace(parentID))
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		folderQuery = folderQuery.Where("LOWER(name) LIKE ? ESCAPE '\\'", likePattern(keyword))
	}
	var folderTotal int64
	if err := folderQuery.Count(&folderTotal).Error; err != nil {
		return nil, err
	}
	documentQuery := s.applyDocumentFilter(
		s.db.WithContext(ctx).Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND folder_id = ?", tenantID, kbID, strings.TrimSpace(parentID)),
		filter,
	)
	var documentTotal int64
	if err := documentQuery.Count(&documentTotal).Error; err != nil {
		return nil, err
	}
	offset := (page - 1) * pageSize
	nodes := make([]Node, 0, pageSize)
	if int64(offset) < folderTotal {
		folderLimit := pageSize
		var folders []Folder
		if err := folderQuery.
			Order("sort_order ASC, normalized_name ASC, id ASC").
			Offset(offset).Limit(folderLimit).Find(&folders).Error; err != nil {
			return nil, err
		}
		views, err := s.folderViews(ctx, folders)
		if err != nil {
			return nil, err
		}
		for i := range views {
			view := views[i]
			nodes = append(nodes, Node{NodeType: "folder", Folder: &view})
		}
	}
	remaining := pageSize - len(nodes)
	if remaining > 0 {
		docOffset := offset - int(folderTotal)
		if docOffset < 0 {
			docOffset = 0
		}
		var documents []*types.Knowledge
		if err := documentQuery.
			Order("created_at DESC, id ASC").
			Offset(docOffset).Limit(remaining).Find(&documents).Error; err != nil {
			return nil, err
		}
		s.attachDocumentTags(ctx, documents)
		for _, document := range documents {
			nodes = append(nodes, Node{NodeType: "document", Document: document})
		}
	}
	breadcrumbs, err := s.breadcrumbs(ctx, tenantID, kbID, parentID)
	if err != nil {
		return nil, err
	}
	return &NodePage{
		Data: nodes, Total: folderTotal + documentTotal,
		Page: page, PageSize: pageSize, Current: current, Breadcrumbs: breadcrumbs,
	}, nil
}

func likePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + strings.ToLower(replacer.Replace(strings.TrimSpace(value))) + "%"
}

func (s *Service) applyDocumentFilter(query *gorm.DB, filter types.KnowledgeListFilter) *gorm.DB {
	if len(filter.TagIDs) > 0 {
		query = query.Where(
			"knowledges.id IN (SELECT knowledge_id FROM knowledge_tag_relations WHERE tag_id IN (?))",
			filter.TagIDs,
		)
	}
	if filter.Keyword != "" {
		pattern := likePattern(filter.Keyword)
		query = query.Where(
			"(LOWER(knowledges.file_name) LIKE ? ESCAPE '\\' OR LOWER(knowledges.title) LIKE ? ESCAPE '\\')",
			pattern, pattern,
		)
	}
	applyType := func(q *gorm.DB, value string) *gorm.DB {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
			return q
		case "manual", "url":
			return q.Where("knowledges.type = ?", strings.ToLower(strings.TrimSpace(value)))
		default:
			return q.Where("LOWER(knowledges.file_type) = ?", strings.ToLower(strings.TrimSpace(value)))
		}
	}
	query = applyType(query, filter.FileType)
	if filter.Source != "" {
		switch filter.Source {
		case "manual", "url":
			query = query.Where("knowledges.type = ?", filter.Source)
		default:
			query = query.Where("knowledges.channel = ?", filter.Source)
		}
	}
	if filter.ParseStatus != "" {
		query = query.Where("knowledges.parse_status = ?", filter.ParseStatus)
	}
	query = knowledgeworkflowfilter.Apply(query, filter.WorkflowStatus)
	if !filter.UpdatedFrom.IsZero() {
		query = query.Where("knowledges.updated_at >= ?", filter.UpdatedFrom)
	}
	if !filter.UpdatedTo.IsZero() {
		query = query.Where("knowledges.updated_at <= ?", filter.UpdatedTo)
	}
	return query
}

func (s *Service) folderViews(ctx context.Context, folders []Folder) ([]FolderView, error) {
	if len(folders) == 0 {
		return []FolderView{}, nil
	}
	ids := make([]string, 0, len(folders))
	for _, folder := range folders {
		ids = append(ids, folder.ID)
	}
	var statsRows []FolderStats
	if err := s.db.WithContext(ctx).Where("folder_id IN ?", ids).Find(&statsRows).Error; err != nil {
		return nil, err
	}
	statsByID := make(map[string]FolderStats, len(statsRows))
	for _, stats := range statsRows {
		statsByID[stats.FolderID] = stats
	}
	views := make([]FolderView, 0, len(folders))
	for _, folder := range folders {
		views = append(views, FolderView{Folder: folder, Stats: statsByID[folder.ID]})
	}
	return views, nil
}

func (s *Service) attachDocumentTags(ctx context.Context, documents []*types.Knowledge) {
	if s.knowledgeService == nil || len(documents) == 0 {
		return
	}
	ids := make([]string, 0, len(documents))
	for _, document := range documents {
		ids = append(ids, document.ID)
	}
	tagMap, err := s.knowledgeService.GetKnowledgeTags(ctx, ids)
	if err != nil {
		return
	}
	for _, document := range documents {
		document.Tags = tagMap[document.ID]
	}
}

func (s *Service) breadcrumbs(
	ctx context.Context,
	tenantID uint64,
	kbID, folderID string,
) ([]Breadcrumb, error) {
	if strings.TrimSpace(folderID) == "" {
		return []Breadcrumb{}, nil
	}
	var rows []struct {
		ID    string
		Name  string
		Depth int
	}
	if err := s.db.WithContext(ctx).
		Table("custom_knowledge_folder_closure AS c").
		Select("f.id, f.name, c.depth").
		Joins("JOIN custom_knowledge_folders AS f ON f.id = c.ancestor_id").
		Where(
			"c.tenant_id = ? AND c.knowledge_base_id = ? AND c.descendant_id = ?",
			tenantID, kbID, folderID,
		).
		Order("c.depth DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	breadcrumbs := make([]Breadcrumb, 0, len(rows))
	for _, row := range rows {
		breadcrumbs = append(breadcrumbs, Breadcrumb{ID: row.ID, Name: row.Name})
	}
	return breadcrumbs, nil
}

func (s *Service) UpdateFolder(
	ctx context.Context,
	kbID, folderID string,
	req UpdateFolderRequest,
) (*FolderView, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		folder, err := s.findFolder(tx, tenantID, kbID, folderID, true)
		if err != nil {
			return err
		}
		if req.ParentID != nil && strings.TrimSpace(*req.ParentID) != folder.ParentID {
			if err := s.moveFolderTx(tx, tenantID, kbID, folder, strings.TrimSpace(*req.ParentID)); err != nil {
				return err
			}
			folder.ParentID = strings.TrimSpace(*req.ParentID)
			if refreshed, err := s.findFolder(tx, tenantID, kbID, folderID, true); err == nil {
				folder = refreshed
			} else {
				return err
			}
		}
		updates := map[string]any{"updated_by": userIDFromContext(ctx), "updated_at": time.Now()}
		if req.Description != nil {
			updates["description"] = strings.TrimSpace(*req.Description)
		}
		if req.SortOrder != nil {
			updates["sort_order"] = *req.SortOrder
		}
		if req.Name != nil {
			name, normalized, err := normalizeFolderName(*req.Name)
			if err != nil {
				return err
			}
			if normalized != folder.NormalizedName {
				updates["name"] = name
				updates["normalized_name"] = normalized
				newPath := pathFor(parentPathFromFolderPath(folder.Path), name)
				if err := s.replaceSubtreePathTx(tx, tenantID, kbID, folder.ID, folder.Path, newPath, 0); err != nil {
					return err
				}
				updates["path"] = newPath
			}
		}
		if err := s.scopedFolder(tx.Model(&Folder{}), tenantID, kbID, folderID).Updates(updates).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrFolderNameExists
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetFolder(ctx, kbID, folderID)
}

func parentPathFromFolderPath(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[:index]
	}
	return ""
}

func (s *Service) replaceSubtreePathTx(
	tx *gorm.DB,
	tenantID uint64,
	kbID, rootID, oldRootPath, newRootPath string,
	depthDelta int,
) error {
	var descendants []Folder
	if err := tx.
		Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id IN (?)",
			tenantID, kbID,
			tx.Model(&FolderClosure{}).Select("descendant_id").
				Where("tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ?", tenantID, kbID, rootID),
		).
		Find(&descendants).Error; err != nil {
		return err
	}
	for _, descendant := range descendants {
		newPath := newRootPath
		if descendant.ID != rootID {
			suffix := strings.TrimPrefix(descendant.Path, oldRootPath)
			suffix = strings.TrimPrefix(suffix, "/")
			newPath = pathFor(newRootPath, suffix)
		}
		if descendant.ID == rootID {
			continue
		}
		if err := tx.Model(&Folder{}).Where("id = ?", descendant.ID).Updates(map[string]any{
			"path":       newPath,
			"depth":      descendant.Depth + depthDelta,
			"updated_at": time.Now(),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

type closureDistance struct {
	FolderID string
	Depth    int
}

func (s *Service) moveFolderTx(
	tx *gorm.DB,
	tenantID uint64,
	kbID string,
	folder *Folder,
	newParentID string,
) error {
	if folder == nil {
		return ErrFolderNotFound
	}
	if newParentID == folder.ID {
		return ErrFolderCycle
	}
	newParent, err := s.findFolder(tx, tenantID, kbID, newParentID, true)
	if err != nil {
		return err
	}
	var subtreeCount int64
	if newParent != nil {
		if err := tx.Model(&FolderClosure{}).Where(
			"tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ? AND descendant_id = ?",
			tenantID, kbID, folder.ID, newParent.ID,
		).Count(&subtreeCount).Error; err != nil {
			return err
		}
		if subtreeCount > 0 {
			return ErrFolderCycle
		}
	}
	newDepth := 1
	newParentPath := ""
	if newParent != nil {
		newDepth = newParent.Depth + 1
		newParentPath = newParent.Path
	}
	var maxRelativeDepth int
	if err := tx.Model(&FolderClosure{}).
		Select("COALESCE(MAX(depth), 0)").
		Where("tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ?", tenantID, kbID, folder.ID).
		Scan(&maxRelativeDepth).Error; err != nil {
		return err
	}
	if newDepth+maxRelativeDepth > MaxFolderDepth {
		return ErrFolderDepth
	}
	var oldAncestors []FolderClosure
	if err := tx.Where(
		"tenant_id = ? AND knowledge_base_id = ? AND descendant_id = ? AND depth > 0",
		tenantID, kbID, folder.ID,
	).Find(&oldAncestors).Error; err != nil {
		return err
	}
	var newAncestors []FolderClosure
	if newParent != nil {
		if err := tx.Where(
			"tenant_id = ? AND knowledge_base_id = ? AND descendant_id = ?",
			tenantID, kbID, newParent.ID,
		).Find(&newAncestors).Error; err != nil {
			return err
		}
	}
	var subtree []FolderClosure
	if err := tx.Where(
		"tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ?",
		tenantID, kbID, folder.ID,
	).Find(&subtree).Error; err != nil {
		return err
	}
	var stats FolderStats
	if err := tx.Where("folder_id = ?", folder.ID).First(&stats).Error; err != nil {
		return err
	}
	if err := s.transferAggregateTx(tx, oldAncestors, stats, -1); err != nil {
		return err
	}
	if err := s.transferAggregateTx(tx, newAncestors, stats, 1); err != nil {
		return err
	}
	oldAncestorIDs := make([]string, 0, len(oldAncestors))
	subtreeIDs := make([]string, 0, len(subtree))
	for _, item := range oldAncestors {
		oldAncestorIDs = append(oldAncestorIDs, item.AncestorID)
	}
	for _, item := range subtree {
		subtreeIDs = append(subtreeIDs, item.DescendantID)
	}
	if len(oldAncestorIDs) > 0 && len(subtreeIDs) > 0 {
		if err := tx.Where("ancestor_id IN ? AND descendant_id IN ?", oldAncestorIDs, subtreeIDs).
			Delete(&FolderClosure{}).Error; err != nil {
			return err
		}
	}
	newLinks := make([]FolderClosure, 0, len(newAncestors)*len(subtree))
	for _, ancestor := range newAncestors {
		for _, descendant := range subtree {
			newLinks = append(newLinks, FolderClosure{
				TenantID: tenantID, KnowledgeBaseID: kbID,
				AncestorID: ancestor.AncestorID, DescendantID: descendant.DescendantID,
				Depth: ancestor.Depth + 1 + descendant.Depth,
			})
		}
	}
	if len(newLinks) > 0 {
		if err := tx.Create(&newLinks).Error; err != nil {
			return err
		}
	}
	if folder.ParentID != "" {
		if err := updateDirectChildCount(tx, folder.ParentID, -1); err != nil {
			return err
		}
	}
	if newParent != nil {
		if err := updateDirectChildCount(tx, newParent.ID, 1); err != nil {
			return err
		}
	}
	newPath := pathFor(newParentPath, folder.Name)
	depthDelta := newDepth - folder.Depth
	if err := s.replaceSubtreePathTx(tx, tenantID, kbID, folder.ID, folder.Path, newPath, depthDelta); err != nil {
		return err
	}
	return tx.Model(&Folder{}).Where("id = ?", folder.ID).Updates(map[string]any{
		"parent_id":  newParentID,
		"path":       newPath,
		"depth":      newDepth,
		"updated_at": time.Now(),
	}).Error
}

func updateDirectChildCount(tx *gorm.DB, folderID string, delta int64) error {
	if folderID == "" {
		return nil
	}
	return tx.Model(&FolderStats{}).Where("folder_id = ?", folderID).Updates(map[string]any{
		"direct_child_folder_count": nonNegativeExpr("direct_child_folder_count", delta),
		"updated_at":                time.Now(),
	}).Error
}

func nonNegativeExpr(column string, delta int64) clause.Expr {
	return gorm.Expr(
		"CASE WHEN "+column+" + ? < 0 THEN 0 ELSE "+column+" + ? END",
		delta, delta,
	)
}

func (s *Service) transferAggregateTx(
	tx *gorm.DB,
	ancestors []FolderClosure,
	stats FolderStats,
	sign int64,
) error {
	if len(ancestors) == 0 {
		return nil
	}
	ids := make([]string, 0, len(ancestors))
	for _, ancestor := range ancestors {
		ids = append(ids, ancestor.AncestorID)
	}
	return tx.Model(&FolderStats{}).Where("folder_id IN ?", ids).Updates(map[string]any{
		"subtree_document_count": nonNegativeExpr(
			"subtree_document_count", sign*stats.SubtreeDocumentCount,
		),
		"parse_pending_count": nonNegativeExpr(
			"parse_pending_count", sign*stats.ParsePendingCount,
		),
		"parse_running_count": nonNegativeExpr(
			"parse_running_count", sign*stats.ParseRunningCount,
		),
		"enrichment_pending_task_count": nonNegativeExpr(
			"enrichment_pending_task_count", sign*stats.EnrichmentPendingTaskCount,
		),
		"wiki_pending_task_count": nonNegativeExpr(
			"wiki_pending_task_count", sign*stats.WikiPendingTaskCount,
		),
		"abnormal_document_count": nonNegativeExpr(
			"abnormal_document_count", sign*stats.AbnormalDocumentCount,
		),
		"failed_document_count": nonNegativeExpr(
			"failed_document_count", sign*stats.FailedDocumentCount,
		),
		"updated_at": time.Now(),
	}).Error
}

func (s *Service) MoveDocuments(
	ctx context.Context,
	kbID string,
	req MoveDocumentsRequest,
) (int64, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return 0, err
	}
	ids := uniqueNonEmpty(req.KnowledgeIDs)
	if len(ids) == 0 {
		return 0, ErrDocumentNotFound
	}
	var affected int64
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.findFolder(tx, tenantID, kbID, req.TargetFolderID, true); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND id IN ?", tenantID, kbID, ids).
			Count(&count).Error; err != nil {
			return err
		}
		if count != int64(len(ids)) {
			return ErrDocumentNotFound
		}
		result := tx.Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND id IN ?", tenantID, kbID, ids).
			Update("folder_id", strings.TrimSpace(req.TargetFolderID))
		affected = result.RowsAffected
		return result.Error
	})
	return affected, err
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Service) DeleteFolder(
	ctx context.Context,
	kbID, folderID, mode string,
) error {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return err
	}
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" {
		mode = "reject"
	}
	if mode != "reject" && mode != "move_to_parent" {
		return ErrFolderDeleteMode
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		folder, err := s.findFolder(tx, tenantID, kbID, folderID, true)
		if err != nil {
			return err
		}
		var stats FolderStats
		if err := tx.Where("folder_id = ?", folder.ID).First(&stats).Error; err != nil {
			return err
		}
		if mode == "reject" && (stats.SubtreeDocumentCount > 0 || stats.DirectChildFolderCount > 0) {
			return ErrFolderNotEmpty
		}
		if mode == "move_to_parent" {
			var children []Folder
			if err := tx.Where(
				"tenant_id = ? AND knowledge_base_id = ? AND parent_id = ?",
				tenantID, kbID, folder.ID,
			).Order("sort_order ASC, id ASC").Find(&children).Error; err != nil {
				return err
			}
			for index := range children {
				child := children[index]
				if err := s.moveFolderTx(tx, tenantID, kbID, &child, folder.ParentID); err != nil {
					return err
				}
			}
			if err := tx.Model(&types.Knowledge{}).
				Where("tenant_id = ? AND knowledge_base_id = ? AND folder_id = ?", tenantID, kbID, folder.ID).
				Update("folder_id", folder.ParentID).Error; err != nil {
				return err
			}
		}
		var remainingDocs int64
		if err := tx.Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND folder_id = ?", tenantID, kbID, folder.ID).
			Count(&remainingDocs).Error; err != nil {
			return err
		}
		var remainingChildren int64
		if err := tx.Model(&Folder{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND parent_id = ?", tenantID, kbID, folder.ID).
			Count(&remainingChildren).Error; err != nil {
			return err
		}
		if remainingDocs > 0 || remainingChildren > 0 {
			return ErrFolderNotEmpty
		}
		if folder.ParentID != "" {
			if err := updateDirectChildCount(tx, folder.ParentID, -1); err != nil {
				return err
			}
		}
		if err := tx.Where("ancestor_id = ? OR descendant_id = ?", folder.ID, folder.ID).
			Delete(&FolderClosure{}).Error; err != nil {
			return err
		}
		if err := tx.Where("folder_id = ?", folder.ID).Delete(&FolderStats{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Folder{}, "id = ?", folder.ID).Error
	})
}

func (s *Service) ListFolderOptions(ctx context.Context, kbID string) ([]FolderOption, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var folders []Folder
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Order("depth ASC, sort_order ASC, normalized_name ASC").
		Find(&folders).Error; err != nil {
		return nil, err
	}
	views, err := s.folderViews(ctx, folders)
	if err != nil {
		return nil, err
	}
	options := make([]FolderOption, 0, len(views))
	for _, view := range views {
		options = append(options, FolderOption{
			ID: view.ID, ParentID: view.ParentID, Name: view.Name,
			Path: view.Path, Depth: view.Depth, Stats: view.Stats,
		})
	}
	return options, nil
}

// SearchKnowledgeBase searches folders and documents recursively within a
// knowledge base (or one of its folders). Folder hits stay management-only;
// callers expand them through ListNodes and only document IDs can be selected
// for retrieval.
func (s *Service) SearchKnowledgeBase(
	ctx context.Context,
	kbID, folderID, keyword string,
	page, pageSize int,
	filter types.KnowledgeListFilter,
) (*FolderSearchPage, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	page, pageSize, err = normalizePage(page, pageSize)
	if err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return &FolderSearchPage{Data: []Node{}, Page: page, PageSize: pageSize}, nil
	}
	if folderID != "" {
		if _, err := s.GetFolder(ctx, kbID, folderID); err != nil {
			return nil, err
		}
	}
	filter.Keyword = keyword
	pattern := likePattern(keyword)
	folderQuery := s.db.WithContext(ctx).Model(&Folder{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Where(
			"(LOWER(name) LIKE ? ESCAPE '\\' OR LOWER(path) LIKE ? ESCAPE '\\' OR LOWER(description) LIKE ? ESCAPE '\\')",
			pattern, pattern, pattern,
		)
	documentQuery := s.applyDocumentFilter(
		s.db.WithContext(ctx).Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID),
		filter,
	)
	if folderID != "" {
		descendants := s.db.WithContext(ctx).Model(&FolderClosure{}).
			Select("descendant_id").
			Where(
				"tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ?",
				tenantID, kbID, folderID,
			)
		folderQuery = folderQuery.Where("id IN (?) AND id <> ?", descendants, folderID)
		documentQuery = documentQuery.Where("folder_id IN (?)", descendants)
	}
	kbName := ""
	if s.knowledgeBaseService != nil {
		if kb, lookupErr := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID); lookupErr == nil && kb != nil {
			kbName = kb.Name
		}
	}
	return s.pageSearchQueries(ctx, folderQuery, documentQuery, kbName, page, pageSize)
}

type accessibleKnowledgeBaseScope struct {
	TenantID uint64
	ID       string
	Name     string
}

// SearchAccessible searches all document knowledge bases visible to the
// caller, including organization-shared bases. requestedKBIDs, when supplied,
// is an intersection filter used by knowledge pickers (for example an
// agent-configured subset); it never expands caller access.
func (s *Service) SearchAccessible(
	ctx context.Context,
	keyword string,
	page, pageSize int,
	requestedKBIDs []string,
) (*FolderSearchPage, error) {
	callerTenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	page, pageSize, err = normalizePage(page, pageSize)
	if err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return &FolderSearchPage{Data: []Node{}, Page: page, PageSize: pageSize}, nil
	}
	requested := make(map[string]struct{}, len(requestedKBIDs))
	for _, id := range requestedKBIDs {
		if id = strings.TrimSpace(id); id != "" {
			requested[id] = struct{}{}
		}
	}
	allRequested := len(requested) == 0
	scopesByKey := make(map[string]accessibleKnowledgeBaseScope)
	add := func(scope accessibleKnowledgeBaseScope) {
		if scope.ID == "" || (!allRequested && !containsStringKey(requested, scope.ID)) {
			return
		}
		scopesByKey[fmt.Sprintf("%d/%s", scope.TenantID, scope.ID)] = scope
	}
	if s.knowledgeBaseService != nil {
		owned, listErr := s.knowledgeBaseService.ListKnowledgeBases(ctx)
		if listErr != nil {
			return nil, listErr
		}
		for _, kb := range owned {
			if kb != nil && kb.Type == types.KnowledgeBaseTypeDocument && !kb.IsTemporary {
				add(accessibleKnowledgeBaseScope{TenantID: callerTenantID, ID: kb.ID, Name: kb.Name})
			}
		}
	}
	if s.kbShareService != nil {
		shared, listErr := s.kbShareService.ListSharedKnowledgeBases(
			ctx, callerTenantID, types.TenantRoleFromContext(ctx),
		)
		if listErr != nil {
			return nil, listErr
		}
		for _, item := range shared {
			kb := item.KnowledgeBase
			if kb != nil && kb.Type == types.KnowledgeBaseTypeDocument && !kb.IsTemporary {
				add(accessibleKnowledgeBaseScope{
					TenantID: item.SourceTenantID,
					ID:       kb.ID,
					Name:     kb.Name,
				})
			}
		}
	}
	scopes := make([]accessibleKnowledgeBaseScope, 0, len(scopesByKey))
	for _, scope := range scopesByKey {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Name == scopes[j].Name {
			return scopes[i].ID < scopes[j].ID
		}
		return scopes[i].Name < scopes[j].Name
	})
	if len(scopes) == 0 {
		return &FolderSearchPage{Data: []Node{}, Page: page, PageSize: pageSize}, nil
	}
	pattern := likePattern(keyword)
	folderQuery := applyAccessibleScope(
		s.db.WithContext(ctx).Model(&Folder{}), scopes,
	).Where(
		"(LOWER(name) LIKE ? ESCAPE '\\' OR LOWER(path) LIKE ? ESCAPE '\\' OR LOWER(description) LIKE ? ESCAPE '\\')",
		pattern, pattern, pattern,
	)
	documentQuery := applyAccessibleScope(
		s.db.WithContext(ctx).Model(&types.Knowledge{}), scopes,
	)
	documentQuery = s.applyDocumentFilter(documentQuery, types.KnowledgeListFilter{Keyword: keyword})
	return s.pageAccessibleSearchQueries(ctx, folderQuery, documentQuery, scopes, page, pageSize)
}

func containsStringKey(values map[string]struct{}, key string) bool {
	_, ok := values[key]
	return ok
}

func applyAccessibleScope(query *gorm.DB, scopes []accessibleKnowledgeBaseScope) *gorm.DB {
	if len(scopes) == 0 {
		return query.Where("1 = 0")
	}
	var expression strings.Builder
	args := make([]any, 0, len(scopes)*2)
	expression.WriteString("(")
	for i, scope := range scopes {
		if i > 0 {
			expression.WriteString(" OR ")
		}
		expression.WriteString("(tenant_id = ? AND knowledge_base_id = ?)")
		args = append(args, scope.TenantID, scope.ID)
	}
	expression.WriteString(")")
	return query.Where(expression.String(), args...)
}

func (s *Service) pageSearchQueries(
	ctx context.Context,
	folderQuery, documentQuery *gorm.DB,
	kbName string,
	page, pageSize int,
) (*FolderSearchPage, error) {
	var folderTotal, documentTotal int64
	if err := folderQuery.Count(&folderTotal).Error; err != nil {
		return nil, err
	}
	if err := documentQuery.Count(&documentTotal).Error; err != nil {
		return nil, err
	}
	offset := (page - 1) * pageSize
	nodes := make([]Node, 0, pageSize)
	if int64(offset) < folderTotal {
		var folders []Folder
		if err := folderQuery.Order("depth ASC, normalized_name ASC, id ASC").
			Offset(offset).Limit(pageSize).Find(&folders).Error; err != nil {
			return nil, err
		}
		views, err := s.folderViews(ctx, folders)
		if err != nil {
			return nil, err
		}
		for i := range views {
			view := views[i]
			nodes = append(nodes, Node{
				NodeType: "folder", KnowledgeBaseID: view.KnowledgeBaseID,
				KnowledgeBaseName: kbName, Folder: &view,
			})
		}
	}
	remaining := pageSize - len(nodes)
	if remaining > 0 {
		documentOffset := offset - int(folderTotal)
		if documentOffset < 0 {
			documentOffset = 0
		}
		var documents []*types.Knowledge
		if err := documentQuery.Order("updated_at DESC, id ASC").
			Offset(documentOffset).Limit(remaining).Find(&documents).Error; err != nil {
			return nil, err
		}
		s.attachDocumentTags(ctx, documents)
		for _, document := range documents {
			nodes = append(nodes, Node{
				NodeType: "document", KnowledgeBaseID: document.KnowledgeBaseID,
				KnowledgeBaseName: kbName, Document: document,
			})
		}
	}
	return &FolderSearchPage{
		Data: nodes, Total: folderTotal + documentTotal, Page: page, PageSize: pageSize,
	}, nil
}

func (s *Service) pageAccessibleSearchQueries(
	ctx context.Context,
	folderQuery, documentQuery *gorm.DB,
	scopes []accessibleKnowledgeBaseScope,
	page, pageSize int,
) (*FolderSearchPage, error) {
	names := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		names[fmt.Sprintf("%d/%s", scope.TenantID, scope.ID)] = scope.Name
	}
	result, err := s.pageSearchQueries(ctx, folderQuery, documentQuery, "", page, pageSize)
	if err != nil {
		return nil, err
	}
	documents := make([]*types.Knowledge, 0, len(result.Data))
	for i := range result.Data {
		node := &result.Data[i]
		tenantID := uint64(0)
		if node.Folder != nil {
			tenantID = node.Folder.TenantID
		} else if node.Document != nil {
			tenantID = node.Document.TenantID
			documents = append(documents, node.Document)
		}
		node.KnowledgeBaseName = names[fmt.Sprintf("%d/%s", tenantID, node.KnowledgeBaseID)]
	}
	s.attachDocumentTagsDirect(ctx, documents)
	return result, nil
}

func (s *Service) attachDocumentTagsDirect(ctx context.Context, documents []*types.Knowledge) {
	if len(documents) == 0 {
		return
	}
	ids := make([]string, 0, len(documents))
	for _, document := range documents {
		ids = append(ids, document.ID)
	}
	var rows []struct {
		KnowledgeID string
		types.KnowledgeTag
	}
	if err := s.db.WithContext(ctx).
		Table("knowledge_tag_relations AS r").
		Select("r.knowledge_id, t.*").
		Joins("JOIN knowledge_tags AS t ON t.id = r.tag_id").
		Where("r.knowledge_id IN ?", ids).
		Order("t.sort_order ASC, t.name ASC").
		Scan(&rows).Error; err != nil {
		return
	}
	tags := make(map[string][]*types.KnowledgeTag)
	for i := range rows {
		tag := rows[i].KnowledgeTag
		tags[rows[i].KnowledgeID] = append(tags[rows[i].KnowledgeID], &tag)
	}
	for _, document := range documents {
		document.Tags = tags[document.ID]
	}
}

func (s *Service) ReconcileAll(ctx context.Context) error {
	var scopes []struct {
		TenantID        uint64
		KnowledgeBaseID string
	}
	if err := s.db.WithContext(ctx).Model(&Folder{}).
		Select("DISTINCT tenant_id, knowledge_base_id").
		Scan(&scopes).Error; err != nil {
		return err
	}
	for _, scope := range scopes {
		if err := s.reconcileScope(ctx, scope.TenantID, scope.KnowledgeBaseID); err != nil {
			return err
		}
	}
	return nil
}

type statVector struct {
	documents  int64
	pending    int64
	running    int64
	enrichment int64
	wiki       int64
	abnormal   int64
	failed     int64
}

var folderStatsDerivativeSuccess = map[string]struct{}{
	"":          {},
	"none":      {},
	"completed": {},
	"done":      {},
	"skipped":   {},
}

func normalizedFolderStatsStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

// knowledgeHasTerminalWorkflowFailure mirrors the user-visible workflow
// projection: an optional branch failure remains recoverable while any
// derivative branch is still active, and only becomes a document failure once
// all branches have settled. A degraded branch is abnormal, but not failed.
func knowledgeHasTerminalWorkflowFailure(knowledge types.Knowledge) bool {
	parseStatus := normalizedFolderStatsStatus(knowledge.ParseStatus)
	if parseStatus == types.ParseStatusFailed {
		return true
	}
	if parseStatus != types.ParseStatusCompleted {
		return false
	}

	statuses := []string{
		normalizedFolderStatsStatus(knowledge.SummaryStatus),
		normalizedFolderStatsStatus(knowledge.EnrichmentStatus),
		normalizedFolderStatsStatus(knowledge.WikiStatus),
	}
	for _, status := range statuses {
		if status == types.SummaryStatusPending || status == types.SummaryStatusProcessing {
			return false
		}
	}
	for _, status := range statuses {
		if status == types.EnrichmentStatusDegraded || status == types.WikiStatusDegraded {
			continue
		}
		if _, ok := folderStatsDerivativeSuccess[status]; !ok {
			return true
		}
	}
	return false
}

func knowledgeHasAbnormalSignal(knowledge types.Knowledge) bool {
	parseStatus := normalizedFolderStatsStatus(knowledge.ParseStatus)
	enrichmentStatus := normalizedFolderStatsStatus(knowledge.EnrichmentStatus)
	wikiStatus := normalizedFolderStatsStatus(knowledge.WikiStatus)
	return parseStatus == types.ParseStatusFailed ||
		parseStatus == types.ParseStatusCancelled ||
		enrichmentStatus == types.EnrichmentStatusFailed ||
		enrichmentStatus == types.EnrichmentStatusDegraded ||
		wikiStatus == types.WikiStatusFailed ||
		wikiStatus == types.WikiStatusDegraded
}

func vectorForKnowledge(knowledge types.Knowledge) statVector {
	vector := statVector{documents: 1}
	if knowledge.ParseStatus == types.ParseStatusPending {
		vector.pending = 1
	}
	if knowledge.ParseStatus == types.ParseStatusProcessing ||
		knowledge.ParseStatus == types.ParseStatusCancelling {
		vector.running = 1
	}
	if knowledge.PendingSubtasksCount > 0 {
		vector.enrichment = int64(knowledge.PendingSubtasksCount)
	}
	if knowledge.WikiStatus == types.WikiStatusPending {
		vector.wiki = 1
	}
	if knowledgeHasTerminalWorkflowFailure(knowledge) {
		vector.failed = 1
	} else if knowledgeHasAbnormalSignal(knowledge) {
		vector.abnormal = 1
	}
	return vector
}

func (v *statVector) add(other statVector) {
	v.documents += other.documents
	v.pending += other.pending
	v.running += other.running
	v.enrichment += other.enrichment
	v.wiki += other.wiki
	v.abnormal += other.abnormal
	v.failed += other.failed
}

func (s *Service) reconcileScope(ctx context.Context, tenantID uint64, kbID string) error {
	var folders []Folder
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Find(&folders).Error; err != nil {
		return err
	}
	if len(folders) == 0 {
		return nil
	}
	var documents []types.Knowledge
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ? AND folder_id <> ''", tenantID, kbID).
		Find(&documents).Error; err != nil {
		return err
	}
	direct := make(map[string]statVector, len(folders))
	for _, document := range documents {
		direct[document.FolderID] = func(value statVector) statVector {
			value.add(vectorForKnowledge(document))
			return value
		}(direct[document.FolderID])
	}
	children := make(map[string][]string, len(folders))
	for _, folder := range folders {
		children[folder.ParentID] = append(children[folder.ParentID], folder.ID)
	}
	totals := make(map[string]statVector, len(folders))
	visiting := make(map[string]bool, len(folders))
	var sum func(string) statVector
	sum = func(folderID string) statVector {
		if value, exists := totals[folderID]; exists {
			return value
		}
		value := direct[folderID]
		if visiting[folderID] {
			return value
		}
		visiting[folderID] = true
		for _, childID := range children[folderID] {
			value.add(sum(childID))
		}
		visiting[folderID] = false
		totals[folderID] = value
		return value
	}
	for _, folder := range folders {
		sum(folder.ID)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, folder := range folders {
			value := totals[folder.ID]
			if err := tx.Model(&FolderStats{}).Where("folder_id = ?", folder.ID).Updates(map[string]any{
				"direct_child_folder_count":     int64(len(children[folder.ID])),
				"subtree_document_count":        value.documents,
				"parse_pending_count":           value.pending,
				"parse_running_count":           value.running,
				"enrichment_pending_task_count": value.enrichment,
				"wiki_pending_task_count":       value.wiki,
				"abnormal_document_count":       value.abnormal,
				"failed_document_count":         value.failed,
				"updated_at":                    time.Now(),
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func sortFoldersByDepthDesc(folders []Folder) {
	sort.Slice(folders, func(i, j int) bool {
		if folders[i].Depth != folders[j].Depth {
			return folders[i].Depth > folders[j].Depth
		}
		return folders[i].ID < folders[j].ID
	})
}
