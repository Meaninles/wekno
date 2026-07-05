package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSelectedSkillContextIncludesSharedUserSkill(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	if err := db.AutoMigrate(&types.User{}, &types.OrganizationTenantMember{}); err != nil {
		t.Fatalf("migrate related tables: %v", err)
	}

	sourceUser := types.User{ID: "source-user", Username: "source", TenantID: 10000, IsActive: true}
	targetUser := types.User{ID: "target-user", Username: "target", TenantID: 10002, IsActive: true}
	if err := db.Create(&sourceUser).Error; err != nil {
		t.Fatalf("create source user: %v", err)
	}
	if err := db.Create(&targetUser).Error; err != nil {
		t.Fatalf("create target user: %v", err)
	}

	skill := Skill{
		ID:           "shared-skill",
		TenantID:     10000,
		CreatorID:    sourceUser.ID,
		Name:         "测试skill",
		Description:  "共享测试 Skill",
		Instructions: "当用户说使用测试skill时，回复：共享 Skill 已生效。",
		Enabled:      true,
	}
	if err := db.Create(&skill).Error; err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if err := db.Create(&UserShare{
		ID:             "share-1",
		SkillID:        skill.ID,
		TargetUserID:   targetUser.ID,
		SharedByUserID: sourceUser.ID,
		SourceTenantID: skill.TenantID,
		Permission:     types.OrgRoleViewer,
	}).Error; err != nil {
		t.Fatalf("create share: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, targetUser.TenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, targetUser.ID)
	got, err := svc.SelectedSkillContext(ctx, []string{skill.Name})
	if err != nil {
		t.Fatalf("SelectedSkillContext returned error: %v", err)
	}
	if !strings.Contains(got, "共享 Skill 已生效") {
		t.Fatalf("selected shared skill context missing instructions:\n%s", got)
	}
}

func TestCreateLightweightSkillUsesPresetPromptValidation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(11001))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "lightweight-user")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)

	skill, err := svc.Create(ctx, SkillRequest{
		Name:         "报告 摘要 Prompt",
		Description:  "",
		Instructions: "请先提炼核心结论，再列出关键依据。",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if skill.Name != "报告 摘要 Prompt" || skill.Description != "" {
		t.Fatalf("created skill = %+v, want readable preset prompt name and optional description", skill)
	}

	_, err = svc.Create(ctx, SkillRequest{
		Name:         "空提示词",
		Description:  "should fail",
		Instructions: " ",
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("Create empty prompt error = %v, want prompt is required", err)
	}
}

func TestCreateLightweightSkillDuplicateNameReturnsSentinel(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(11002))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "duplicate-lightweight-user")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)

	req := SkillRequest{
		Name:         "重复技能",
		Description:  "duplicate test",
		Instructions: "按要求完成任务。",
	}
	if _, err := svc.Create(ctx, req); err != nil {
		t.Fatalf("first Create returned error: %v", err)
	}
	if _, err := svc.Create(ctx, req); !errors.Is(err, errLightweightSkillNameExists) {
		t.Fatalf("second Create error = %v, want errLightweightSkillNameExists", err)
	}
}

func TestDuplicateSkillNameErrorDetectionCoversDatabaseConstraints(t *testing.T) {
	lightErr := errors.New(`ERROR: duplicate key value violates unique constraint "idx_custom_skill_global_name" (SQLSTATE 23505)`)
	if !isLightweightSkillNameExistsError(lightErr) {
		t.Fatalf("lightweight duplicate constraint was not detected")
	}
	proErr := errors.New(`ERROR: duplicate key value violates unique constraint "idx_custom_professional_skill_name" (SQLSTATE 23505)`)
	if !isProfessionalSkillNameExistsError(proErr) {
		t.Fatalf("professional duplicate constraint was not detected")
	}
	if isLightweightSkillNameExistsError(proErr) {
		t.Fatalf("professional duplicate constraint must not be classified as lightweight")
	}
}

