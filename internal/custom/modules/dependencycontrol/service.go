package dependencycontrol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultProbeInterval = 15 * time.Second
	defaultRetryAfter    = 30 * time.Second
)

type cachedCapability struct {
	row       Capability
	expiresAt time.Time
}

type Service struct {
	db      *gorm.DB
	profile runtimeprofile.Profile

	mu     sync.Mutex
	cache  map[string]cachedCapability
	cancel context.CancelFunc
	done   chan struct{}

	repairing atomic.Bool
}

func NewService(db *gorm.DB, profile runtimeprofile.Profile) *Service {
	return &Service{db: db, profile: profile, cache: make(map[string]cachedCapability)}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("dependency control database is unavailable")
	}
	return s.db.WithContext(ctx).AutoMigrate(&Capability{})
}

func cacheKey(capability, scope string) string { return capability + "\x00" + scope }

func (s *Service) Before(ctx context.Context, capability, scope string) error {
	if s == nil || s.db == nil || s.db.Dialector.Name() != "postgres" {
		return nil
	}
	row, err := s.get(ctx, capability, scope)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return s.deferPostgres(err)
	}
	switch row.State {
	case StateBlocked, StateRepairing, StateChecking:
		return &DeferredError{
			Capability: capability, Scope: scope, IncidentID: row.IncidentID,
			Code: row.LastErrorCode, RetryAfter: defaultRetryAfter,
			Cause: errors.New(strings.TrimSpace(row.LastErrorMessage)),
		}
	default:
		return nil
	}
}

// Observe returns either the original business error or a structured,
// budget-free dependency deferral. Only exact corruption signatures open the
// keyword-index circuit; unrelated PostgreSQL XX000 errors remain visible.
func (s *Service) Observe(ctx context.Context, capability, scope string, cause error) error {
	if cause == nil {
		return nil
	}
	if isKeywordIndexCorruption(cause) {
		incidentID, blockErr := s.block(
			context.WithoutCancel(ctx), capability, scope,
			"keyword_index_corrupt", cause,
		)
		if blockErr != nil {
			logger.Warnf(ctx, "[dependency control] persist index circuit failed: %v", blockErr)
		}
		return &DeferredError{
			Capability: capability, Scope: scope, IncidentID: incidentID,
			Code: "keyword_index_corrupt", RetryAfter: defaultRetryAfter, Cause: cause,
		}
	}
	if isInfrastructureUnavailable(cause) {
		return s.deferPostgres(cause)
	}
	return cause
}

func (s *Service) deferPostgres(cause error) error {
	return &DeferredError{
		Capability: CapabilityPostgres, Scope: "primary",
		Code: "postgres_unavailable", RetryAfter: defaultRetryAfter, Cause: cause,
	}
}

func (s *Service) block(
	ctx context.Context, capability, scope, code string, cause error,
) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("dependency control database is unavailable")
	}
	now := time.Now().UTC()
	incidentID := uuid.NewString()
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	row := Capability{
		Capability: capability, Scope: scope, State: StateBlocked,
		IncidentID: incidentID, LastErrorCode: code, LastErrorMessage: message,
		LastCheckedAt: &now, BlockedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "capability"}, {Name: "scope"}},
		DoUpdates: clause.Assignments(map[string]any{
			"state": StateBlocked,
			"incident_id": gorm.Expr(
				"CASE WHEN custom_dependency_capabilities.state IN ('blocked','repairing') AND custom_dependency_capabilities.incident_id <> '' THEN custom_dependency_capabilities.incident_id ELSE ? END",
				incidentID,
			),
			"last_error_code": code, "last_error_message": message,
			"last_checked_at": now, "blocked_at": now, "updated_at": now,
		}),
	}).Create(&row).Error
	if err != nil {
		return incidentID, err
	}
	s.invalidate(capability, scope)
	stored, loadErr := s.get(ctx, capability, scope)
	if loadErr == nil && stored.IncidentID != "" {
		incidentID = stored.IncidentID
	}
	return incidentID, nil
}

