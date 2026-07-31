package modeladmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/types"
)

type ResourcePool struct {
	ID                    string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	Name                  string    `json:"name" gorm:"type:varchar(128);not null"`
	ResourceKind          string    `json:"resource_kind" gorm:"type:varchar(32);not null;index"`
	MaxInflight           int       `json:"max_inflight" gorm:"not null"`
	MaxBackgroundInflight int       `json:"max_background_inflight" gorm:"not null"`
	InteractiveReserve    int       `json:"interactive_reserve" gorm:"not null"`
	TenantGuaranteed      int       `json:"tenant_guaranteed" gorm:"not null;default:1"`
	TenantBurst           int       `json:"tenant_burst" gorm:"not null"`
	DocumentGuaranteed    int       `json:"document_guaranteed" gorm:"not null;default:1"`
	DocumentBurst         int       `json:"document_burst" gorm:"not null;default:2"`
	RPM                   int       `json:"rpm" gorm:"not null;default:0"`
	TPM                   int64     `json:"tpm" gorm:"not null;default:0"`
	TokenBurst            int64     `json:"token_burst" gorm:"not null;default:0"`
	RequestTimeoutSeconds int       `json:"request_timeout_seconds" gorm:"not null;default:900"`
	CircuitThreshold      int       `json:"circuit_threshold" gorm:"not null;default:3"`
	CircuitWindowSeconds  int       `json:"circuit_window_seconds" gorm:"not null;default:600"`
	CircuitOpenSeconds    int       `json:"circuit_open_seconds" gorm:"not null;default:60"`
	State                 string    `json:"state" gorm:"type:varchar(24);not null;default:'enabled';index"`
	PolicyVersion         uint64    `json:"policy_version" gorm:"not null;default:1"`
	CreatedAt             time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt             time.Time `json:"updated_at" gorm:"not null"`
}

func (ResourcePool) TableName() string { return "custom_model_resource_pools" }

type ResourceBinding struct {
	ModelID          string    `json:"model_id" gorm:"type:varchar(64);primaryKey"`
	ModelTenantID    uint64    `json:"model_tenant_id" gorm:"primaryKey"`
	ResourcePoolID   string    `json:"resource_pool_id" gorm:"type:varchar(64);not null;index"`
	QuotaPoolID      string    `json:"quota_pool_id" gorm:"type:varchar(64);not null;default:''"`
	GatewayPoolID    string    `json:"gateway_pool_id" gorm:"type:varchar(64);not null;default:''"`
	RouteFingerprint string    `json:"route_fingerprint" gorm:"type:varchar(64);not null;index"`
	BindingVersion   uint64    `json:"binding_version" gorm:"not null;default:1"`
	CreatedAt        time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt        time.Time `json:"updated_at" gorm:"not null"`
}

func (ResourceBinding) TableName() string { return "custom_model_resource_bindings" }

type QuotaPool struct {
	ID            string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	Name          string    `json:"name" gorm:"type:varchar(128);not null"`
	RPM           int       `json:"rpm" gorm:"not null;default:0"`
	TPM           int64     `json:"tpm" gorm:"not null;default:0"`
	TokenBurst    int64     `json:"token_burst" gorm:"not null;default:0"`
	State         string    `json:"state" gorm:"type:varchar(24);not null;default:'enabled'"`
	PolicyVersion uint64    `json:"policy_version" gorm:"not null;default:1"`
	CreatedAt     time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt     time.Time `json:"updated_at" gorm:"not null"`
}

func (QuotaPool) TableName() string { return "custom_model_quota_pools" }

type GatewayPool struct {
	ID            string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	Name          string    `json:"name" gorm:"type:varchar(128);not null"`
	MaxInflight   int       `json:"max_inflight" gorm:"not null;default:32"`
	State         string    `json:"state" gorm:"type:varchar(24);not null;default:'enabled'"`
	PolicyVersion uint64    `json:"policy_version" gorm:"not null;default:1"`
	CreatedAt     time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt     time.Time `json:"updated_at" gorm:"not null"`
}