func TestProfessionalPackagesSelectsConfiguredSkills(t *testing.T) {
	root := t.TempDir()
	writeProfessionalSkill(t, root, "alpha-skill", "Alpha skill", "alpha body")
	writeProfessionalSkill(t, root, "beta-skill", "Beta skill", "beta body")
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	metadata, err := ProfessionalMetadata(context.Background())
	if err != nil {
		t.Fatalf("ProfessionalMetadata returned error: %v", err)
	}
	if len(metadata) != 2 || metadata[0].Name != "alpha-skill" || metadata[1].Name != "beta-skill" {
		t.Fatalf("metadata = %+v, want sorted alpha/beta skills", metadata)
	}

	packages, err := ProfessionalPackages(context.Background(), []string{"beta-skill"}, false)
	if err != nil {
		t.Fatalf("ProfessionalPackages returned error: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("packages len = %d, want 1", len(packages))
	}
	if packages[0].Name != "beta-skill" || packages[0].Description != "Beta skill" {
		t.Fatalf("package metadata = %+v, want beta-skill", packages[0])
	}
	if len(packages[0].Files) != 1 || packages[0].Files[0].Path != "SKILL.md" {
		t.Fatalf("package files = %+v, want only SKILL.md", packages[0].Files)
	}
	content, err := base64.StdEncoding.DecodeString(packages[0].Files[0].ContentBase64)
	if err != nil {
		t.Fatalf("decode skill content: %v", err)
	}
	if !strings.Contains(string(content), "beta body") {
		t.Fatalf("packaged content = %q, want beta body", string(content))
	}
}

func TestImportProfessionalSkillDuplicateNameReturnsSentinel(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	root := t.TempDir()
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipFile(t, zw, "duplicate-pro/SKILL.md", "---\nname: duplicate-pro\ndescription: Duplicate professional skill\n---\n\n# Body\n")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(12003))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "duplicate-professional-user")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)
	req := func() ProfessionalSkillImportRequest {
		return ProfessionalSkillImportRequest{
			File:     nopMultipartFile{bytes.NewReader(buf.Bytes())},
			Filename: "duplicate-pro.zip",
		}
	}
	if _, err := svc.ImportProfessionalSkill(ctx, req()); err != nil {
		t.Fatalf("first ImportProfessionalSkill returned error: %v", err)
	}
	if _, err := svc.ImportProfessionalSkill(ctx, req()); !errors.Is(err, errProfessionalSkillNameExists) {
		t.Fatalf("second ImportProfessionalSkill error = %v, want errProfessionalSkillNameExists", err)
	}
}

