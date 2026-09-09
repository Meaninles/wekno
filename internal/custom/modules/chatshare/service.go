package chatshare

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/artifactstore"
	"github.com/Tencent/WeKnora/internal/custom/modules/authsecurity"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

var (
	ErrWebLoginRequired              = errors.New("web login required")
	ErrInvalidMessageSelection       = errors.New("invalid message selection")
	ErrArtifactShareNotFound         = errors.New("artifact share not found")
	ErrArtifactShareUnsupported      = errors.New("artifact sharing supports HTML artifacts only")
	ErrArtifactShareRevoked          = errors.New("artifact share revoked")
	ErrArtifactShareTokenUnavailable = errors.New("artifact share token unavailable")
	ErrArtifactShareNotPublished     = errors.New("artifact share not published")
	ErrArtifactSharePasswordRequired = errors.New("artifact share password required")
	ErrArtifactSharePasswordInvalid  = errors.New("artifact share password invalid")
	ErrArtifactSharePasswordLocked   = errors.New("artifact share password temporarily locked")
	ErrArtifactSharePasswordTooShort = errors.New("artifact share password too short")
	ErrArtifactSharePasswordTooLong  = errors.New("artifact share password too long")
	ErrArtifactSharePreviewInvalid   = errors.New("artifact share preview token invalid")
	ErrArtifactSharePreviewForbidden = errors.New("artifact share preview forbidden")
)

const maxShareMessageSelection = 1000

const (
	artifactSharePreviewTTL       = 30 * time.Minute
	artifactShareAccessTTL        = 30 * time.Minute
	artifactSharePasswordMinRunes = 6
	artifactSharePasswordMaxBytes = 72 // bcrypt's input limit
)

var providerResourcePattern = regexp.MustCompile("(?i)(?:local|minio|cos|tos|s3|oss|ks3|obs)://[^\\s<>\"'`]+")

type Service struct {
	db                *gorm.DB
	sessionService    interfaces.SessionService
	tenantService     interfaces.TenantService
	globalFileService interfaces.FileService
	authSecurity      *authsecurity.Service
	frontendBaseURL   string
	localBaseDir      string
	artifactStore     *artifactstore.Store
}

func (s *Service) SetArtifactStore(store *artifactstore.Store) {
	if s != nil {
		s.artifactStore = store
	}
}

type artifactRow struct {
	ID           string         `gorm:"column:id"`
	TenantID     uint64         `gorm:"column:tenant_id"`
	UserID       string         `gorm:"column:user_id"`
	SessionID    string         `gorm:"column:session_id"`
	MessageID    string         `gorm:"column:message_id"`
	FilePath     string         `gorm:"column:file_path"`
	StorageState string         `gorm:"column:storage_state"`
	FileName     string         `gorm:"column:file_name"`
	FileType     string         `gorm:"column:file_type"`
	FileSize     int64          `gorm:"column:file_size"`
	SHA256       string         `gorm:"column:sha256"`
	ContentType  string         `gorm:"column:content_type"`
	CreatedAt    time.Time      `gorm:"column:created_at"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at"`
}

type shareTurn struct {
	ID             string
	Messages       []types.Message
	Selectable     bool
	DisabledReason string
}

func (artifactRow) TableName() string {
	return "custom_general_agent_artifacts"
}

type SharedArtifactFile struct {
	Reader      io.ReadCloser
	FileName    string
	ContentType string
	FileSize    int64
}

// ArtifactSharePasswordAttemptError preserves the shared auth-security retry
// budget so the HTTP layer can tell the recipient how many attempts remain.
// The wrapped cause remains one of the regular artifact-share password errors
// for callers that use errors.Is.
type ArtifactSharePasswordAttemptError struct {
	Cause  error
	Status *authsecurity.AttemptStatus
}

func (e *ArtifactSharePasswordAttemptError) Error() string {
	if e == nil || e.Cause == nil {
		return "artifact share password attempt failed"
	}
	return e.Cause.Error()
}