func (s *Service) get(ctx context.Context, capability, scope string) (Capability, error) {
	key := cacheKey(capability, scope)
	s.mu.Lock()
	if cached, ok := s.cache[key]; ok && time.Now().Before(cached.expiresAt) {
		s.mu.Unlock()
		return cached.row, nil
	}
	s.mu.Unlock()
	var row Capability
	err := s.db.WithContext(ctx).Where(
		"capability = ? AND scope = ?", capability, scope,
	).First(&row).Error
	if err != nil {
		return Capability{}, err
	}
	s.mu.Lock()
	s.cache[key] = cachedCapability{row: row, expiresAt: time.Now().Add(2 * time.Second)}
	s.mu.Unlock()
	return row, nil
}

func (s *Service) invalidate(capability, scope string) {
	s.mu.Lock()
	delete(s.cache, cacheKey(capability, scope))
	s.mu.Unlock()
}

func (s *Service) Snapshot(ctx context.Context) ([]Capability, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("dependency control database is unavailable")
	}
	var rows []Capability
	err := s.db.WithContext(ctx).Order("capability ASC, scope ASC").Find(&rows).Error
	return rows, err
}

func (s *Service) IsRepairing() bool { return s != nil && s.repairing.Load() }

func (s *Service) ReadyFor(profile runtimeprofile.Profile) (bool, string) {
	if s == nil || !profile.RunsParseWorker() {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.Before(ctx, CapabilityKeywordIndex, KeywordIndexScope)
	if err == nil {
		return true, ""
	}
	return false, err.Error()
}

func (s *Service) Start(parent context.Context) error {
	if s == nil || !s.profile.RunsMaintenance() {
		return nil
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(defaultProbeInterval)
		defer ticker.Stop()
		for {
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				logger.Warnf(ctx, "[dependency control] index reconciliation incomplete: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Service) Reconcile(ctx context.Context) error {
	if s == nil || s.db == nil || s.db.Dialector.Name() != "postgres" {
		return nil
	}
	var bootEpoch string
	if err := s.db.WithContext(ctx).Raw(
		"SELECT pg_postmaster_start_time()::text",
	).Scan(&bootEpoch).Error; err != nil {
		return s.deferPostgres(err)
	}
	row, err := s.get(ctx, CapabilityKeywordIndex, KeywordIndexScope)
	needsCheck := errors.Is(err, gorm.ErrRecordNotFound)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if !needsCheck {
		needsCheck = row.ObservedBootEpoch != bootEpoch ||
			row.State == StateBlocked || row.State == StateRepairing || row.State == StateChecking
	}
	if !needsCheck {
		return nil
	}
	if err := s.setState(ctx, StateChecking, bootEpoch, row.IncidentID, row.LastErrorCode, row.LastErrorMessage); err != nil {
		return err
	}
	verified, details, verifyErr := s.verifyKeywordIndex(ctx)
	if verifyErr == nil && verified {
		return s.markHealthy(ctx, bootEpoch, details)
	}
	if !autoRepairEnabled() {
		_, blockErr := s.block(ctx, CapabilityKeywordIndex, KeywordIndexScope,
			"keyword_index_verify_failed", errors.Join(verifyErr, errors.New(details)))
		return errors.Join(verifyErr, blockErr)
	}
	return s.repairKeywordIndex(ctx, bootEpoch, errors.Join(verifyErr, errors.New(details)))
}

func (s *Service) verifyKeywordIndex(ctx context.Context) (bool, string, error) {
	type check struct {
		Name    string `gorm:"column:check_name"`
		Passed  bool   `gorm:"column:passed"`
		Details string `gorm:"column:details"`
	}
	var checks []check
	err := s.db.WithContext(ctx).Raw(`
		SELECT * FROM pdb.verify_index(
			'public.embeddings_search_idx'::regclass,
			false, 0.01, false, false, false, NULL
		)`).Scan(&checks).Error
	if err != nil {
		return false, "verify query failed", err
	}
	if len(checks) == 0 {
		return false, "verify returned no checks", nil
	}
	details := make([]string, 0, len(checks))
	allPassed := true
	for _, item := range checks {
		details = append(details, fmt.Sprintf("%s=%t(%s)", item.Name, item.Passed, item.Details))
		allPassed = allPassed && item.Passed
	}
	return allPassed, strings.Join(details, "; "), nil
}

func (s *Service) repairKeywordIndex(ctx context.Context, bootEpoch string, cause error) error {
	if !s.repairing.CompareAndSwap(false, true) {
		return nil
	}
	defer s.repairing.Store(false)
	incidentID := ""
	if row, err := s.get(ctx, CapabilityKeywordIndex, KeywordIndexScope); err == nil {
		incidentID = row.IncidentID
	}
	if incidentID == "" {
		incidentID = uuid.NewString()
	}
	if err := s.setState(ctx, StateRepairing, bootEpoch, incidentID,
		"keyword_index_reindexing", errorString(cause)); err != nil {
		return err
	}
	logger.Warnf(ctx, "[dependency control] rebuilding %s incident=%s", KeywordIndexScope, incidentID)
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	if _, err := sqlDB.ExecContext(ctx,
		"REINDEX INDEX CONCURRENTLY public.embeddings_search_idx"); err != nil {
		_, blockErr := s.block(context.WithoutCancel(ctx), CapabilityKeywordIndex,
			KeywordIndexScope, "keyword_index_reindex_failed", err)
		return errors.Join(err, blockErr)
	}
	verified, details, err := s.verifyKeywordIndex(ctx)
	if err != nil || !verified {
		verifyErr := errors.Join(err, errors.New(details))
		_, blockErr := s.block(context.WithoutCancel(ctx), CapabilityKeywordIndex,
			KeywordIndexScope, "keyword_index_verify_failed", verifyErr)
		return errors.Join(verifyErr, blockErr)
	}
	return s.markHealthy(ctx, bootEpoch, details)
}

func (s *Service) setState(
	ctx context.Context, state State, bootEpoch, incidentID, code, message string,
) error {
	now := time.Now().UTC()
	row := Capability{
		Capability: CapabilityKeywordIndex, Scope: KeywordIndexScope, State: state,
		IncidentID: incidentID, ObservedBootEpoch: bootEpoch,
		LastErrorCode: code, LastErrorMessage: message,
		LastCheckedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "capability"}, {Name: "scope"}},
		DoUpdates: clause.Assignments(map[string]any{
			"state": state, "incident_id": incidentID,
			"observed_boot_epoch": bootEpoch, "last_error_code": code,
			"last_error_message": message, "last_checked_at": now, "updated_at": now,
		}),
	}).Create(&row).Error
	s.invalidate(CapabilityKeywordIndex, KeywordIndexScope)
	return err
}

func (s *Service) markHealthy(ctx context.Context, bootEpoch, details string) error {
	now := time.Now().UTC()
	row := Capability{
		Capability: CapabilityKeywordIndex, Scope: KeywordIndexScope,
		State: StateHealthy, HealthEpoch: 1, ObservedBootEpoch: bootEpoch,
		LastErrorMessage: details, LastCheckedAt: &now, LastHealthyAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "capability"}, {Name: "scope"}},
		DoUpdates: clause.Assignments(map[string]any{
			"state": StateHealthy, "incident_id": "",
			"health_epoch":        gorm.Expr("custom_dependency_capabilities.health_epoch + 1"),
			"observed_boot_epoch": bootEpoch, "last_error_code": "",
			"last_error_message": details, "last_checked_at": now,
			"last_healthy_at": now, "updated_at": now,
		}),
	}).Create(&row).Error
	s.invalidate(CapabilityKeywordIndex, KeywordIndexScope)
	if err == nil {
		logger.Infof(ctx, "[dependency control] %s healthy", KeywordIndexScope)
	}
	return err
}

func isKeywordIndexCorruption(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	exactMarker := strings.Contains(message, "segmentmetaentryheader") &&
		(strings.Contains(message, "unexpectedend") || strings.Contains(message, "unexpected end"))
	if !exactMarker {
		return false
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr) || pgErr.Code == "XX000"
}

func isInfrastructureUnavailable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused", "connection reset", "broken pipe", "no such host",
		"network is unreachable", "database system is starting up",
		"terminating connection due to administrator command", "server closed the connection",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func autoRepairEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CUSTOM_DEPENDENCY_AUTO_REPAIR_INDEX")))
	return raw == "" || raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
