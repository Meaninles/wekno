package authsecurity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	captchaAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	captchaLength   = 5
	challengePrefix = "custom:authsecurity:challenge:"
)

type Service struct {
	db    *gorm.DB
	redis *redis.Client
	cfg   Config

	mu         sync.Mutex
	challenges map[string]challengeRecord
}

func NewService(db *gorm.DB, redisClient *redis.Client, cfg Config) *Service {
	return &Service{
		db:         db,
		redis:      redisClient,
		cfg:        cfg.normalize(),
		challenges: make(map[string]challengeRecord),
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).AutoMigrate(&LoginAttempt{})
}

func (s *Service) GetPasswordCapability(ctx context.Context, userID string) (*PasswordCapability, error) {
	userID = strings.TrimSpace(userID)
	if s == nil || s.db == nil || userID == "" {
		return nil, errors.New("password capability is unavailable")
	}
	var linkedIAMUsers int64
	if err := s.db.WithContext(ctx).
		Table("custom_iam_users").
		Where("weknora_user_id = ? AND deleted_at IS NULL", userID).
		Count(&linkedIAMUsers).Error; err != nil {
		return nil, fmt.Errorf("check IAM account linkage: %w", err)
	}
	if linkedIAMUsers > 0 {
		return &PasswordCapability{
			AccountSource:     "iam",
			CanChangePassword: false,
			Reason:            "密码由 IAM 统一身份认证系统管理",
		}, nil
	}
	return &PasswordCapability{
		AccountSource:     "local",
		CanChangePassword: true,
	}, nil
}

func (s *Service) NewChallenge(ctx context.Context) (*ChallengeResponse, error) {
	if s == nil {
		return nil, errors.New("auth security service is unavailable")
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate login key: %w", err)
	}
	publicKeyPEM, err := encodePublicKeyPEM(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	privateKeyPEM := encodePrivateKeyPEM(privateKey)

	answer, err := randomCaptchaText(captchaLength)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	record := challengeRecord{
		ID:                id,
		PrivateKeyPEM:     privateKeyPEM,
		CaptchaAnswerHash: captchaHash(id, answer),
		ExpiresAt:         time.Now().UTC().Add(s.cfg.ChallengeTTL),
	}
	if err := s.storeChallenge(ctx, record); err != nil {
		return nil, err
	}

	return &ChallengeResponse{
		ChallengeID:      id,
		PublicKey:        publicKeyPEM,
		CaptchaImage:     captchaDataURI(answer),
		ExpiresInSeconds: int(s.cfg.ChallengeTTL.Seconds()),
	}, nil
}

func (s *Service) DecryptPassword(ctx context.Context, challengeID, encryptedPassword, captchaAnswer string) (string, error) {
	passwords, err := s.DecryptPasswords(ctx, challengeID, captchaAnswer, encryptedPassword)
	if err != nil {
		return "", err
	}
	if len(passwords) == 0 {
		return "", apperrors.NewValidationError("密码密文无效，请刷新后重试")
	}
	return passwords[0], nil
}

func (s *Service) DecryptPasswords(ctx context.Context, challengeID, captchaAnswer string, encryptedPasswords ...string) ([]string, error) {
	record, err := s.consumeChallenge(ctx, challengeID)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(record.ExpiresAt) {
		return nil, apperrors.NewValidationError("验证码已过期，请刷新后重试")
	}
	if record.CaptchaAnswerHash == "" || record.CaptchaAnswerHash != captchaHash(record.ID, captchaAnswer) {
		return nil, apperrors.NewValidationError("验证码错误，请重新输入")
	}
	block, _ := pem.Decode([]byte(record.PrivateKeyPEM))
	if block == nil {
		return nil, apperrors.NewValidationError("认证挑战无效，请刷新后重试")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, apperrors.NewValidationError("认证挑战无效，请刷新后重试")
	}
	passwords := make([]string, 0, len(encryptedPasswords))
	for _, encryptedPassword := range encryptedPasswords {
		ciphertext, err := decodeBase64(encryptedPassword)
		if err != nil {
			return nil, apperrors.NewValidationError("密码密文格式无效")
		}
		plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, ciphertext, nil)
		if err != nil {
			logger.Warnf(ctx, "[authsecurity] password decrypt failed: %v", err)
			return nil, apperrors.NewValidationError("密码密文无效，请刷新后重试")
		}
		password := string(plaintext)
		if strings.TrimSpace(password) == "" || len(password) > 512 {
			return nil, apperrors.NewValidationError("密码密文无效，请刷新后重试")
		}
		passwords = append(passwords, password)
	}
	return passwords, nil
}

func (s *Service) CheckLocked(ctx context.Context, username string) (*time.Time, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	key := normalizeUsername(username)
	if key == "" {
		return nil, nil
	}
	var attempt LoginAttempt
	err := s.db.WithContext(ctx).First(&attempt, "username_key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if attempt.LockedUntil != nil && attempt.LockedUntil.After(time.Now().UTC()) {
		return attempt.LockedUntil, nil
	}
	return nil, nil
}