func (e *ArtifactSharePasswordAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewService(
	db *gorm.DB,
	sessionService interfaces.SessionService,
	tenantService interfaces.TenantService,
	globalFileService interfaces.FileService,
	frontendBaseURL string,
	authSecurity *authsecurity.Service,
) *Service {
	baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	if baseDir == "" {
		baseDir = "/data/files"
	}
	absDir, _ := filepath.Abs(baseDir)
	return &Service{
		db:                db,
		sessionService:    sessionService,
		tenantService:     tenantService,
		globalFileService: globalFileService,
		authSecurity:      authSecurity,
		frontendBaseURL:   strings.TrimSpace(frontendBaseURL),
		localBaseDir:      absDir,
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	return db.WithContext(ctx).AutoMigrate(&Link{}, &MessageSnapshot{}, &ResourceSnapshot{}, &ArtifactShareLink{})
}

func (s *Service) GetCandidates(ctx context.Context, sessionID string) (*CandidatesDTO, error) {
	if err := requireWebUser(ctx); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.sessionService == nil {
		return nil, fmt.Errorf("chat share service is unavailable")
	}
	session, messages, err := s.loadSessionMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	messageIDs := make([]string, 0, len(messages))
	for i := range messages {
		messageIDs = append(messageIDs, messages[i].ID)
	}
	artifactsByMessage, err := s.artifactSnapshotsByMessage(ctx, s.db, session.TenantID, session.ID, messageIDs, "")
	if err != nil {
		logger.Warnf(ctx, "[chatshare] failed to load candidate artifacts: session_id=%s err=%v", session.ID, err)
		artifactsByMessage = nil
	}

	turns := buildShareTurns(messages)
	turnByMessageID := make(map[string]*shareTurn, len(messages))
	for i := range turns {
		for _, message := range turns[i].Messages {
			turnByMessageID[message.ID] = &turns[i]
		}
	}

	candidates := make([]CandidateMessageDTO, 0, len(messages))
	for _, message := range messages {
		turn := turnByMessageID[message.ID]
		if turn == nil {
			continue
		}
		candidates = append(candidates, CandidateMessageDTO{
			ID:             message.ID,
			TurnID:         turn.ID,
			SessionID:      message.SessionID,
			RequestID:      message.RequestID,
			Content:        message.Content,
			Role:           message.Role,
			MentionedItems: append(types.MentionedItems(nil), message.MentionedItems...),
			Images:         append(types.MessageImages(nil), message.Images...),
			Attachments:    append(types.MessageAttachments(nil), message.Attachments...),
			ToolResults:    structuredToolResultsFromAgentSteps(message.AgentSteps),
			Artifacts:      artifactsByMessage[message.ID],
			IsCompleted:    message.IsCompleted,
			IsFallback:     message.IsFallback,
			Channel:        message.Channel,
			Selectable:     turn.Selectable,
			DisabledReason: turn.DisabledReason,
			CreatedAt:      message.CreatedAt,
			UpdatedAt:      message.UpdatedAt,
		})
	}
	title := strings.TrimSpace(session.Title)
	if title == "" {
		title = "未命名对话"
	}
	return &CandidatesDTO{SessionID: session.ID, Title: title, Messages: candidates}, nil
}

func (s *Service) CreateShare(ctx context.Context, sessionID string, messageIDs []string) (*LinkDTO, error) {
	if err := requireWebUser(ctx); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.sessionService == nil {
		return nil, fmt.Errorf("chat share service is unavailable")
	}

	selectedIDSet := make(map[string]struct{}, len(messageIDs))
	for _, rawID := range messageIDs {
		id := strings.TrimSpace(rawID)
		if id != "" {
			selectedIDSet[id] = struct{}{}
		}
	}
	if len(selectedIDSet) == 0 || len(selectedIDSet) > maxShareMessageSelection {
		return nil, ErrInvalidMessageSelection
	}

	session, messages, err := s.loadSessionMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	selectedMessageCount := 0
	for _, turn := range buildShareTurns(messages) {
		selectedInTurn := 0
		for _, message := range turn.Messages {
			if _, selected := selectedIDSet[message.ID]; selected {
				selectedInTurn++
			}
		}
		if selectedInTurn == 0 {
			continue
		}
		if !turn.Selectable || selectedInTurn != len(turn.Messages) {
			return nil, ErrInvalidMessageSelection
		}
		selectedMessageCount += selectedInTurn
	}
	if selectedMessageCount != len(selectedIDSet) {
		return nil, ErrInvalidMessageSelection
	}
	selectedMessages := make([]types.Message, 0, len(selectedIDSet))
	for _, message := range messages {
		if _, selected := selectedIDSet[message.ID]; selected {
			selectedMessages = append(selectedMessages, message)
		}
	}

	token, tokenHash, err := generateToken()
	if err != nil {
		return nil, err
	}
	userID, _ := types.UserIDFromContext(ctx)
	if userID == "" {
		principal, _ := types.PrincipalFromContext(ctx)
		userID = principal.ID
	}

	link := Link{
		TokenHash:       tokenHash,
		SourceTenantID:  session.TenantID,
		SessionID:       session.ID,
		SourceUserID:    session.UserID,
		CreatedByUserID: userID,
		Title:           strings.TrimSpace(session.Title),
		Status:          ShareStatusActive,
	}
	if link.Title == "" {
		link.Title = "未命名对话"
	}

	snapshots := make([]MessageSnapshot, 0, len(selectedMessages))
	for idx, message := range selectedMessages {
		snapshots = append(snapshots, messageSnapshotFromMessage(message, idx+1))
	}

	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&link).Error; err != nil {
			return err
		}
		for i := range snapshots {
			snapshots[i].ShareID = link.ID
		}
		if len(snapshots) > 0 {
			if err := tx.Create(&snapshots).Error; err != nil {
				return err
			}
		}
		resources, err := s.buildResourceSnapshots(ctx, tx, &link, selectedMessages)
		if err != nil {
			return err
		}
		if len(resources) > 0 {
			if err := tx.Create(&resources).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &LinkDTO{
		ID:        link.ID,
		SessionID: link.SessionID,
		Token:     token,
		URL:       s.shareURL(token),
		Title:     link.Title,
		CreatedAt: link.CreatedAt,
	}, nil
}

// CreateArtifactShare lazily creates an anonymous capability link for one
// ready HTML artifact. The artifact is looked up from the authenticated
// owner's tenant/session scope; the client never supplies a storage path.
// The unique tenant/artifact index makes this operation idempotent across
// browser tabs and API replicas. The returned preview URL is a short-lived
// creator capability; the public URL remains unpublished until a password is
// set through SetArtifactSharePassword.
func (s *Service) CreateArtifactShare(ctx context.Context, artifactID string) (*ArtifactShareLinkDTO, error) {
	tenantID, userID, err := s.artifactOwnerContext(ctx)
	if err != nil {
		return nil, err
	}
	artifact, err := s.findOwnedHTMLArtifact(ctx, artifactID, tenantID, userID)
	if err != nil {
		return nil, err
	}
	link, err := s.getOrCreateArtifactShare(ctx, artifact, userID)
	if err != nil {
		return nil, err
	}
	return s.artifactShareLinkDTO(link)
}

// SetArtifactSharePassword publishes an artifact share. It is intentionally
// owner-scoped and idempotent: once a password exists it is never replaced by
// a later request, because the server must not be able to recover or expose
// the creator's plaintext password.
func (s *Service) SetArtifactSharePassword(ctx context.Context, artifactID, password string) (*ArtifactShareLinkDTO, error) {
	tenantID, userID, err := s.artifactOwnerContext(ctx)
	if err != nil {
		return nil, err
	}
	artifact, err := s.findOwnedHTMLArtifact(ctx, artifactID, tenantID, userID)
	if err != nil {
		return nil, err
	}
	link, err := s.getOrCreateArtifactShare(ctx, artifact, userID)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(link.PasswordHash) == "" {
		if err := validateArtifactSharePassword(password); err != nil {
			return nil, err
		}
		hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("failed to hash artifact share password: %w", err)
		}
		result := s.db.WithContext(ctx).Model(&ArtifactShareLink{}).
			Where("id = ? AND (password_hash IS NULL OR password_hash = '')", link.ID).
			Update("password_hash", string(hashed))
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected > 0 {
			link.PasswordHash = string(hashed)
		} else if err := s.db.WithContext(ctx).First(link, "id = ?", link.ID).Error; err != nil {
			return nil, err
		}
	}
	return s.artifactShareLinkDTO(link)
}

func (s *Service) artifactOwnerContext(ctx context.Context) (uint64, string, error) {
	if err := requireWebUser(ctx); err != nil {
		return 0, "", err
	}
	if s == nil || s.db == nil {
		return 0, "", fmt.Errorf("chat share service is unavailable")
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	userID := types.SessionOwnerIDFromContext(ctx)
	if !ok || tenantID == 0 || strings.TrimSpace(userID) == "" {
		return 0, "", ErrWebLoginRequired
	}
	return tenantID, strings.TrimSpace(userID), nil
}

func (s *Service) findOwnedHTMLArtifact(ctx context.Context, artifactID string, tenantID uint64, userID string) (artifactRow, error) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return artifactRow{}, fmt.Errorf("artifact_id is required")
	}
	var artifact artifactRow
	if err := s.db.WithContext(ctx).
		Where(
			"id = ? AND tenant_id = ? AND user_id = ? AND storage_state = ? AND deleted_at IS NULL",
			artifactID,
			tenantID,
			userID,
			"ready",
		).
		First(&artifact).Error; err != nil {
		return artifactRow{}, ErrArtifactShareNotFound
	}
	if !isHTMLArtifact(artifact) {
		return artifactRow{}, ErrArtifactShareUnsupported
	}
	return artifact, nil
}

