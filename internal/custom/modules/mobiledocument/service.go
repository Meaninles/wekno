package mobiledocument

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	defaultDownloadTTL = 2 * time.Minute
	minDownloadTTL     = 30 * time.Second
	maxDownloadTTL     = 10 * time.Minute
	downloadPath       = "/api/v1/custom/mobile-documents/download"
)

var (
	ErrSigningKeyUnavailable = errors.New("mobile document signing key is unavailable")
	ErrInvalidTicket         = errors.New("invalid mobile document download ticket")
	ErrExpiredTicket         = errors.New("mobile document download ticket expired")
	ErrTicketOwnerMismatch   = errors.New("mobile document download ticket owner mismatch")
)

type knowledgeFiles interface {
	GetKnowledgeByIDOnly(ctx context.Context, id string) (*types.Knowledge, error)
	GetKnowledgeFile(ctx context.Context, id string) (io.ReadCloser, string, error)
}

type Config struct {
	SigningKey []byte
	TTL        time.Duration
}

type Ticket struct {
	KnowledgeID string
	TenantID    uint64
	ExpiresAt   time.Time
	Signature   string
}

type Service struct {
	knowledge knowledgeFiles
	key       []byte
	ttl       time.Duration
	now       func() time.Time
}

func LoadConfigFromEnv() Config {
	ttl := defaultDownloadTTL
	if raw := strings.TrimSpace(os.Getenv("CUSTOM_MOBILE_DOCUMENT_DOWNLOAD_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			ttl = parsed
		}
	}
	if ttl < minDownloadTTL {
		ttl = minDownloadTTL
	}
	if ttl > maxDownloadTTL {
		ttl = maxDownloadTTL
	}
	return Config{
		SigningKey: []byte(os.Getenv("SYSTEM_AES_KEY")),
		TTL:        ttl,
	}
}

func NewService(knowledge knowledgeFiles, cfg Config) *Service {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultDownloadTTL
	}
	return &Service{
		knowledge: knowledge,
		key:       append([]byte(nil), cfg.SigningKey...),
		ttl:       ttl,
		now:       time.Now,
	}
}

func (s *Service) Issue(ctx context.Context, knowledgeID string) (string, time.Time, error) {
	if len(s.key) < 16 {
		return "", time.Time{}, ErrSigningKeyUnavailable
	}
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return "", time.Time{}, ErrInvalidTicket
	}
	record, err := s.knowledge.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		return "", time.Time{}, err
	}
	if record == nil || record.ID != knowledgeID || record.TenantID == 0 {
		return "", time.Time{}, ErrInvalidTicket
	}

	expiresAt := s.now().Add(s.ttl).UTC()
	ticket := Ticket{
		KnowledgeID: knowledgeID,
		TenantID:    record.TenantID,
		ExpiresAt:   expiresAt,
	}
	ticket.Signature = s.sign(ticket)

	query := url.Values{}
	query.Set("knowledge_id", ticket.KnowledgeID)
	query.Set("tenant_id", strconv.FormatUint(ticket.TenantID, 10))
	query.Set("expires", strconv.FormatInt(ticket.ExpiresAt.Unix(), 10))
	query.Set("sig", ticket.Signature)
	return downloadPath + "?" + query.Encode(), expiresAt, nil
}

func (s *Service) Resolve(ctx context.Context, values url.Values) (*types.Knowledge, error) {
	ticket, err := parseTicket(values)
	if err != nil {
		return nil, err
	}
	if len(s.key) < 16 || !hmac.Equal([]byte(s.sign(ticket)), []byte(ticket.Signature)) {
		return nil, ErrInvalidTicket
	}
	if s.now().Unix() > ticket.ExpiresAt.Unix() {
		return nil, ErrExpiredTicket
	}

	record, err := s.knowledge.GetKnowledgeByIDOnly(ctx, ticket.KnowledgeID)
	if err != nil {
		return nil, err
	}
	if record == nil || record.ID != ticket.KnowledgeID || record.TenantID != ticket.TenantID {
		return nil, ErrTicketOwnerMismatch
	}
	return record, nil
}

func (s *Service) Open(ctx context.Context, record *types.Knowledge) (io.ReadCloser, string, error) {
	if record == nil || record.ID == "" || record.TenantID == 0 {
		return nil, "", ErrInvalidTicket
	}
	ownerCtx := context.WithValue(ctx, types.TenantIDContextKey, record.TenantID)
	return s.knowledge.GetKnowledgeFile(ownerCtx, record.ID)
}

func (s *Service) sign(ticket Ticket) string {
	payload := fmt.Sprintf(
		"v1\n%s\n%d\n%d",
		ticket.KnowledgeID,
		ticket.TenantID,
		ticket.ExpiresAt.Unix(),
	)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func parseTicket(values url.Values) (Ticket, error) {
	knowledgeID := strings.TrimSpace(values.Get("knowledge_id"))
	tenantID, tenantErr := strconv.ParseUint(strings.TrimSpace(values.Get("tenant_id")), 10, 64)
	expires, expiresErr := strconv.ParseInt(strings.TrimSpace(values.Get("expires")), 10, 64)
	signature := strings.TrimSpace(values.Get("sig"))
	if knowledgeID == "" || tenantErr != nil || tenantID == 0 || expiresErr != nil || expires <= 0 {
		return Ticket{}, ErrInvalidTicket
	}
	if len(signature) != sha256.Size*2 {
		return Ticket{}, ErrInvalidTicket
	}
	if _, err := hex.DecodeString(signature); err != nil {
		return Ticket{}, ErrInvalidTicket
	}
	return Ticket{
		KnowledgeID: knowledgeID,
		TenantID:    tenantID,
		ExpiresAt:   time.Unix(expires, 0).UTC(),
		Signature:   signature,
	}, nil
}