func TestReservedProfessionalSkillsAreImmutablePresets(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	root := t.TempDir()
	writeProfessionalSkill(t, root, "anysearch-skill", "AnySearch", "search body")
	writeProfessionalSkill(t, root, "find-skill-skillhub", "Find SkillHub", "find body")
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(14001))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "reserved-professional-owner")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)

	items, err := svc.ListProfessionalForManage(ctx)
	if err != nil {
		t.Fatalf("ListProfessionalForManage returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	for _, item := range items {
		if !item.SystemReserved {
			t.Fatalf("item %s SystemReserved = false, want true", item.Name)
		}
		if !item.IsMine || item.CanManage || item.CanDownload || item.ID != "" {
			t.Fatalf("reserved item = %+v, want owned preset without generated management capability", item)
		}
	}

	if _, err := svc.ImportProfessionalSkill(ctx, ProfessionalSkillImportRequest{Name: "anysearch-skill"}); err == nil ||
		!strings.Contains(err.Error(), "system reserved") {
		t.Fatalf("ImportProfessionalSkill reserved error = %v, want system reserved", err)
	}

	record := &ProfessionalSkill{
		TenantID:        14001,
		CreatorID:       "reserved-professional-owner",
		Name:            "anysearch-skill",
		Description:     "existing reserved record",
		ArchiveFileName: "anysearch-skill.zip",
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("create reserved professional record: %v", err)
	}

	items, err = svc.ListProfessionalForManage(ctx)
	if err != nil {
		t.Fatalf("ListProfessionalForManage after existing record returned error: %v", err)
	}
	for _, item := range items {
		if item.Name == "anysearch-skill" && (!item.IsMine || item.CanManage || item.CanDownload) {
			t.Fatalf("reserved item with existing record = %+v, want owned immutable preset", item)
		}
	}

	otherCtx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(14002))
	otherCtx = context.WithValue(otherCtx, types.UserIDContextKey, "other-reserved-professional-user")
	otherCtx = context.WithValue(otherCtx, types.TenantRoleContextKey, types.TenantRoleContributor)
	otherItems, err := svc.ListProfessionalForManage(otherCtx)
	if err != nil {
		t.Fatalf("ListProfessionalForManage for other user returned error: %v", err)
	}
	var foundOtherReserved bool
	for _, item := range otherItems {
		if item.Name != "anysearch-skill" {
			continue
		}
		foundOtherReserved = true
		if !item.SystemReserved || !item.IsMine || item.CanManage || item.CanDownload {
			t.Fatalf("reserved item for other user = %+v, want owned immutable preset", item)
		}
	}
	if !foundOtherReserved {
		t.Fatalf("reserved skill with existing record was not visible to other user: %+v", otherItems)
	}

	if _, err := svc.UpdateProfessionalSkill(ctx, record.ID, ProfessionalSkillUpdateRequest{Name: "renamed-anysearch"}); err == nil ||
		!strings.Contains(err.Error(), "system reserved") {
		t.Fatalf("UpdateProfessionalSkill reserved error = %v, want system reserved", err)
	}
	if err := svc.DeleteProfessionalSkill(ctx, record.ID); err == nil ||
		!strings.Contains(err.Error(), "system reserved") {
		t.Fatalf("DeleteProfessionalSkill reserved error = %v, want system reserved", err)
	}

	regular := &ProfessionalSkill{
		TenantID:        14001,
		CreatorID:       "reserved-professional-owner",
		Name:            "regular-professional-skill",
		Description:     "regular",
		ArchiveFileName: "regular.zip",
	}
	if err := db.Create(regular).Error; err != nil {
		t.Fatalf("create regular professional record: %v", err)
	}
	if _, err := svc.UpdateProfessionalSkill(ctx, regular.ID, ProfessionalSkillUpdateRequest{Name: "find-skill-skillhub"}); err == nil ||
		!strings.Contains(err.Error(), "system reserved") {
		t.Fatalf("UpdateProfessionalSkill rename to reserved error = %v, want system reserved", err)
	}
}