func (s *Service) getOrCreateArtifactShare(
	ctx context.Context,
	artifact artifactRow,
	createdByUserID string,
) (*ArtifactShareLink, error) {
	lookup := func(db *gorm.DB) (*ArtifactShareLink, error) {
		var link ArtifactShareLink
		err := db.WithContext(ctx).
			Where("tenant_id = ? AND artifact_id = ?", artifact.TenantID, artifact.ID).
			First(&link).Error
		if err != nil {
			return nil, err
		}
		return &link, nil
	}

	link, err := lookup(s.db)
	if err == nil {
		return link, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	token, tokenHashValue, err := generateToken()
	if err != nil {
		return nil, err
	}
	tokenCiphertext, err := encryptArtifactShareToken(token)
	if err != nil {
		return nil, err
	}
	link = &ArtifactShareLink{
		TokenHash:       tokenHashValue,
		TokenCiphertext: tokenCiphertext,
		TenantID:        artifact.TenantID,
		SessionID:       artifact.SessionID,
		MessageID:       artifact.MessageID,
		ArtifactID:      artifact.ID,
		Filename:        artifact.FileName,
		FileType:        artifact.FileType,
		FileSize:        artifact.FileSize,
		SHA256:          artifact.SHA256,
		ContentType:     artifact.ContentType,
		CreatedByUserID: strings.TrimSpace(createdByUserID),
		Status:          ShareStatusActive,
	}
	if err := s.db.WithContext(ctx).Create(link).Error; err != nil {
		// A concurrent request may have won the unique artifact key. Re-read
		// the durable link and return it instead of minting another URL.
		if existing, lookupErr := lookup(s.db); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return link, nil
}

// GetArtifactShare keeps the public password/access-token flow for normal
// share URLs. A non-empty preview token switches to the authenticated,
// creator-only preview flow. Normal conversation-share endpoints remain
// authenticated and unchanged.
func (s *Service) GetArtifactShare(ctx context.Context, token, previewToken, accessToken, clientIP string) (*ArtifactShareViewDTO, error) {
	link, err := s.findActiveArtifactShare(ctx, token)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(previewToken) != "" {
		if err := s.authorizeArtifactSharePreview(ctx, link, previewToken); err != nil {
			return nil, err
		}
	}
	artifact, err := s.loadArtifactForShare(ctx, link)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(previewToken) != "" {
		s.recordArtifactShareView(ctx, link.ID)
		view := s.artifactShareViewDTO(link, artifact, token, false)
		view.ContentURL = s.artifactSharePreviewContentURL(token, previewToken)
		return view, nil
	}

	authorized, err := s.authorizeArtifactShare(link, accessToken)
	if errors.Is(err, ErrArtifactSharePasswordRequired) {
		view := s.artifactShareViewDTO(link, artifact, token, true)
		if err := s.populatePasswordAttemptStatus(ctx, link, clientIP, view); err != nil {
			return nil, err
		}
		return view, nil
	}
	if err != nil {
		return nil, err
	}

	if authorized {
		s.recordArtifactShareView(ctx, link.ID)
	}
	return s.artifactShareViewDTO(link, artifact, token, false), nil
}

// GetArtifactShareContent streams only the artifact pinned by the share
// record. With a preview token it requires an authenticated creator; without
// one it preserves the public password/access-token share flow.
func (s *Service) GetArtifactShareContent(ctx context.Context, token, previewToken, accessToken string) (*SharedArtifactFile, error) {
	link, err := s.findActiveArtifactShare(ctx, token)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(previewToken) != "" {
		if err := s.authorizeArtifactSharePreview(ctx, link, previewToken); err != nil {
			return nil, err
		}
	} else if _, err := s.authorizeArtifactShare(link, accessToken); err != nil {
		return nil, err
	}
	artifact, err := s.loadArtifactForShare(ctx, link)
	if err != nil {
		return nil, err
	}
	if s.artifactStore == nil {
		return nil, fmt.Errorf("private artifact object storage is unavailable")
	}
	reader, err := s.artifactStore.Open(ctx, artifact.FilePath)
	if err != nil {
		return nil, ErrArtifactShareNotFound
	}
	return &SharedArtifactFile{
		Reader:      reader,
		FileName:    artifact.FileName,
		ContentType: "text/html; charset=utf-8",
		FileSize:    artifact.FileSize,
	}, nil
}

// VerifyArtifactSharePassword exchanges the creator-provided password for a
// short-lived bearer capability. It does not require a WeKnora account and it
// never returns the stored password hash or plaintext.
func (s *Service) VerifyArtifactSharePassword(ctx context.Context, token, password, clientIP string) (*ArtifactShareAccessDTO, error) {
	link, err := s.findActiveArtifactShare(ctx, token)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadArtifactForShare(ctx, link); err != nil {
		return nil, err
	}
	if strings.TrimSpace(link.PasswordHash) == "" {
		return nil, ErrArtifactShareNotPublished
	}
	if err := validateArtifactSharePassword(password); err != nil {
		return nil, err
	}
	attemptKey := artifactSharePasswordAttemptKey(link.ID, clientIP)
	if status, err := s.passwordAttemptStatus(ctx, attemptKey); err != nil {
		return nil, fmt.Errorf("check artifact share password attempts: %w", err)
	} else if status != nil && status.LockedUntil != nil {
		return nil, &ArtifactSharePasswordAttemptError{Cause: ErrArtifactSharePasswordLocked, Status: status}
	}
	if err := bcrypt.CompareHashAndPassword([]byte(link.PasswordHash), []byte(password)); err != nil {
		status, recordErr := s.recordPasswordAttemptFailure(ctx, attemptKey, clientIP)
		if recordErr != nil {
			return nil, fmt.Errorf("record artifact share password attempt: %w", recordErr)
		}
		cause := error(ErrArtifactSharePasswordInvalid)
		if status != nil && status.LockedUntil != nil {
			cause = ErrArtifactSharePasswordLocked
		}
		return nil, &ArtifactSharePasswordAttemptError{Cause: cause, Status: status}
	}
	if s.authSecurity != nil {
		s.authSecurity.RecordSuccess(ctx, attemptKey)
	}
	accessToken, expiresAt, err := issueArtifactShareCapability(link, artifactShareCapabilityAccess, artifactShareAccessTTL)
	if err != nil {
		return nil, err
	}
	s.recordArtifactShareView(ctx, link.ID)
	return &ArtifactShareAccessDTO{AccessToken: accessToken, ExpiresAt: expiresAt}, nil
}

func (s *Service) artifactShareViewDTO(link *ArtifactShareLink, artifact *artifactRow, token string, requiresPassword bool) *ArtifactShareViewDTO {
	return &ArtifactShareViewDTO{
		ID:               link.ID,
		ArtifactID:       artifact.ID,
		Filename:         artifact.FileName,
		FileType:         artifact.FileType,
		FileSize:         artifact.FileSize,
		ContentType:      "text/html; charset=utf-8",
		ContentURL:       s.artifactShareContentURL(token),
		RequiresPassword: requiresPassword,
		CreatedAt:        link.CreatedAt,
	}
}

func (s *Service) authorizeArtifactShare(link *ArtifactShareLink, accessToken string) (bool, error) {
	if strings.TrimSpace(link.PasswordHash) == "" {
		return false, ErrArtifactShareNotPublished
	}
	if verifyArtifactShareCapability(accessToken, artifactShareCapabilityAccess, link) {
		return true, nil
	}
	return false, ErrArtifactSharePasswordRequired
}

func (s *Service) authorizeArtifactSharePreview(ctx context.Context, link *ArtifactShareLink, previewToken string) error {
	if err := requireWebUser(ctx); err != nil {
		return err
	}
	if !verifyArtifactShareCapability(previewToken, artifactShareCapabilityPreview, link) {
		return ErrArtifactSharePreviewInvalid
	}
	principal, ok := types.PrincipalFromContext(ctx)
	if !ok || principal.Type != types.PrincipalWebUser ||
		strings.TrimSpace(principal.ID) != strings.TrimSpace(link.CreatedByUserID) {
		return ErrArtifactSharePreviewForbidden
	}
	return nil
}

func (s *Service) populatePasswordAttemptStatus(
	ctx context.Context,
	link *ArtifactShareLink,
	clientIP string,
	view *ArtifactShareViewDTO,
) error {
	if view == nil || link == nil {
		return nil
	}
	status, err := s.passwordAttemptStatus(ctx, artifactSharePasswordAttemptKey(link.ID, clientIP))
	if err != nil {
		return fmt.Errorf("read artifact share password attempts: %w", err)
	}
	if status == nil {
		return nil
	}
	view.PasswordAttemptsRemaining = &status.Remaining
	view.PasswordAttemptsMax = status.MaxFailures
	view.PasswordLockedUntil = status.LockedUntil
	return nil
}

func (s *Service) passwordAttemptStatus(ctx context.Context, key string) (*authsecurity.AttemptStatus, error) {
	if s.authSecurity == nil {
		return nil, nil
	}
	return s.authSecurity.GetAttemptStatus(ctx, key)
}

func (s *Service) recordPasswordAttemptFailure(ctx context.Context, key, clientIP string) (*authsecurity.AttemptStatus, error) {
	if s.authSecurity == nil {
		return nil, nil
	}
	return s.authSecurity.RecordAttemptFailure(ctx, key, clientIP)
}

func artifactSharePasswordAttemptKey(shareID, clientIP string) string {
	shareID = strings.TrimSpace(shareID)
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		clientIP = "unknown"
	}
	return "artifact-share:" + shareID + ":" + clientIP
}

func (s *Service) recordArtifactShareView(ctx context.Context, shareID string) {
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&ArtifactShareLink{}).
		Where("id = ?", shareID).
		Updates(map[string]any{
			"view_count":     gorm.Expr("view_count + 1"),
			"last_viewed_at": now,
		}).Error; err != nil {
		logger.Warnf(ctx, "[chatshare] failed to update artifact share view count: share_id=%s err=%v", shareID, err)
	}
}

