package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestProfessionalSkillIsVisibleAcrossInstancesAndRestart(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	store := newMemoryProfessionalStore()
	uploader := NewServiceWithProfessionalStore(db, store)
	if err := uploader.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}

	instanceARoot := t.TempDir()
	instanceBRoot := t.TempDir()
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", instanceARoot)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(15001))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "multi-instance-owner")
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)

	var upload bytes.Buffer
	writer := zip.NewWriter(&upload)
	writeZipFile(
		t,
		writer,
		"cluster-skill/SKILL.md",
		"---\nname: cluster-skill\ndescription: shared object test\n---\n\n# Cluster skill\n",
	)
	writeZipFile(t, writer, "cluster-skill/references/runbook.md", "# Shared runbook\n")
	if err := writer.Close(); err != nil {
		t.Fatalf("close upload: %v", err)
	}
	imported, err := uploader.ImportProfessionalSkill(ctx, ProfessionalSkillImportRequest{
		File:     nopMultipartFile{bytes.NewReader(upload.Bytes())},
		Filename: "cluster-skill.zip",
	})
	if err != nil {
		t.Fatalf("import through instance A: %v", err)
	}

	// Instance B has a different disposable filesystem and sees the uploaded
	// skill only through the shared database and object store.
	t.Setenv("WEKNORA_PROFESSIONAL_SKILLS_DIR", instanceBRoot)
	readerInstance := NewServiceWithProfessionalStore(db, store)
	items, err := readerInstance.ListProfessionalForManage(ctx)
	if err != nil {
		t.Fatalf("list through instance B: %v", err)
	}
	if len(items) != 1 || items[0].ID != imported.ID || items[0].Name != "cluster-skill" {
		t.Fatalf("instance B items = %+v, want imported skill", items)
	}
	metadata, err := readerInstance.ProfessionalMetadata(ctx)
	if err != nil {
		t.Fatalf("metadata through instance B: %v", err)
	}
	if len(metadata) != 1 || metadata[0].Name != "cluster-skill" {
		t.Fatalf("instance B metadata = %+v, want imported skill", metadata)
	}
	packages, err := readerInstance.ProfessionalPackages(ctx, []string{"cluster-skill"}, false)
	if err != nil {
		t.Fatalf("runtime package through instance B: %v", err)
	}
	if len(packages) != 1 || len(packages[0].Files) != 2 {
		t.Fatalf("instance B packages = %+v, want two-file skill", packages)
	}
	var runtimeMarkdown string
	for _, file := range packages[0].Files {
		if file.Path == "SKILL.md" {
			content, decodeErr := base64.StdEncoding.DecodeString(file.ContentBase64)
			if decodeErr != nil {
				t.Fatalf("decode runtime markdown: %v", decodeErr)
			}
			runtimeMarkdown = string(content)
		}
	}
	if !strings.Contains(runtimeMarkdown, "name: cluster-skill") {
		t.Fatalf("runtime markdown = %q, want stable runtime name", runtimeMarkdown)
	}

	// A fresh service simulates a restarted pod and must still download the
	// exact canonical package without any local workspace state.
	restarted := NewServiceWithProfessionalStore(db, store)
	download, err := restarted.DownloadProfessionalSkill(ctx, imported.ID)
	if err != nil {
		t.Fatalf("download after restart: %v", err)
	}
	content, err := io.ReadAll(download.Reader)
	if err != nil {
		t.Fatalf("read download after restart: %v", err)
	}
	_ = download.Reader.Close()
	if int64(len(content)) != download.Size || store.count() != 1 {
		t.Fatalf(
			"download size/object count = %d/%d, want %d/1",
			len(content),
			store.count(),
			download.Size,
		)
	}
}

func TestMigrateFailsClosedWhenStoredProfessionalSkillsLoseObjectConfiguration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	configured := NewServiceWithProfessionalStore(db, newMemoryProfessionalStore())
	if err := configured.Migrate(context.Background()); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if err := db.Create(&ProfessionalSkill{
		ID:           "stored-skill",
		TenantID:     15002,
		CreatorID:    "owner",
		Name:         "stored-skill",
		DisplayName:  "Stored skill",
		ObjectPath:   "memory://professional/package.zip",
		ObjectSize:   100,
		ObjectSHA256: strings.Repeat("a", 64),
		FileCount:    1,
	}).Error; err != nil {
		t.Fatalf("create stored skill: %v", err)
	}

	unconfigured := &Service{db: db}
	err = unconfigured.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "require shared object storage") {
		t.Fatalf("Migrate error = %v, want fail-closed object storage error", err)
	}
}

func TestImportRepairsOwnedLegacyRecordWhoseLocalPackageWasLost(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	store := newMemoryProfessionalStore()
	service := NewServiceWithProfessionalStore(db, store)
	if err := service.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	legacy := ProfessionalSkill{
		ID:              "legacy-missing-skill",
		TenantID:        15003,
		CreatorID:       "legacy-owner",
		Name:            "legacy-missing",
		Description:     "old metadata",
		ArchiveFileName: "legacy-missing.zip",
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy record: %v", err)
	}

	var upload bytes.Buffer
	writer := zip.NewWriter(&upload)
	writeZipFile(
		t,
		writer,
		"legacy-missing/SKILL.md",
		"---\nname: legacy-missing\ndescription: repaired package\n---\n\n# Repaired\n",
	)
	if err := writer.Close(); err != nil {
		t.Fatalf("close repair package: %v", err)
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, legacy.TenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, legacy.CreatorID)
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleContributor)
	repaired, err := service.ImportProfessionalSkill(ctx, ProfessionalSkillImportRequest{
		File:     nopMultipartFile{bytes.NewReader(upload.Bytes())},
		Filename: legacy.ArchiveFileName,
	})
	if err != nil {
		t.Fatalf("repair import: %v", err)
	}
	if repaired.ID != legacy.ID || repaired.FileCount != 1 || store.count() != 1 {
		t.Fatalf("repaired item/store = %+v/%d, want same id and one object", repaired, store.count())
	}
	var persisted ProfessionalSkill
	if err := db.First(&persisted, "id = ?", legacy.ID).Error; err != nil {
		t.Fatalf("reload repaired record: %v", err)
	}
	if persisted.ObjectPath == "" || persisted.ObjectSHA256 == "" || persisted.ObjectSize == 0 {
		t.Fatalf("persisted repair is incomplete: %+v", persisted)
	}
}