func TestProfessionalSkillRelativePathValidationAllowsSafeUnicode(t *testing.T) {
	valid := map[string]string{
		"references/7套新增风格规范.md":        "references/7套新增风格规范.md",
		`references\MBE插画风格规范.md`:       "references/MBE插画风格规范.md",
		"./references/粗线条感风格 PPT 模板.md": "references/粗线条感风格 PPT 模板.md",
	}
	for input, want := range valid {
		got, err := normalizeProfessionalSkillRelativePath(input)
		if err != nil {
			t.Fatalf("normalizeProfessionalSkillRelativePath(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeProfessionalSkillRelativePath(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"",
		"/absolute.md",
		"../escape.md",
		"references/../escape.md",
		"references//double-slash.md",
		"references/a:b.md",
		"references/\x00bad.md",
		"references/\u202Ebad.md",
	}
	for _, input := range invalid {
		if got, err := normalizeProfessionalSkillRelativePath(input); err == nil {
			t.Fatalf("normalizeProfessionalSkillRelativePath(%q) = %q, want error", input, got)
		}
	}
}

func TestListProfessionalForManageAdoptsFilesystemSkill(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	if err := db.AutoMigrate(&types.User{}); err != nil {
		t.Fatalf("migrate related tables: %v", err)
	}
	root := t.TempDir()
	writeProfessionalSkill(t, root, "filesystem-skill", "Filesystem skill", "filesystem body")
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(13001))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "professional-owner")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)
	items, err := svc.ListProfessionalForManage(ctx)
	if err != nil {
		t.Fatalf("ListProfessionalForManage returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	item := items[0]
	if item.ID == "" || !item.Managed || !item.CanManage || !item.CanDownload {
		t.Fatalf("filesystem professional item = %+v, want manageable downloadable item", item)
	}

	download, err := svc.DownloadProfessionalSkill(ctx, item.ID)
	if err != nil {
		t.Fatalf("DownloadProfessionalSkill returned error: %v", err)
	}
	if download.Cleanup != nil {
		defer download.Cleanup()
	}
	if download.Filename != "filesystem-skill.zip" {
		t.Fatalf("download filename = %q, want filesystem-skill.zip", download.Filename)
	}
	zr, err := zip.OpenReader(download.Path)
	if err != nil {
		t.Fatalf("open generated zip: %v", err)
	}
	var names []string
	for _, file := range zr.File {
		names = append(names, file.Name)
	}
	_ = zr.Close()
	if len(names) != 1 || names[0] != "SKILL.md" {
		t.Fatalf("generated zip files = %+v, want SKILL.md", names)
	}

	if _, err := svc.ShareProfessionalToUser(ctx, item.ID, ShareUserRequest{
		UserID:     "professional-target",
		Permission: types.OrgRoleViewer,
	}); err != nil {
		t.Fatalf("ShareProfessionalToUser returned error: %v", err)
	}
	shares, err := svc.ListProfessionalShares(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListProfessionalShares returned error: %v", err)
	}
	if len(shares.UserShares) != 1 {
		t.Fatalf("user shares len = %d, want 1", len(shares.UserShares))
	}

	updated, err := svc.UpdateProfessionalSkill(ctx, item.ID, ProfessionalSkillUpdateRequest{
		Name: "filesystem-skill-renamed",
	})
	if err != nil {
		t.Fatalf("UpdateProfessionalSkill returned error: %v", err)
	}
	if updated.Name != "filesystem-skill-renamed" || !updated.CanDownload {
		t.Fatalf("updated filesystem item = %+v, want renamed downloadable item", updated)
	}
	if _, err := os.Stat(filepath.Join(root, "filesystem-skill")); !os.IsNotExist(err) {
		t.Fatalf("old filesystem skill directory still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "filesystem-skill-renamed", "SKILL.md")); err != nil {
		t.Fatalf("renamed filesystem skill missing SKILL.md: %v", err)
	}

	if err := svc.DeleteProfessionalSkill(ctx, item.ID); err != nil {
		t.Fatalf("DeleteProfessionalSkill returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "filesystem-skill-renamed")); !os.IsNotExist(err) {
		t.Fatalf("deleted filesystem skill directory still exists or stat failed: %v", err)
	}
}

func TestImportProfessionalSkillExtractsPackageBeforeRuntime(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	root := t.TempDir()
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipFile(t, zw, "packed-skill/SKILL.md", "---\nname: package-name\ndescription: packaged description\n---\n\n# Body\n\nUse resources.\n")
	writeZipFile(t, zw, "packed-skill/references/checklist.md", "# Checklist\n")
	writeZipFile(t, zw, "packed-skill/references/7套新增风格规范.md", "# 7 styles\n")
	writeZipFile(t, zw, "packed-skill/references/粗线条感风格 PPT 模板.md", "# Bold line style\n")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(12001))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "professional-user")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)
	item, err := svc.ImportProfessionalSkill(ctx, ProfessionalSkillImportRequest{
		Name:        "imported-skill",
		Description: "display description",
		File:        nopMultipartFile{bytes.NewReader(buf.Bytes())},
		Filename:    "skill.zip",
	})
	if err != nil {
		t.Fatalf("ImportProfessionalSkill returned error: %v", err)
	}
	if item.Name != "imported-skill" || item.Description != "display description" || item.FileCount < 2 {
		t.Fatalf("imported item = %+v, want imported-skill with form description and extracted files", item)
	}
	if item.ID == "" || !item.CanManage || !item.CanDownload {
		t.Fatalf("imported item permissions = %+v, want manageable and downloadable item", item)
	}
	if _, err := os.Stat(filepath.Join(root, "imported-skill", "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md was not extracted before runtime: %v", err)
	}
	originalSkillMD, err := os.ReadFile(filepath.Join(root, "imported-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("read imported original SKILL.md: %v", err)
	}
	if !strings.Contains(string(originalSkillMD), "name: package-name") {
		t.Fatalf("import modified original SKILL.md:\n%s", string(originalSkillMD))
	}
	if _, err := os.Stat(filepath.Join(root, "imported-skill", "references", "checklist.md")); err != nil {
		t.Fatalf("resource file was not extracted before runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "imported-skill", "references", "7套新增风格规范.md")); err != nil {
		t.Fatalf("unicode resource file was not extracted before runtime: %v", err)
	}

	packages, err := ProfessionalPackages(context.Background(), []string{"imported-skill"}, false)
	if err != nil {
		t.Fatalf("ProfessionalPackages returned error: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("packages len = %d, want 1", len(packages))
	}
	if packages[0].Name != "imported-skill" {
		t.Fatalf("package name = %q, want imported-skill", packages[0].Name)
	}
	var runtimeSkillMD string
	for _, file := range packages[0].Files {
		if strings.HasSuffix(file.Path, ".zip") || file.Path == professionalSkillMetaFile || file.Path == professionalArchiveFile {
			t.Fatalf("runtime package includes non-runtime artifact: %+v", file)
		}
		if file.Path == "SKILL.md" {
			data, err := base64.StdEncoding.DecodeString(file.ContentBase64)
			if err != nil {
				t.Fatalf("decode runtime SKILL.md: %v", err)
			}
			runtimeSkillMD = string(data)
		}
	}
	if !strings.Contains(runtimeSkillMD, "name: imported-skill") {
		t.Fatalf("runtime SKILL.md was not adapted:\n%s", runtimeSkillMD)
	}
	var foundUnicode bool
	for _, file := range packages[0].Files {
		if file.Path == "references/7套新增风格规范.md" {
			foundUnicode = true
			break
		}
	}
	if !foundUnicode {
		t.Fatalf("runtime package files = %+v, want unicode reference path", packages[0].Files)
	}

	download, err := svc.DownloadProfessionalSkill(ctx, item.ID)
	if err != nil {
		t.Fatalf("DownloadProfessionalSkill returned error: %v", err)
	}
	if download.Filename != "skill.zip" {
		t.Fatalf("download filename = %q, want skill.zip", download.Filename)
	}
	downloaded, err := os.ReadFile(download.Path)
	if err != nil {
		t.Fatalf("read original package: %v", err)
	}
	if !bytes.Equal(downloaded, buf.Bytes()) {
		t.Fatalf("downloaded original package content changed")
	}

	updated, err := svc.UpdateProfessionalSkill(ctx, item.ID, ProfessionalSkillUpdateRequest{
		Name:                "updated-skill",
		Description:         "updated display description",
		DescriptionProvided: true,
	})
	if err != nil {
		t.Fatalf("UpdateProfessionalSkill returned error: %v", err)
	}
	if updated.Name != "updated-skill" || updated.Description != "updated display description" || updated.ArchiveFileName != "skill.zip" {
		t.Fatalf("updated item = %+v, want renamed item retaining original archive", updated)
	}
	if _, err := os.Stat(filepath.Join(root, "imported-skill")); !os.IsNotExist(err) {
		t.Fatalf("old skill directory still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "updated-skill", professionalArchiveFile)); err != nil {
		t.Fatalf("updated skill lost original archive: %v", err)
	}
	updatedSkillMD, err := os.ReadFile(filepath.Join(root, "updated-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("read updated SKILL.md: %v", err)
	}
	if !strings.Contains(string(updatedSkillMD), "name: package-name") ||
		!strings.Contains(string(updatedSkillMD), "description: packaged description") {
		t.Fatalf("updated original SKILL.md was unexpectedly changed:\n%s", string(updatedSkillMD))
	}
	updatedPackages, err := ProfessionalPackages(context.Background(), []string{"updated-skill"}, false)
	if err != nil {
		t.Fatalf("ProfessionalPackages after update returned error: %v", err)
	}
	if len(updatedPackages) != 1 {
		t.Fatalf("updated packages len = %d, want 1", len(updatedPackages))
	}
	var updatedRuntimeSkillMD string
	for _, file := range updatedPackages[0].Files {
		if file.Path == "SKILL.md" {
			data, err := base64.StdEncoding.DecodeString(file.ContentBase64)
			if err != nil {
				t.Fatalf("decode updated runtime SKILL.md: %v", err)
			}
			updatedRuntimeSkillMD = string(data)
		}
	}
	if !strings.Contains(updatedRuntimeSkillMD, "name: updated-skill") {
		t.Fatalf("updated runtime SKILL.md was not adapted:\n%s", updatedRuntimeSkillMD)
	}

	if err := svc.DeleteProfessionalSkill(ctx, item.ID); err != nil {
		t.Fatalf("DeleteProfessionalSkill returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "updated-skill")); !os.IsNotExist(err) {
		t.Fatalf("deleted professional skill directory still exists or stat failed: %v", err)
	}
}

func TestImportProfessionalSkillUsesSlugAndPreservesReadableName(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	root := t.TempDir()
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", root)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipFile(t, zw, "word-docx/SKILL.md", `---
name: Word / DOCX
slug: word-docx
version: 1.0.2
description: Create, inspect, and edit Microsoft Word documents and DOCX files.
---

# Word / DOCX

Use this skill for Word documents.
`)
	writeZipFile(t, zw, "word-docx/_meta.json", `{"slug":"word-docx","version":"1.0.2"}`)
	writeZipFile(t, zw, "excel-xlsx (1).zip", "not part of this skill")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(12002))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "professional-word-user")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)
	item, err := svc.ImportProfessionalSkill(ctx, ProfessionalSkillImportRequest{
		File:     nopMultipartFile{bytes.NewReader(buf.Bytes())},
		Filename: "word-docx (1).zip",
	})
	if err != nil {
		t.Fatalf("ImportProfessionalSkill returned error: %v", err)
	}
	if item.Name != "word-docx" || item.DisplayName != "word-docx" {
		t.Fatalf("imported item = %+v, want slug name and slug display name", item)
	}
	if _, err := os.Stat(filepath.Join(root, "word-docx", "excel-xlsx (1).zip")); !os.IsNotExist(err) {
		t.Fatalf("sibling archive outside skill root should not be copied into skill directory: %v", err)
	}
	originalSkillMD, err := os.ReadFile(filepath.Join(root, "word-docx", "SKILL.md"))
	if err != nil {
		t.Fatalf("read original SKILL.md: %v", err)
	}
	if !strings.Contains(string(originalSkillMD), "name: Word / DOCX") {
		t.Fatalf("original SKILL.md was modified:\n%s", string(originalSkillMD))
	}

	items, err := svc.ListProfessionalForManage(ctx)
	if err != nil {
		t.Fatalf("ListProfessionalForManage returned error: %v", err)
	}
	if len(items) != 1 || items[0].Name != "word-docx" {
		t.Fatalf("managed items = %+v, want slug name", items)
	}

	packages, err := ProfessionalPackages(context.Background(), []string{"word-docx"}, false)
	if err != nil {
		t.Fatalf("ProfessionalPackages returned error: %v", err)
	}
	if len(packages) != 1 || packages[0].Name != "word-docx" {
		t.Fatalf("packages = %+v, want slug name", packages)
	}
	var runtimeSkillMD string
	for _, file := range packages[0].Files {
		if file.Path == "SKILL.md" {
			data, err := base64.StdEncoding.DecodeString(file.ContentBase64)
			if err != nil {
				t.Fatalf("decode runtime SKILL.md: %v", err)
			}
			runtimeSkillMD = string(data)
		}
		if strings.HasSuffix(file.Path, ".zip") {
			t.Fatalf("runtime package includes sibling archive: %+v", file)
		}
	}
	if !strings.Contains(runtimeSkillMD, "name: word-docx") {
		t.Fatalf("runtime SKILL.md was not adapted with slug name:\n%s", runtimeSkillMD)
	}
}

func writeZipFile(t *testing.T, zw *zip.Writer, name, content string) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("create zip file %s: %v", name, err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("write zip file %s: %v", name, err)
	}
}

type nopMultipartFile struct {
	*bytes.Reader
}

func (f nopMultipartFile) Close() error { return nil }

func writeProfessionalSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
}