func (s *Service) findActiveArtifactShare(ctx context.Context, token string) (*ArtifactShareLink, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("chat share service is unavailable")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrArtifactShareNotFound
	}
	var link ArtifactShareLink
	if err := s.db.WithContext(ctx).
		Where("token_hash = ? AND status = ? AND revoked_at IS NULL", tokenHash(token), ShareStatusActive).
		First(&link).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrArtifactShareNotFound
		}
		return nil, err
	}
	return &link, nil
}

func (s *Service) loadArtifactForShare(ctx context.Context, link *ArtifactShareLink) (*artifactRow, error) {
	if link == nil || strings.TrimSpace(link.ArtifactID) == "" {
		return nil, ErrArtifactShareNotFound
	}
	var artifact artifactRow
	if err := s.db.WithContext(ctx).
		Where(
			"id = ? AND tenant_id = ? AND session_id = ? AND storage_state = ? AND deleted_at IS NULL",
			link.ArtifactID,
			link.TenantID,
			link.SessionID,
			"ready",
		).
		First(&artifact).Error; err != nil {
		return nil, ErrArtifactShareNotFound
	}
	if !isHTMLArtifact(artifact) ||
		artifact.FileSize != link.FileSize ||
		strings.TrimSpace(artifact.SHA256) != strings.TrimSpace(link.SHA256) {
		return nil, ErrArtifactShareNotFound
	}
	return &artifact, nil
}