func (GatewayPool) TableName() string { return "custom_model_gateway_pools" }

type AdmissionTemplate struct {
	Kind                  string    `json:"kind" gorm:"type:varchar(32);primaryKey"`
	MaxInflight           int       `json:"max_inflight" gorm:"not null"`
	MaxBackgroundInflight int       `json:"max_background_inflight" gorm:"not null"`
	InteractiveReserve    int       `json:"interactive_reserve" gorm:"not null"`
	TenantBurst           int       `json:"tenant_burst" gorm:"not null"`
	DocumentBurst         int       `json:"document_burst" gorm:"not null"`
	RPM                   int       `json:"rpm" gorm:"not null;default:0"`
	TPM                   int64     `json:"tpm" gorm:"not null;default:0"`
	RequestTimeoutSeconds int       `json:"request_timeout_seconds" gorm:"not null;default:900"`
	CircuitThreshold      int       `json:"circuit_threshold" gorm:"not null;default:3"`
	CircuitWindowSeconds  int       `json:"circuit_window_seconds" gorm:"not null;default:600"`
	CircuitOpenSeconds    int       `json:"circuit_open_seconds" gorm:"not null;default:60"`
	PolicyVersion         uint64    `json:"policy_version" gorm:"not null;default:1"`
	UpdatedBy             string    `json:"updated_by" gorm:"type:varchar(36);not null;default:''"`
	CreatedAt             time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt             time.Time `json:"updated_at" gorm:"not null"`
}

func (AdmissionTemplate) TableName() string { return "custom_model_admission_templates" }

type AdmissionAudit struct {
	ID            uint64    `json:"id" gorm:"primaryKey;autoIncrement"`
	ActorID       string    `json:"actor_id" gorm:"type:varchar(36);not null;default:''"`
	Action        string    `json:"action" gorm:"type:varchar(48);not null;index"`
	ResourceType  string    `json:"resource_type" gorm:"type:varchar(32);not null"`
	ResourceID    string    `json:"resource_id" gorm:"type:varchar(64);not null;index"`
	OldValue      string    `json:"old_value" gorm:"type:text;not null;default:'{}'"`
	NewValue      string    `json:"new_value" gorm:"type:text;not null;default:'{}'"`
	PolicyVersion uint64    `json:"policy_version" gorm:"not null;default:0"`
	CreatedAt     time.Time `json:"created_at" gorm:"not null;index"`
}

func (AdmissionAudit) TableName() string { return "custom_model_admission_audits" }

type ResolvedPolicy struct {
	PoolID           string
	QuotaPoolID      string
	GatewayPoolID    string
	Limit            Limit
	QuotaLimit       Limit
	GatewayLimit     Limit
	TPM              int64
	TokenBurst       int64
	QuotaTPM         int64
	QuotaTokenBurst  int64
	RequestTimeout   time.Duration
	CircuitThreshold int
	CircuitWindow    time.Duration
	CircuitOpen      time.Duration
	PolicyVersion    uint64
	Source           string
}

type Store struct {
	db *gorm.DB

	mu       sync.Mutex
	cache    map[string]cachedPolicy
	cacheTTL time.Duration
}

type cachedPolicy struct {
	policy    ResolvedPolicy
	expiresAt time.Time
}

func NewStore(db *gorm.DB) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db, cache: make(map[string]cachedPolicy), cacheTTL: time.Second}
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("model admission database is unavailable")
	}
	if err := s.db.WithContext(ctx).AutoMigrate(
		&ResourcePool{},
		&ResourceBinding{},
		&QuotaPool{},
		&GatewayPool{},
		&AdmissionTemplate{},
		&AdmissionAudit{},
	); err != nil {
		return fmt.Errorf("migrate model admission control plane: %w", err)
	}
	if err := s.seedBuiltinTemplates(ctx); err != nil {
		return err
	}
	return s.ReconcileModels(ctx)
}