func (s *Service) RecordSuccess(ctx context.Context, username string) {
	if s == nil || s.db == nil {
		return
	}
	key := normalizeUsername(username)
	if key == "" {
		return
	}
	if err := s.db.WithContext(ctx).Delete(&LoginAttempt{}, "username_key = ?", key).Error; err != nil {
		logger.Warnf(ctx, "[authsecurity] failed to clear login attempts for %q: %v", key, err)
	}
}

func (s *Service) RecordFailure(ctx context.Context, username, ip string) (*time.Time, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	key := normalizeUsername(username)
	if key == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	var lockedUntil *time.Time
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locking := func(db *gorm.DB) *gorm.DB {
			switch tx.Dialector.Name() {
			case "postgres", "mysql":
				return db.Clauses(clause.Locking{Strength: "UPDATE"})
			default:
				return db
			}
		}
		var attempt LoginAttempt
		err := locking(tx).First(&attempt, "username_key = ?", key).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			attempt = LoginAttempt{UsernameKey: key}
		} else if err != nil {
			return err
		}
		if attempt.LockedUntil != nil && attempt.LockedUntil.After(now) {
			lockedUntil = attempt.LockedUntil
			return nil
		}
		if attempt.LastFailedAt == nil || now.Sub(*attempt.LastFailedAt) > s.cfg.FailureWindow {
			attempt.FailedCount = 0
		}
		attempt.FailedCount++
		attempt.LastFailedAt = &now
		attempt.LastIP = ip
		if attempt.FailedCount >= s.cfg.MaxFailures {
			until := now.Add(s.cfg.LockDuration)
			attempt.LockedUntil = &until
			lockedUntil = &until
		} else {
			attempt.LockedUntil = nil
		}
		return tx.Save(&attempt).Error
	})
	return lockedUntil, err
}

func (s *Service) storeChallenge(ctx context.Context, record challengeRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if s.redis != nil {
		if err := s.redis.Set(ctx, challengePrefix+record.ID, data, s.cfg.ChallengeTTL).Err(); err == nil {
			return nil
		} else {
			logger.Warnf(ctx, "[authsecurity] redis challenge store failed, using memory fallback: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredChallengesLocked(time.Now().UTC())
	s.challenges[record.ID] = record
	return nil
}

func (s *Service) consumeChallenge(ctx context.Context, id string) (challengeRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return challengeRecord{}, apperrors.NewValidationError("请刷新验证码后重试")
	}
	if s.redis != nil {
		data, err := s.redis.GetDel(ctx, challengePrefix+id).Bytes()
		if err == nil {
			var record challengeRecord
			if err := json.Unmarshal(data, &record); err != nil {
				return challengeRecord{}, apperrors.NewValidationError("登录挑战无效，请刷新后重试")
			}
			return record, nil
		}
		if err != redis.Nil {
			logger.Warnf(ctx, "[authsecurity] redis challenge consume failed, trying memory fallback: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredChallengesLocked(time.Now().UTC())
	record, ok := s.challenges[id]
	if !ok {
		return challengeRecord{}, apperrors.NewValidationError("验证码已过期，请刷新后重试")
	}
	delete(s.challenges, id)
	return record, nil
}

func (s *Service) cleanupExpiredChallengesLocked(now time.Time) {
	for id, record := range s.challenges {
		if now.After(record.ExpiresAt) {
			delete(s.challenges, id)
		}
	}
}

func encodePrivateKeyPEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func encodePublicKeyPEM(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	})), nil
}

func randomCaptchaText(length int) (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(captchaAlphabet)))
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(captchaAlphabet[n.Int64()])
	}
	return b.String(), nil
}

func captchaHash(challengeID, answer string) string {
	normalized := strings.ToUpper(strings.TrimSpace(answer))
	sum := sha256.Sum256([]byte(challengeID + ":" + normalized))
	return hex.EncodeToString(sum[:])
}

func captchaDataURI(answer string) string {
	escaped := html.EscapeString(answer)
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="142" height="46" viewBox="0 0 142 46">
<rect width="142" height="46" rx="6" fill="#f3f4f6"/>
<path d="M8 34 C28 12, 45 44, 66 20 S109 12, 134 31" fill="none" stroke="#0f766e" stroke-width="2" opacity=".55"/>
<path d="M10 14 L132 38" stroke="#94a3b8" stroke-width="1" opacity=".45"/>
<path d="M18 39 L124 9" stroke="#cbd5e1" stroke-width="1" opacity=".55"/>
<text x="18" y="31" font-family="Consolas, Menlo, monospace" font-size="24" font-weight="700" fill="#111827" letter-spacing="5">%s</text>
<circle cx="25" cy="11" r="2" fill="#0f766e" opacity=".5"/>
<circle cx="92" cy="37" r="2" fill="#64748b" opacity=".55"/>
<circle cx="119" cy="17" r="1.8" fill="#0f766e" opacity=".45"/>
</svg>`, escaped)
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

func decodeBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("empty ciphertext")
	}
	if data, err := base64.StdEncoding.DecodeString(value); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