func (s *Service) artifactShareLinkDTO(link *ArtifactShareLink) (*ArtifactShareLinkDTO, error) {
	if link == nil || link.Status != ShareStatusActive || link.RevokedAt != nil {
		return nil, ErrArtifactShareRevoked
	}
	token, err := secutils.DecryptStoredSecret(link.TokenCiphertext)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrArtifactShareTokenUnavailable
	}
	previewToken, _, err := issueArtifactShareCapability(link, artifactShareCapabilityPreview, artifactSharePreviewTTL)
	if err != nil {
		return nil, err
	}
	return &ArtifactShareLinkDTO{
		ID:                 link.ID,
		ArtifactID:         link.ArtifactID,
		Filename:           link.Filename,
		URL:                s.artifactShareURL(token),
		PreviewURL:         s.artifactSharePreviewURL(token, previewToken),
		PasswordConfigured: strings.TrimSpace(link.PasswordHash) != "",
		CreatedAt:          link.CreatedAt,
	}, nil
}

const (
	artifactShareCapabilityPreview = "preview"
	artifactShareCapabilityAccess  = "access"
)

type artifactShareCapabilityClaims struct {
	Kind       string `json:"kind"`
	ShareID    string `json:"share_id"`
	ArtifactID string `json:"artifact_id"`
	OwnerID    string `json:"owner_id,omitempty"`
	TokenHash  string `json:"token_hash,omitempty"`
	ExpiresAt  int64  `json:"expires_at"`
	Nonce      string `json:"nonce"`
}

func issueArtifactShareCapability(link *ArtifactShareLink, kind string, ttl time.Duration) (string, time.Time, error) {
	if link == nil || strings.TrimSpace(link.ID) == "" || strings.TrimSpace(kind) == "" {
		return "", time.Time{}, ErrArtifactShareTokenUnavailable
	}
	key := secutils.GetAESKey()
	if key == nil {
		return "", time.Time{}, ErrArtifactShareTokenUnavailable
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	claims := artifactShareCapabilityClaims{
		Kind:       kind,
		ShareID:    link.ID,
		ArtifactID: link.ArtifactID,
		ExpiresAt:  expiresAt.Unix(),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
	}
	if kind == artifactShareCapabilityPreview {
		claims.OwnerID = link.CreatedByUserID
	} else if kind == artifactShareCapabilityAccess {
		claims.TokenHash = link.TokenHash
	} else {
		return "", time.Time{}, ErrArtifactShareTokenUnavailable
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signedPayload := kind + "." + encodedPayload
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signedPayload + "." + signature, expiresAt, nil
}

func verifyArtifactShareCapability(value, kind string, link *ArtifactShareLink) bool {
	if link == nil || strings.TrimSpace(value) == "" || strings.TrimSpace(kind) == "" {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != kind {
		return false
	}
	key := secutils.GetAESKey()
	if key == nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims artifactShareCapabilityClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Kind != kind {
		return false
	}
	if claims.ExpiresAt <= time.Now().UTC().Unix() ||
		claims.ShareID != link.ID || claims.ArtifactID != link.ArtifactID {
		return false
	}
	if kind == artifactShareCapabilityPreview && claims.OwnerID != link.CreatedByUserID {
		return false
	}
	if kind == artifactShareCapabilityAccess && claims.TokenHash != link.TokenHash {
		return false
	}
	signedPayload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signedPayload))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	return err == nil && hmac.Equal(mac.Sum(nil), signature)
}

func validateArtifactSharePassword(password string) error {
	if strings.TrimSpace(password) == "" || utf8.RuneCountInString(password) < artifactSharePasswordMinRunes {
		return ErrArtifactSharePasswordTooShort
	}
	if len([]byte(password)) > artifactSharePasswordMaxBytes {
		return ErrArtifactSharePasswordTooLong
	}
	return nil
}

func encryptArtifactShareToken(token string) (string, error) {
	key := secutils.GetAESKey()
	if key == nil {
		return "", ErrArtifactShareTokenUnavailable
	}
	return secutils.EncryptAESGCM(token, key)
}

func isHTMLArtifact(artifact artifactRow) bool {
	fileType := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(artifact.FileType)), ".")
	if fileType == "html" || fileType == "htm" {
		return true
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(artifact.ContentType, ";", 2)[0]))
	return contentType == "text/html" || contentType == "application/xhtml+xml"
}

func (s *Service) loadSessionMessages(ctx context.Context, sessionID string) (*types.Session, []types.Message, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	session, err := s.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if session == nil {
		return nil, nil, gorm.ErrRecordNotFound
	}
	var messages []types.Message
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND role IN ?", session.ID, []string{"user", "assistant"}).
		Order("created_at ASC, updated_at ASC, id ASC").
		Find(&messages).Error; err != nil {
		return nil, nil, err
	}
	return session, messages, nil
}

func buildShareTurns(messages []types.Message) []shareTurn {
	turns := make([]shareTurn, 0, (len(messages)+1)/2)
	requestTurnIndex := make(map[string]int)

	for _, message := range messages {
		requestID := strings.TrimSpace(message.RequestID)
		if requestID == "" {
			turns = append(turns, shareTurn{
				ID:       "unmatched:" + message.ID,
				Messages: []types.Message{message},
			})
			continue
		}
		turnID := "request:" + requestID
		index, exists := requestTurnIndex[turnID]
		if !exists {
			index = len(turns)
			requestTurnIndex[turnID] = index
			turns = append(turns, shareTurn{ID: turnID})
		}
		turns[index].Messages = append(turns[index].Messages, message)
	}

	for i := range turns {
		userCount := 0
		assistantCount := 0
		assistantCompleted := false
		assistantHasOutput := false
		for _, message := range turns[i].Messages {
			switch message.Role {
			case "user":
				userCount++
			case "assistant":
				assistantCount++
				assistantCompleted = message.IsCompleted
				assistantHasOutput = strings.TrimSpace(message.Content) != ""
			}
		}
		switch {
		case userCount == 1 && assistantCount == 1 && len(turns[i].Messages) == 2 && assistantCompleted && assistantHasOutput:
			turns[i].Selectable = true
		case userCount == 1 && assistantCount == 1 && !assistantCompleted:
			turns[i].DisabledReason = "回答生成中，完成后可分享"
		case userCount == 1 && assistantCount == 1 && !assistantHasOutput:
			turns[i].DisabledReason = "回答无有效内容，无法分享"
		case userCount > 0 && assistantCount == 0:
			turns[i].DisabledReason = "暂无完整回答，无法分享"
		case userCount == 0 && assistantCount > 0:
			turns[i].DisabledReason = "未找到关联问题，无法分享"
		default:
			turns[i].DisabledReason = "问答关联异常，无法分享"
		}
	}

	return turns
}

