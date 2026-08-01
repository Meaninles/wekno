// Package runtimeinstances records the live role/capability of every WeKnora
// process. It is observability and soft-balancing state only; task leases and
// provider admission remain the correctness boundaries.
package runtimeinstances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	StateReady    = "ready"
	StateDraining = "draining"
	StateStopped  = "stopped"

	HeartbeatInterval = 10 * time.Second
	StaleAfter        = 35 * time.Second
)

type Instance struct {
	InstanceID            string     `json:"instance_id" gorm:"type:varchar(200);primaryKey"`
	BootID                string     `json:"boot_id" gorm:"type:varchar(36);not null;index"`
	Role                  string     `json:"role" gorm:"type:varchar(32);not null;index"`
	State                 string     `json:"state" gorm:"type:varchar(24);not null;index"`
	DerivativeConcurrency int        `json:"derivative_concurrency" gorm:"not null;default:0"`
	WikiConcurrency       int        `json:"wiki_concurrency" gorm:"not null;default:0"`
	ParseConcurrency      int        `json:"parse_concurrency" gorm:"not null;default:0"`
	StartedAt             time.Time  `json:"started_at" gorm:"not null"`
	LastHeartbeatAt       time.Time  `json:"last_heartbeat_at" gorm:"not null;index"`
	StoppedAt             *time.Time `json:"stopped_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt             time.Time  `json:"updated_at" gorm:"not null"`
}

func (Instance) TableName() string { return "custom_runtime_instances" }

type Registry struct {
	db                    *gorm.DB
	profile               runtimeprofile.Profile
	instanceID            string
	bootID                string
	derivativeConcurrency int
	wikiConcurrency       int
	parseConcurrency      int

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

func NewRegistry(db *gorm.DB, profile runtimeprofile.Profile) *Registry {
	derivativeConcurrency, wikiConcurrency, parseConcurrency := configuredConcurrency(profile)
	return &Registry{
		db: db, profile: profile, instanceID: resolveInstanceID(profile),
		bootID:                uuid.NewString(),
		derivativeConcurrency: derivativeConcurrency,
		wikiConcurrency:       wikiConcurrency,
		parseConcurrency:      parseConcurrency,
	}
}

func resolveInstanceID(profile runtimeprofile.Profile) string {
	if value := strings.TrimSpace(os.Getenv("CUSTOM_RUNTIME_INSTANCE_ID")); value != "" {
		return value
	}
	if profile.RunsParseWorker() {
		if value := strings.TrimSpace(os.Getenv("CUSTOM_DOCUMENT_QUEUE_INSTANCE_ID")); value != "" {
			return value
		}
	}
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	if host == "" {
		host = "host-" + uuid.NewString()
	}
	return string(profile.Role) + ":" + host
}

func configuredConcurrency(profile runtimeprofile.Profile) (derivative, wiki, parse int) {
	switch profile.Role {
	case runtimeprofile.RoleDerivativeWorker:
		derivative = positiveEnv("WEKNORA_ASYNQ_TASK_CONCURRENCY", 32)
	case runtimeprofile.RoleWikiWorker:
		wiki = positiveEnv("WEKNORA_WIKI_MAP_TASK_CONCURRENCY", 4)
	case runtimeprofile.RoleParseWorker:
		parse = positiveEnv("WEKNORA_ASYNQ_CONCURRENCY", 4)
	case runtimeprofile.RoleDevAll:
		derivative = positiveEnv("WEKNORA_ASYNQ_TASK_CONCURRENCY", 32)
		wiki = positiveEnv("WEKNORA_WIKI_MAP_TASK_CONCURRENCY", 4)
		parse = positiveEnv("WEKNORA_ASYNQ_CONCURRENCY", 4)
	}
	return derivative, wiki, parse
}

func positiveEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

func (r *Registry) Migrate(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("runtime instance registry database is unavailable")
	}
	db := r.db.WithContext(ctx)
	if err := db.AutoMigrate(&Instance{}); err != nil {
		return err
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_custom_runtime_instances_live
		ON custom_runtime_instances (role, state, last_heartbeat_at)`).Error
}

func (r *Registry) Start(parent context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("runtime instance registry is unavailable")
	}
	if r.profile.Role == runtimeprofile.RoleMigration {
		return nil
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.done = make(chan struct{})
	r.started = true
	r.mu.Unlock()

	if err := r.register(ctx); err != nil {
		cancel()
		return err
	}
	go r.heartbeatLoop(ctx)
	return nil
}

