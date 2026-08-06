// Package dependencycontrol turns shared infrastructure outages into durable,
// budget-free waits. PostgreSQL remains the source of truth; process-local
// caches only reduce query volume and never decide terminal business state.
package dependencycontrol

import (
	"errors"
	"fmt"
	"time"
)

type State string

const (
	StateUnknown   State = "unknown"
	StateChecking  State = "checking"
	StateHealthy   State = "healthy"
	StateDegraded  State = "degraded"
	StateBlocked   State = "blocked"
	StateRepairing State = "repairing"
)

const (
	CapabilityKeywordIndex = "keyword_index"
	CapabilityPostgres     = "postgres_heap"
	KeywordIndexScope      = "public.embeddings_search_idx"
)

type Capability struct {
	Capability        string     `json:"capability" gorm:"type:varchar(64);primaryKey"`
	Scope             string     `json:"scope" gorm:"type:varchar(160);primaryKey"`
	State             State      `json:"state" gorm:"type:varchar(24);not null;index"`
	IncidentID        string     `json:"incident_id,omitempty" gorm:"type:varchar(64);not null;default:'';index"`
	HealthEpoch       int64      `json:"health_epoch" gorm:"not null;default:0"`
	ObservedBootEpoch string     `json:"observed_boot_epoch,omitempty" gorm:"type:varchar(128);not null;default:''"`
	LastErrorCode     string     `json:"last_error_code,omitempty" gorm:"type:varchar(96);not null;default:''"`
	LastErrorMessage  string     `json:"last_error_message,omitempty" gorm:"type:text;not null;default:''"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	LastHealthyAt     *time.Time `json:"last_healthy_at,omitempty"`
	BlockedAt         *time.Time `json:"blocked_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt         time.Time  `json:"updated_at" gorm:"not null"`
}

func (Capability) TableName() string { return "custom_dependency_capabilities" }

var ErrDependencyDeferred = errors.New("shared dependency is unavailable")

// DeferredError is intentionally compatible with modeladmission's generic
// budget-free deferral contract. Existing durable document/derivative/Wiki
// handlers therefore preserve their retry budgets without importing this
// package directly.
type DeferredError struct {
	Capability string
	Scope      string
	IncidentID string
	Code       string
	RetryAfter time.Duration
	Cause      error
}

func (e *DeferredError) Error() string {
	delay := e.RetryAfter
	if delay < time.Second {
		delay = time.Second
	}
	message := fmt.Sprintf(
		"%v: capability=%s scope=%s code=%s retry_after=%s",
		ErrDependencyDeferred, e.Capability, e.Scope, e.Code, delay.Round(time.Second),
	)
	if e.IncidentID != "" {
		message += " incident=" + e.IncidentID
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *DeferredError) Unwrap() error                  { return e.Cause }
func (e *DeferredError) ModelWorkDeferred() bool        { return true }
func (e *DeferredError) ModelRetryAfter() time.Duration { return e.RetryAfter }
func (e *DeferredError) Is(target error) bool {
	return target == ErrDependencyDeferred || errors.Is(e.Cause, target)
}

func RetryAfter(err error) (time.Duration, bool) {
	var deferred *DeferredError
	if !errors.As(err, &deferred) {
		return 0, false
	}
	delay := deferred.RetryAfter
	if delay < time.Second {
		delay = time.Second
	}
	return delay, true
}