func messageSnapshotFromMessage(message types.Message, seq int) MessageSnapshot {
	return MessageSnapshot{
		Seq:                 seq,
		OriginalMessageID:   message.ID,
		SessionID:           message.SessionID,
		RequestID:           message.RequestID,
		Content:             message.Content,
		Role:                message.Role,
		KnowledgeReferences: make(types.References, 0),
		MentionedItems:      append(types.MentionedItems(nil), message.MentionedItems...),
		Images:              append(types.MessageImages(nil), message.Images...),
		Attachments:         append(types.MessageAttachments(nil), message.Attachments...),
		ToolResults:         structuredToolResultsFromAgentSteps(message.AgentSteps),
		IsCompleted:         message.IsCompleted,
		IsFallback:          message.IsFallback,
		Channel:             message.Channel,
		CreatedAt:           message.CreatedAt,
		UpdatedAt:           message.UpdatedAt,
	}
}

func (s *Service) GetShare(ctx context.Context, token string) (*ViewDTO, error) {
	if err := requireWebUser(ctx); err != nil {
		return nil, err
	}
	link, err := s.findActiveLink(ctx, token)
	if err != nil {
		return nil, err
	}

	var messages []MessageSnapshot
	if err := s.db.WithContext(ctx).
		Where("share_id = ?", link.ID).
		Order("seq ASC, created_at ASC, id ASC").
		Find(&messages).Error; err != nil {
		return nil, err
	}
	if err := s.attachStructuredToolResults(ctx, link, messages); err != nil {
		logger.Warnf(ctx, "[chatshare] failed to attach structured tool results: share_id=%s err=%v", link.ID, err)
	}
	if err := s.attachArtifacts(ctx, link, token, messages); err != nil {
		logger.Warnf(ctx, "[chatshare] failed to attach artifacts: share_id=%s err=%v", link.ID, err)
	}
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&Link{}).
		Where("id = ?", link.ID).
		Updates(map[string]any{
			"view_count":     gorm.Expr("view_count + 1"),
			"last_viewed_at": now,
		}).Error; err != nil {
		logger.Warnf(ctx, "[chatshare] failed to update view count: share_id=%s err=%v", link.ID, err)
	}

	return &ViewDTO{
		ID:        link.ID,
		SessionID: link.SessionID,
		Title:     link.Title,
		CreatedAt: link.CreatedAt,
		Messages:  messages,
	}, nil
}

func (s *Service) attachStructuredToolResults(ctx context.Context, link *Link, messages []MessageSnapshot) error {
	if s == nil || s.db == nil || link == nil || len(messages) == 0 {
		return nil
	}
	missingIDs := make([]string, 0)
	for _, message := range messages {
		if len(message.ToolResults) > 0 || strings.TrimSpace(message.OriginalMessageID) == "" {
			continue
		}
		missingIDs = append(missingIDs, message.OriginalMessageID)
	}
	if len(missingIDs) == 0 {
		return nil
	}

	var originals []types.Message
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND id IN ?", link.SessionID, missingIDs).
		Find(&originals).Error; err != nil {
		return err
	}
	if len(originals) == 0 {
		return nil
	}

	byID := make(map[string]ToolResultSnapshots, len(originals))
	for _, message := range originals {
		results := structuredToolResultsFromAgentSteps(message.AgentSteps)
		if len(results) > 0 {
			byID[message.ID] = results
		}
	}
	for i := range messages {
		if len(messages[i].ToolResults) > 0 {
			continue
		}
		if results := byID[messages[i].OriginalMessageID]; len(results) > 0 {
			messages[i].ToolResults = results
		}
	}
	return nil
}

func structuredToolResultsFromAgentSteps(steps types.AgentSteps) ToolResultSnapshots {
	if len(steps) == 0 {
		return nil
	}
	results := make(ToolResultSnapshots, 0)
	seen := make(map[string]bool)
	for _, step := range steps {
		for _, toolCall := range step.ToolCalls {
			if toolCall.Result == nil || !toolCall.Result.Success || toolCall.Result.Data == nil {
				continue
			}
			snapshot, ok := structuredToolResultSnapshot(toolCall.Result)
			if !ok {
				continue
			}
			key := structuredToolResultKey(snapshot)
			if key != "" && seen[key] {
				continue
			}
			if key != "" {
				seen[key] = true
			}
			results = append(results, snapshot)
		}
	}
	return results
}

func structuredToolResultSnapshot(result *types.ToolResult) (ToolResultSnapshot, bool) {
	if result == nil || result.Data == nil {
		return ToolResultSnapshot{}, false
	}
	if strings.TrimSpace(fmt.Sprint(result.Data["display_type"])) != "structured_analysis_result" {
		return ToolResultSnapshot{}, false
	}
	toolData := agenttools.SanitizeToolDataForClient(result.Data)
	if !structuredChartRenderable(toolData) {
		return ToolResultSnapshot{}, false
	}
	return ToolResultSnapshot{
		DisplayType: "structured_analysis_result",
		ToolData:    toolData,
	}, true
}

func structuredChartRenderable(data map[string]interface{}) bool {
	if data == nil || !boolMapField(data, "chart_requested") {
		return false
	}
	chart, ok := data["chart"].(map[string]interface{})
	if !ok || chart == nil {
		return false
	}
	return boolMapField(chart, "eligible")
}