func (s *Store) ReconcileModels(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("model admission database is unavailable")
	}
	var models []types.Model
	if err := s.db.WithContext(ctx).Where("deleted_at IS NULL").Find(&models).Error; err != nil {
		return fmt.Errorf("list models for resource-pool reconciliation: %w", err)
	}
	var templateRows []AdmissionTemplate
	if err := s.db.WithContext(ctx).Find(&templateRows).Error; err != nil {
		return fmt.Errorf("list admission templates: %w", err)
	}
	templates := make(map[string]AdmissionTemplate, len(templateRows))
	for _, template := range templateRows {
		templates[template.Kind] = template
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for index := range models {
			model := &models[index]
			fingerprint := RouteFingerprint(model)
			poolID := AutoPoolID(fingerprint)
			policy := builtinPolicy(
				kindForModelType(model.Type),
				model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly,
			)
			templateName := string(policy.kind)
			if model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
				templateName = string(KindDerivative)
			}
			if template, ok := templates[templateName]; ok {
				policy = templatePolicy(template, policy.kind)
			}
			resourceKind := string(policy.kind)
			if model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
				resourceKind = string(KindDerivative)
			}
			pool := ResourcePool{
				ID: poolID, Name: actualModelName(model), ResourceKind: resourceKind,
				MaxInflight:           policy.limit.Concurrency,
				MaxBackgroundInflight: policy.limit.Background,
				InteractiveReserve:    policy.reserve,
				TenantGuaranteed:      1, TenantBurst: policy.limit.PerTenant,
				DocumentGuaranteed: 1, DocumentBurst: policy.limit.PerDocument,
				RPM: policy.limit.RPM, TPM: policy.tpm,
				RequestTimeoutSeconds: 900,
				CircuitThreshold:      3, CircuitWindowSeconds: 600, CircuitOpenSeconds: 60,
				State: "enabled", PolicyVersion: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&pool).Error; err != nil {
				return err
			}
			// policy_version=1 marks an auto-managed pool. A manual pool edit
			// increments the version and is never overwritten by reconciliation.
			if err := tx.Model(&ResourcePool{}).
				Where("id = ? AND policy_version = 1", pool.ID).
				Updates(map[string]any{
					"name": pool.Name, "resource_kind": pool.ResourceKind,
					"max_inflight":            pool.MaxInflight,
					"max_background_inflight": pool.MaxBackgroundInflight,
					"interactive_reserve":     pool.InteractiveReserve,
					"tenant_burst":            pool.TenantBurst,
					"document_burst":          pool.DocumentBurst,
					"rpm":                     pool.RPM, "tpm": pool.TPM,
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
			binding := ResourceBinding{
				ModelID: model.ID, ModelTenantID: model.TenantID,
				ResourcePoolID: poolID, RouteFingerprint: fingerprintDigest(fingerprint),
				BindingVersion: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Resolve(ctx context.Context, spec Spec, fallback Limit) (ResolvedPolicy, error) {
	auto := builtinPolicy(spec.Kind, spec.DerivativeOnly)
	if spec.RouteFingerprint == "" {
		if fallback.Concurrency > 0 || fallback.RPM > 0 || fallback.PerTenant > 0 {
			auto.limit = fallback
		}
		return resolvedBuiltinPolicy(spec.Domain, auto, "environment"), nil
	}
	cacheKey := fmt.Sprintf("%d/%s/%s", spec.ModelTenantID, spec.ModelID, spec.RouteFingerprint)
	if policy, ok := s.cached(cacheKey); ok {
		return policy, nil
	}
	if s == nil || s.db == nil {
		return resolvedBuiltinPolicy(
			AutoPoolID(spec.RouteFingerprint), auto, "builtin",
		), nil
	}

	var binding ResourceBinding
	err := s.db.WithContext(ctx).
		Where("model_id = ? AND model_tenant_id = ?", spec.ModelID, spec.ModelTenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		source := "builtin"
		if template, ok := s.template(ctx, templateKind(spec)); ok {
			auto = templatePolicy(template, spec.Kind)
			source = "template"
		}
		policy := resolvedBuiltinPolicy(
			AutoPoolID(spec.RouteFingerprint), auto, source,
		)
		s.put(cacheKey, policy)
		return policy, nil
	}
	if err != nil {
		return ResolvedPolicy{}, err
	}
	var pool ResourcePool
	if err := s.db.WithContext(ctx).Where("id = ?", binding.ResourcePoolID).First(&pool).Error; err != nil {
		return ResolvedPolicy{}, err
	}
	if pool.State == "disabled" || pool.State == "draining" {
		return ResolvedPolicy{}, fmt.Errorf("model resource pool %s is %s", pool.ID, pool.State)
	}
	policy := ResolvedPolicy{
		PoolID: pool.ID, QuotaPoolID: binding.QuotaPoolID, GatewayPoolID: binding.GatewayPoolID,
		Limit: Limit{
			Concurrency: pool.MaxInflight,
			Background:  pool.MaxBackgroundInflight,
			RPM:         pool.RPM,
			PerTenant:   pool.TenantBurst,
			PerDocument: pool.DocumentBurst,
		},
		TPM:              pool.TPM,
		TokenBurst:       pool.TokenBurst,
		RequestTimeout:   secondsDuration(pool.RequestTimeoutSeconds, 900),
		CircuitThreshold: positiveInt(pool.CircuitThreshold, 3),
		CircuitWindow:    secondsDuration(pool.CircuitWindowSeconds, 600),
		CircuitOpen:      secondsDuration(pool.CircuitOpenSeconds, 60),
		PolicyVersion:    pool.PolicyVersion,
		Source:           "resource_pool",
	}
	if binding.QuotaPoolID != "" {
		var quota QuotaPool
		if err := s.db.WithContext(ctx).Where("id = ?", binding.QuotaPoolID).First(&quota).Error; err != nil {
			return ResolvedPolicy{}, fmt.Errorf("load quota pool %s: %w", binding.QuotaPoolID, err)
		}
		if quota.State != "enabled" {
			return ResolvedPolicy{}, fmt.Errorf("model quota pool %s is %s", quota.ID, quota.State)
		}
		policy.QuotaLimit = Limit{RPM: quota.RPM}
		policy.QuotaTPM = quota.TPM
		policy.QuotaTokenBurst = quota.TokenBurst
	}
	if binding.GatewayPoolID != "" {
		var gateway GatewayPool
		if err := s.db.WithContext(ctx).Where("id = ?", binding.GatewayPoolID).First(&gateway).Error; err != nil {
			return ResolvedPolicy{}, fmt.Errorf("load gateway pool %s: %w", binding.GatewayPoolID, err)
		}
		if gateway.State != "enabled" {
			return ResolvedPolicy{}, fmt.Errorf("model gateway pool %s is %s", gateway.ID, gateway.State)
		}
		policy.GatewayLimit = Limit{Concurrency: gateway.MaxInflight}
	}
	s.put(cacheKey, policy)
	return policy, nil
}

func resolvedBuiltinPolicy(
	poolID string,
	policy builtinPolicyValue,
	source string,
) ResolvedPolicy {
	return ResolvedPolicy{
		PoolID:           poolID,
		Limit:            policy.limit,
		TPM:              policy.tpm,
		RequestTimeout:   15 * time.Minute,
		CircuitThreshold: 3,
		CircuitWindow:    10 * time.Minute,
		CircuitOpen:      time.Minute,
		Source:           source,
	}
}

func positiveInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func secondsDuration(seconds, fallback int) time.Duration {
	return time.Duration(positiveInt(seconds, fallback)) * time.Second
}

func (s *Store) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cache = make(map[string]cachedPolicy)
	s.mu.Unlock()
}

func (s *Store) cached(key string) (ResolvedPolicy, bool) {
	if s == nil {
		return ResolvedPolicy{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.cache[key]
	if !ok || time.Now().After(item.expiresAt) {
		delete(s.cache, key)
		return ResolvedPolicy{}, false
	}
	return item.policy, true
}

func (s *Store) put(key string, policy ResolvedPolicy) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cache[key] = cachedPolicy{policy: policy, expiresAt: time.Now().Add(s.cacheTTL)}
	s.mu.Unlock()
}

type builtinPolicyValue struct {
	kind    Kind
	limit   Limit
	reserve int
	tpm     int64
}

func builtinPolicy(kind Kind, derivative bool) builtinPolicyValue {
	if derivative || kind == KindDerivative {
		return builtinPolicyValue{
			kind:  KindDerivative,
			limit: Limit{Concurrency: 2, Background: 2, PerTenant: 2, PerDocument: 2},
			tpm:   20_000,
		}
	}
	switch kind {
	case KindChat:
		return builtinPolicyValue{
			kind:    kind,
			limit:   Limit{Concurrency: 4, Background: 3, PerTenant: 4, PerDocument: 2},
			reserve: 1,
		}
	case KindEmbedding, KindRerank:
		return builtinPolicyValue{
			kind:  kind,
			limit: Limit{Concurrency: 8, Background: 8, PerTenant: 8, PerDocument: 2},
		}
	case KindVLM, KindASR:
		return builtinPolicyValue{
			kind:  kind,
			limit: Limit{Concurrency: 2, Background: 2, PerTenant: 2, PerDocument: 2},
		}
	case KindParser:
		return builtinPolicyValue{
			kind:  kind,
			limit: Limit{Concurrency: 4, Background: 4, PerTenant: 4, PerDocument: 2},
		}
	default:
		return builtinPolicyValue{
			kind:  kind,
			limit: Limit{Concurrency: 1, Background: 1, PerTenant: 1, PerDocument: 1},
		}
	}
}

func kindForModelType(modelType types.ModelType) Kind {
	switch modelType {
	case types.ModelTypeEmbedding:
		return KindEmbedding
	case types.ModelTypeRerank:
		return KindRerank
	case types.ModelTypeVLLM:
		return KindVLM
	case types.ModelTypeASR:
		return KindASR
	default:
		return KindChat
	}
}

func actualModelName(model *types.Model) string {
	if model == nil {
		return "unknown"
	}
	if name := strings.TrimSpace(model.Parameters.ExtraConfig["remote_model_name"]); name != "" {
		return name
	}
	if name := strings.TrimSpace(model.DisplayName); name != "" {
		return name
	}
	if name := strings.TrimSpace(model.Name); name != "" {
		return name
	}
	return model.ID
}

func AutoPoolID(fingerprint string) string {
	return "auto-" + fingerprintDigest(fingerprint)[:24]
}

func fingerprintDigest(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:])
}

func (s *Store) seedBuiltinTemplates(ctx context.Context) error {
	now := time.Now().UTC()
	rows := make([]AdmissionTemplate, 0, 7)
	for _, kind := range []Kind{
		KindChat, KindDerivative, KindEmbedding, KindRerank, KindVLM, KindASR, KindParser,
	} {
		policy := builtinPolicy(kind, kind == KindDerivative)
		rows = append(rows, AdmissionTemplate{
			Kind:                  kind.String(),
			MaxInflight:           policy.limit.Concurrency,
			MaxBackgroundInflight: policy.limit.Background,
			InteractiveReserve:    policy.reserve,
			TenantBurst:           policy.limit.PerTenant,
			DocumentBurst:         policy.limit.PerDocument,
			RPM:                   policy.limit.RPM,
			TPM:                   policy.tpm,
			RequestTimeoutSeconds: 900,
			CircuitThreshold:      3,
			CircuitWindowSeconds:  600,
			CircuitOpenSeconds:    60,
			PolicyVersion:         1,
			CreatedAt:             now,
			UpdatedAt:             now,
		})
	}
	if err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rows).Error; err != nil {
		return fmt.Errorf("seed built-in model admission templates: %w", err)
	}
	return nil
}

