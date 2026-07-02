package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
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
	if _, err := os.Stat(filepath.Join(root, "imported-skill", "references", "checklist.md")); err != nil {
		t.Fatalf("resource file was not extracted before runtime: %v", err)
	}

	packages, err := ProfessionalPackages(context.Background(), []string{"imported-skill"}, false)
	if err != nil {
		t.Fatalf("ProfessionalPackages returned error: %v", err)
	}
	if len(packages) != 1 {
		t.Fatalf("packages len = %d, want 1", len(packages))
	}
	for _, file := range packages[0].Files {
		if strings.HasSuffix(file.Path, ".zip") || file.Path == professionalSkillMetaFile || file.Path == professionalArchiveFile {
			t.Fatalf("runtime package includes non-runtime artifact: %+v", file)
		}
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
	if !strings.Contains(string(updatedSkillMD), "name: updated-skill") ||
		!strings.Contains(string(updatedSkillMD), "description: packaged description") {
		t.Fatalf("updated SKILL.md was not normalized:\n%s", string(updatedSkillMD))
	}

	if err := svc.DeleteProfessionalSkill(ctx, item.ID); err != nil {
		t.Fatalf("DeleteProfessionalSkill returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "updated-skill")); !os.IsNotExist(err) {
		t.Fatalf("deleted professional skill directory still exists or stat failed: %v", err)
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