func boolMapField(data map[string]interface{}, key string) bool {
	if data == nil {
		return false
	}
	switch v := data[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

func structuredToolResultKey(result ToolResultSnapshot) string {
	data := result.ToolData
	if data == nil {
		return ""
	}
	chart, _ := data["chart"].(map[string]interface{})
	contract, _ := chart["contract"].(map[string]interface{})
	encoding, _ := contract["encoding"].(map[string]interface{})
	valueEncoding, _ := encoding["value"].(map[string]interface{})
	columns, _ := data["columns"].([]interface{})
	columnNames := make([]string, 0, len(columns))
	for _, item := range columns {
		if col, ok := item.(map[string]interface{}); ok {
			if name := strings.TrimSpace(fmt.Sprint(col["name"])); name != "" {
				columnNames = append(columnNames, name)
			}
		}
	}
	return strings.Join([]string{
		strings.TrimSpace(fmt.Sprint(contract["id"])),
		strings.TrimSpace(fmt.Sprint(contract["type"])),
		strings.TrimSpace(fmt.Sprint(valueEncoding["field"])),
		strings.TrimSpace(fmt.Sprint(data["query"])),
		strings.Join(columnNames, ","),
		strings.TrimSpace(fmt.Sprint(data["row_count"])),
	}, "|")
}

func (s *Service) attachArtifacts(ctx context.Context, link *Link, token string, messages []MessageSnapshot) error {
	if s == nil || s.db == nil || link == nil || len(messages) == 0 {
		return nil
	}
	var allowed []ResourceSnapshot
	if err := s.db.WithContext(ctx).
		Where("share_id = ? AND resource_type = ?", link.ID, ResourceTypeArtifact).
		Find(&allowed).Error; err != nil {
		return err
	}
	if len(allowed) == 0 {
		return nil
	}
	artifactIDs := make([]string, 0, len(allowed))
	for _, resource := range allowed {
		artifactIDs = append(artifactIDs, resource.ResourceKey)
	}
	byMessageID, err := s.artifactSnapshotsByMessage(
		ctx,
		s.db.Where("id IN ?", artifactIDs),
		link.SourceTenantID,
		link.SessionID,
		nil,
		token,
	)
	if err != nil {
		return err
	}
	for i := range messages {
		if artifacts := byMessageID[messages[i].OriginalMessageID]; len(artifacts) > 0 {
			messages[i].Artifacts = artifacts
		}
	}
	return nil
}

func (s *Service) artifactSnapshotsByMessage(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	sessionID string,
	messageIDs []string,
	token string,
) (map[string][]ArtifactSnapshot, error) {
	if db == nil {
		return nil, nil
	}
	query := db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND deleted_at IS NULL", tenantID, sessionID)
	if len(messageIDs) > 0 {
		query = query.Where("message_id IN ?", messageIDs)
	}
	var rows []artifactRow
	if err := query.Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	byMessageID := make(map[string][]ArtifactSnapshot, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.MessageID) == "" {
			continue
		}
		downloadURL := ""
		if token != "" {
			downloadURL = s.shareArtifactURL(token, row.ID)
		}
		byMessageID[row.MessageID] = append(byMessageID[row.MessageID], ArtifactSnapshot{
			ArtifactID:  row.ID,
			Filename:    row.FileName,
			FileType:    row.FileType,
			FileSize:    row.FileSize,
			SHA256:      row.SHA256,
			DownloadURL: downloadURL,
		})
	}
	return byMessageID, nil
}

func (s *Service) buildResourceSnapshots(
	ctx context.Context,
	db *gorm.DB,
	link *Link,
	messages []types.Message,
) ([]ResourceSnapshot, error) {
	if db == nil || link == nil || len(messages) == 0 {
		return nil, nil
	}
	resources := make([]ResourceSnapshot, 0)
	seen := make(map[string]struct{})
	add := func(messageID, resourceType, resourceKey string) {
		resourceKey = strings.TrimSpace(resourceKey)
		if messageID == "" || resourceKey == "" {
			return
		}
		key := resourceType + "\x00" + resourceKey
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		resources = append(resources, ResourceSnapshot{
			ShareID:           link.ID,
			OriginalMessageID: messageID,
			ResourceType:      resourceType,
			ResourceKey:       resourceKey,
		})
	}

	messageIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.ID)
		for _, image := range message.Images {
			if path := canonicalProviderResource(image.URL); path != "" {
				add(message.ID, ResourceTypeFile, path)
			}
		}
		for _, attachment := range message.Attachments {
			if path := canonicalProviderResource(attachment.URL); path != "" {
				add(message.ID, ResourceTypeFile, path)
			}
		}
		for _, path := range providerResourcesFromContent(message.Content) {
			add(message.ID, ResourceTypeFile, path)
		}
	}

	var artifacts []artifactRow
	if err := db.WithContext(ctx).
		Where(
			"tenant_id = ? AND session_id = ? AND message_id IN ? AND deleted_at IS NULL",
			link.SourceTenantID,
			link.SessionID,
			messageIDs,
		).
		Find(&artifacts).Error; err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		add(artifact.MessageID, ResourceTypeArtifact, artifact.ID)
	}
	return resources, nil
}

func canonicalProviderResource(raw string) string {
	decoded := strings.TrimSpace(html.UnescapeString(raw))
	if decoded == "" || types.ParseProviderScheme(decoded) == "" {
		return ""
	}
	return decoded
}

func providerResourcesFromContent(content string) []string {
	decoded := html.UnescapeString(content)
	matches := providerResourcePattern.FindAllString(decoded, -1)
	if len(matches) == 0 {
		return nil
	}
	resources := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		match = strings.TrimRight(strings.TrimSpace(match), ".,;:!?)]}")
		path := canonicalProviderResource(match)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		resources = append(resources, path)
	}
	return resources
}