func (kind Kind) String() string {
	return string(kind)
}

func templateKind(spec Spec) string {
	if spec.DerivativeOnly {
		return string(KindDerivative)
	}
	return string(spec.Kind)
}

func (s *Store) template(ctx context.Context, kind string) (AdmissionTemplate, bool) {
	if s == nil || s.db == nil || strings.TrimSpace(kind) == "" {
		return AdmissionTemplate{}, false
	}
	var row AdmissionTemplate
	if err := s.db.WithContext(ctx).Where("kind = ?", kind).First(&row).Error; err != nil {
		return AdmissionTemplate{}, false
	}
	return row, true
}

func templatePolicy(template AdmissionTemplate, callKind Kind) builtinPolicyValue {
	policyKind := callKind
	if template.Kind == string(KindDerivative) {
		policyKind = KindDerivative
	}
	return builtinPolicyValue{
		kind: policyKind,
		limit: Limit{
			Concurrency: template.MaxInflight,
			Background:  template.MaxBackgroundInflight,
			RPM:         template.RPM,
			PerTenant:   template.TenantBurst,
			PerDocument: template.DocumentBurst,
		},
		reserve: template.InteractiveReserve,
		tpm:     template.TPM,
	}
}

var safePoolID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,63}$`)

func ValidatePool(pool *ResourcePool) error {
	if pool == nil {
		return errors.New("resource pool is required")
	}
	pool.ID = strings.TrimSpace(pool.ID)
	pool.Name = strings.TrimSpace(pool.Name)
	pool.ResourceKind = strings.TrimSpace(pool.ResourceKind)
	if !safePoolID.MatchString(pool.ID) {
		return errors.New("resource pool id contains unsupported characters or exceeds 64 characters")
	}
	if pool.Name == "" || pool.ResourceKind == "" {
		return errors.New("resource pool name and resource_kind are required")
	}
	if pool.MaxInflight < 1 || pool.MaxInflight > 1024 {
		return errors.New("max_inflight must be between 1 and 1024")
	}
	if pool.MaxBackgroundInflight < 0 || pool.MaxBackgroundInflight > pool.MaxInflight {
		return errors.New("max_background_inflight must be between 0 and max_inflight")
	}
	if pool.InteractiveReserve < 0 || pool.InteractiveReserve > pool.MaxInflight {
		return errors.New("interactive_reserve must be between 0 and max_inflight")
	}
	if pool.TenantBurst < 1 || pool.TenantBurst > pool.MaxInflight {
		return errors.New("tenant_burst must be between 1 and max_inflight")
	}
	if pool.TenantGuaranteed < 1 || pool.TenantGuaranteed > pool.TenantBurst {
		return errors.New("tenant_guaranteed must be between 1 and tenant_burst")
	}
	if pool.DocumentBurst < 1 || pool.DocumentBurst > pool.MaxInflight {
		return errors.New("document_burst must be between 1 and max_inflight")
	}
	if pool.DocumentGuaranteed < 1 || pool.DocumentGuaranteed > pool.DocumentBurst {
		return errors.New("document_guaranteed must be between 1 and document_burst")
	}
	if pool.RPM < 0 || pool.TPM < 0 || pool.TokenBurst < 0 {
		return errors.New("rpm, tpm, and token_burst cannot be negative")
	}
	if pool.RequestTimeoutSeconds < 1 || pool.RequestTimeoutSeconds > 7200 {
		return errors.New("request_timeout_seconds must be between 1 and 7200")
	}
	if pool.CircuitThreshold < 1 || pool.CircuitThreshold > 100 {
		return errors.New("circuit_threshold must be between 1 and 100")
	}
	if pool.CircuitWindowSeconds < 1 || pool.CircuitWindowSeconds > 86400 {
		return errors.New("circuit_window_seconds must be between 1 and 86400")
	}
	if pool.CircuitOpenSeconds < 1 || pool.CircuitOpenSeconds > 86400 {
		return errors.New("circuit_open_seconds must be between 1 and 86400")
	}
	switch pool.State {
	case "", "enabled":
		pool.State = "enabled"
	case "draining", "disabled":
	default:
		return errors.New("state must be enabled, draining, or disabled")
	}
	return nil
}