func (r *Registry) register(ctx context.Context) error {
	now := time.Now().UTC()
	row := Instance{
		InstanceID: r.instanceID, BootID: r.bootID, Role: string(r.profile.Role),
		State:                 StateReady,
		DerivativeConcurrency: r.derivativeConcurrency,
		WikiConcurrency:       r.wikiConcurrency,
		ParseConcurrency:      r.parseConcurrency,
		StartedAt:             now, LastHeartbeatAt: now, CreatedAt: now, UpdatedAt: now,
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "instance_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"boot_id": r.bootID, "role": string(r.profile.Role), "state": StateReady,
			"derivative_concurrency": r.derivativeConcurrency,
			"wiki_concurrency":       r.wikiConcurrency,
			"parse_concurrency":      r.parseConcurrency, "started_at": now,
			"last_heartbeat_at": now, "stopped_at": nil, "updated_at": now,
		}),
	}).Create(&row).Error
}

func (r *Registry) heartbeatLoop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			result := r.db.WithContext(ctx).Model(&Instance{}).
				Where("instance_id = ? AND boot_id = ? AND state = ?", r.instanceID, r.bootID, StateReady).
				Updates(map[string]any{
					"last_heartbeat_at":      now,
					"derivative_concurrency": r.derivativeConcurrency,
					"wiki_concurrency":       r.wikiConcurrency,
					"parse_concurrency":      r.parseConcurrency,
					"updated_at":             now,
				})
			if result.Error != nil {
				logger.Warnf(ctx, "[runtime instances] heartbeat failed instance=%s role=%s: %v", r.instanceID, r.profile.Role, result.Error)
				continue
			}
			if result.RowsAffected != 1 {
				logger.Warnf(ctx, "[runtime instances] boot fenced instance=%s role=%s", r.instanceID, r.profile.Role)
				return
			}
		}
	}
}

func (r *Registry) Stop() error {
	if r == nil {
		return nil
	}
	var stopErr error
	r.stopOnce.Do(func() {
		r.mu.Lock()
		cancel := r.cancel
		done := r.done
		started := r.started
		r.mu.Unlock()
		if !started {
			return
		}
		now := time.Now().UTC()
		ctx, cancelUpdate := context.WithTimeout(context.Background(), 3*time.Second)
		result := r.db.WithContext(ctx).Model(&Instance{}).
			Where("instance_id = ? AND boot_id = ?", r.instanceID, r.bootID).
			Updates(map[string]any{"state": StateDraining, "last_heartbeat_at": now, "updated_at": now})
		cancelUpdate()
		if result.Error != nil {
			stopErr = result.Error
		}
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				stopErr = errors.Join(stopErr, errors.New("runtime instance heartbeat did not stop"))
			}
		}
		stopped := time.Now().UTC()
		ctx, cancelUpdate = context.WithTimeout(context.Background(), 3*time.Second)
		result = r.db.WithContext(ctx).Model(&Instance{}).
			Where("instance_id = ? AND boot_id = ?", r.instanceID, r.bootID).
			Updates(map[string]any{
				"state": StateStopped, "stopped_at": stopped,
				"last_heartbeat_at": stopped, "updated_at": stopped,
			})
		cancelUpdate()
		if result.Error != nil {
			stopErr = errors.Join(stopErr, result.Error)
		}
	})
	return stopErr
}

func Start(registry *Registry, cleaner interfaces.ResourceCleaner) error {
	if registry == nil {
		return fmt.Errorf("runtime instance registry is unavailable")
	}
	if err := registry.Start(context.Background()); err != nil {
		return err
	}
	if cleaner != nil {
		cleaner.RegisterWithName("RuntimeInstanceRegistry", registry.Stop)
	}
	return nil
}

func ListActive(ctx context.Context, db *gorm.DB) ([]Instance, error) {
	if db == nil {
		return nil, errors.New("runtime instance database is unavailable")
	}
	var rows []Instance
	err := db.WithContext(ctx).
		Where("state = ? AND last_heartbeat_at >= ?", StateReady, time.Now().UTC().Add(-StaleAfter)).
		Order("role, instance_id").Find(&rows).Error
	return rows, err
}