func (s *Service) resourceAllowed(ctx context.Context, shareID, resourceType, resourceKey string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&ResourceSnapshot{}).
		Where(
			"share_id = ? AND resource_type = ? AND resource_hash = ? AND resource_key = ?",
			shareID,
			resourceType,
			tokenHash(resourceKey),
			resourceKey,
		).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Service) GetSharedFile(ctx context.Context, token string, filePath string) (io.ReadCloser, string, error) {
	if err := requireWebUser(ctx); err != nil {
		return nil, "", err
	}
	link, err := s.findActiveLink(ctx, token)
	if err != nil {
		return nil, "", err
	}
	filePath = strings.TrimSpace(filePath)
	if filePath == "" || strings.Contains(filePath, "..") {
		return nil, "", fmt.Errorf("invalid file_path")
	}
	if err := s.resourceAllowed(ctx, link.ID, ResourceTypeFile, filePath); err != nil {
		return nil, "", err
	}
	if err := validateStoragePathTenant(filePath, link.SourceTenantID); err != nil {
		return nil, "", err
	}
	if s.tenantService == nil {
		return nil, "", fmt.Errorf("tenant service is unavailable")
	}
	tenant, err := s.tenantService.GetTenantByID(ctx, link.SourceTenantID)
	if err != nil {
		return nil, "", err
	}
	if tenant == nil {
		return nil, "", gorm.ErrRecordNotFound
	}

	provider := types.ParseProviderScheme(filePath)
	var (
		fileSvc          interfaces.FileService
		resolvedProvider string
	)
	if tenant.StorageEngineConfig != nil {
		fileSvc, resolvedProvider, err = filesvc.NewFileServiceFromStorageConfig(provider, tenant.StorageEngineConfig, s.localBaseDir)
	} else {
		err = http.ErrMissingFile
	}
	if err != nil {
		globalStorageType := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
		if globalStorageType == "" {
			globalStorageType = "local"
		}
		if provider == globalStorageType && s.globalFileService != nil {
			logger.Warnf(ctx, "[chatshare] tenant storage config invalid, fallback to global file service: tenant_id=%d provider=%s err=%v", tenant.ID, provider, err)
			fileSvc = s.globalFileService
			resolvedProvider = globalStorageType
		} else {
			return nil, "", err
		}
	}

	reader, err := fileSvc.GetFile(ctx, filePath)
	if err != nil {
		logger.Warnf(ctx, "[chatshare] get shared file failed: share_id=%s tenant_id=%d provider=%s path=%q err=%v",
			link.ID, link.SourceTenantID, resolvedProvider, filePath, err)
		return nil, "", err
	}
	return reader, contentTypeForPath(filePath), nil
}

func (s *Service) GetSharedArtifact(ctx context.Context, token string, artifactID string) (*SharedArtifactFile, error) {
	if err := requireWebUser(ctx); err != nil {
		return nil, err
	}
	link, err := s.findActiveLink(ctx, token)
	if err != nil {
		return nil, err
	}
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if err := s.resourceAllowed(ctx, link.ID, ResourceTypeArtifact, artifactID); err != nil {
		return nil, err
	}

	var row artifactRow
	if err := s.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND session_id = ? AND storage_state = ? AND deleted_at IS NULL",
			artifactID, link.SourceTenantID, link.SessionID, "ready").
		First(&row).Error; err != nil {
		return nil, err
	}
	if s.artifactStore == nil {
		return nil, fmt.Errorf("private artifact object storage is unavailable")
	}
	reader, err := s.artifactStore.Open(ctx, row.FilePath)
	if err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	contentType := strings.TrimSpace(row.ContentType)
	if contentType == "" {
		contentType = contentTypeForPath(row.FileName)
	}
	return &SharedArtifactFile{
		Reader:      reader,
		FileName:    row.FileName,
		ContentType: contentType,
		FileSize:    row.FileSize,
	}, nil
}

func (s *Service) findActiveLink(ctx context.Context, token string) (*Link, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("chat share service is unavailable")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}
	hash := tokenHash(token)
	var link Link
	err := s.db.WithContext(ctx).
		Where("token_hash = ? AND status = ? AND revoked_at IS NULL", hash, ShareStatusActive).
		First(&link).Error
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func generateToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	return token, tokenHash(token), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func requireWebUser(ctx context.Context) error {
	principal, ok := types.PrincipalFromContext(ctx)
	if !ok || principal.Type != types.PrincipalWebUser || principal.ID == "" || types.IsSyntheticUserID(principal.ID) {
		return ErrWebLoginRequired
	}
	return nil
}

func (s *Service) shareURL(token string) string {
	path := "/share/chat/" + token
	if s == nil || strings.TrimSpace(s.frontendBaseURL) == "" {
		return path
	}
	return strings.TrimRight(s.frontendBaseURL, "/") + path
}

func (s *Service) shareArtifactURL(token string, artifactID string) string {
	return "/api/v1/custom/chat-share/" + token + "/artifacts/" + artifactID + "/download"
}

func (s *Service) artifactShareURL(token string) string {
	path := "/share/artifact/" + url.PathEscape(strings.TrimSpace(token))
	if s == nil || strings.TrimSpace(s.frontendBaseURL) == "" {
		return path
	}
	return strings.TrimRight(s.frontendBaseURL, "/") + path
}

func (s *Service) artifactSharePreviewURL(token, previewToken string) string {
	path := "/share/artifact/" + url.PathEscape(strings.TrimSpace(token))
	query := url.Values{}
	query.Set("preview", strings.TrimSpace(previewToken))
	path += "?" + query.Encode()
	if s == nil || strings.TrimSpace(s.frontendBaseURL) == "" {
		return path
	}
	return strings.TrimRight(s.frontendBaseURL, "/") + path
}

func (s *Service) artifactShareContentURL(token string) string {
	return "/api/v1/custom/artifact-share/" + url.PathEscape(strings.TrimSpace(token)) + "/content"
}

func (s *Service) artifactSharePreviewContentURL(token, previewToken string) string {
	path := "/api/v1/custom/artifact-share/" + url.PathEscape(strings.TrimSpace(token)) + "/preview/content"
	query := url.Values{}
	query.Set("preview", strings.TrimSpace(previewToken))
	return path + "?" + query.Encode()
}

func contentTypeForPath(filePath string) string {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".csv":
		return "text/csv; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func validateStoragePathTenant(filePath string, tenantID uint64) error {
	pathTenant := parseTenantIDFromStoragePath(filePath)
	if pathTenant == 0 {
		return fmt.Errorf("storage path has no tenant segment")
	}
	if pathTenant != tenantID {
		return fmt.Errorf("storage path tenant mismatch")
	}
	return nil
}

func parseTenantIDFromStoragePath(filePath string) uint64 {
	_, rest, ok := strings.Cut(filePath, "://")
	if !ok {
		return 0
	}
	for _, part := range strings.Split(rest, "/") {
		id, err := strconv.ParseUint(part, 10, 64)
		if err == nil {
			return id
		}
	}
	return 0
}
