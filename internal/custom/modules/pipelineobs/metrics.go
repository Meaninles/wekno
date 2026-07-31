package pipelineobs

import (
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	contentCacheOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weknora",
			Subsystem: "content_cache",
			Name:      "operations_total",
			Help:      "Content-addressed cache operations by artifact kind and result.",
		},
		[]string{"kind", "operation", "result"},
	)
	contentCachePayloadBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "weknora",
			Subsystem: "content_cache",
			Name:      "payload_bytes",
			Help:      "Uncompressed content cache payload sizes.",
			Buckets:   prometheus.ExponentialBuckets(128, 4, 10),
		},
		[]string{"kind"},
	)
	modelAdmissionWait = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "weknora",
			Subsystem: "model_admission",
			Name:      "wait_seconds",
			Help:      "Time spent waiting for a distributed model/parser admission lease.",
			Buckets:   []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 180, 300},
		},
		[]string{"kind", "class", "result"},
	)
	modelAdmissionInflight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "weknora",
			Subsystem: "model_admission",
			Name:      "inflight",
			Help:      "Current model/parser calls admitted by this application replica.",
		},
		[]string{"kind", "class"},
	)
	documentWorkflowOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weknora",
			Subsystem: "document_workflow",
			Name:      "operations_total",
			Help:      "Durable document workflow state-machine operations.",
		},
		[]string{"operation", "result"},
	)
	documentWorkerActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "weknora",
			Subsystem: "document_worker",
			Name:      "active",
			Help:      "Complete document workflows active in this application replica.",
		},
	)
	documentWorkerCapacity = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "weknora",
			Subsystem: "document_worker",
			Name:      "capacity",
			Help:      "Configured complete-document concurrency for this application replica.",
		},
	)
	wikiDocumentOutcomes = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weknora",
			Subsystem: "wiki",
			Name:      "document_outcomes_total",
			Help:      "Terminal per-document Wiki outcomes.",
		},
		[]string{"outcome"},
	)
	modelPoolInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "model_pool_inflight",
		Help: "Current admitted provider calls by actual resource pool.",
	}, []string{"pool"})
	modelPoolWaiting = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "model_pool_waiting",
		Help: "Current admission waiters by actual resource pool.",
	}, []string{"pool"})
	modelPoolAdmissionRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "weknora", Name: "model_pool_admission_rejected_total",
		Help: "Pre-provider admission rejections by actual resource pool and reason.",
	}, []string{"pool", "reason"})
	modelPoolProviderDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "weknora", Name: "model_pool_provider_duration_seconds",
		Help:    "Provider call duration by actual resource pool.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 180, 600},
	}, []string{"pool"})
	modelPoolProviderErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "weknora", Name: "model_pool_provider_errors_total",
		Help: "Provider errors by actual resource pool and bounded error class.",
	}, []string{"pool", "class"})
	modelPoolCircuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "model_pool_circuit_state",
		Help: "Circuit state by actual resource pool: 0 closed, 1 open.",
	}, []string{"pool"})
	derivativeWorkItems = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "derivative_work_items",
		Help: "PostgreSQL-authoritative derivative work items by state, kind, and pool.",
	}, []string{"state", "kind", "pool"})
	derivativeOldestWait = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "derivative_oldest_wait_seconds",
		Help: "Age of the oldest due derivative item by kind and pool.",
	}, []string{"kind", "pool"})
	derivativeProviderAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "weknora", Name: "derivative_provider_attempts_total",
		Help: "Actual derivative provider calls by kind and pool.",
	}, []string{"kind", "pool"})
	derivativeMaterializeRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "weknora", Name: "derivative_materialize_retries_total",
		Help: "Derivative materialization retries by kind.",
	}, []string{"kind"})
	derivativeFinalizeRetries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "weknora", Name: "derivative_finalize_retries_total",
		Help: "Derivative terminal-finalization retries.",
	})
	derivativeDuplicateDelivery = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "weknora", Name: "derivative_duplicate_delivery_total",
		Help: "Duplicate or stale derivative wake deliveries safely acknowledged.",
	})
	processingSpanRows = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "processing_span_rows_total",
		Help: "Current number of Span V2 rows.",
	})
	processingSpanInserts = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "weknora", Name: "processing_span_inserts_total",
		Help: "Span V2 logical row inserts.",
	})
	processingSpanUpdates = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "weknora", Name: "processing_span_updates_total",
		Help: "Span V2 logical row updates.",
	})
	processingSpanRetentionDeleted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "weknora", Name: "processing_span_retention_deleted_total",
		Help: "Span V2 rows removed by retention.",
	})
	maintenanceLeader = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "maintenance_leader",
		Help: "Whether this maintenance replica currently owns the advisory lock.",
	})
	dbPoolOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "db_pool_open",
		Help: "Open database connections by runtime role.",
	}, []string{"role"})
	dbPoolInUse = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "db_pool_in_use",
		Help: "In-use database connections by runtime role.",
	}, []string{"role"})
	dbPoolWaitCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "db_pool_wait_count_total",
		Help: "Cumulative database connection waits by runtime role.",
	}, []string{"role"})
	dbPoolWaitDuration = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weknora", Name: "db_pool_wait_duration_seconds",
		Help: "Cumulative database connection wait duration by runtime role.",
	}, []string{"role"})
)

var (
	dbPools          sync.Map
	dbMetricLoopOnce sync.Once
)

func ObserveContentCache(kind, operation, result string, payloadBytes int) {
	contentCacheOperations.WithLabelValues(kind, operation, result).Inc()
	if payloadBytes >= 0 {
		contentCachePayloadBytes.WithLabelValues(kind).Observe(float64(payloadBytes))
	}
}

func ObserveModelAdmission(kind string, background bool, result string, elapsed time.Duration) {
	class := "interactive"
	if background {
		class = "background"
	}
	modelAdmissionWait.WithLabelValues(kind, class, result).Observe(elapsed.Seconds())
}

func ModelAdmissionAcquired(kind string, background bool) {
	class := "interactive"
	if background {
		class = "background"
	}
	modelAdmissionInflight.WithLabelValues(kind, class).Inc()
}

func ModelAdmissionReleased(kind string, background bool) {
	class := "interactive"
	if background {
		class = "background"
	}
	modelAdmissionInflight.WithLabelValues(kind, class).Dec()
}

func ObserveDocumentWorkflow(operation, result string) {
	documentWorkflowOperations.WithLabelValues(operation, result).Inc()
}

func SetDocumentWorkerCapacity(capacity int) {
	documentWorkerCapacity.Set(float64(capacity))
}

func DocumentWorkerStarted() { documentWorkerActive.Inc() }
func DocumentWorkerStopped() { documentWorkerActive.Dec() }

func ObserveWikiOutcome(outcome string) {
	wikiDocumentOutcomes.WithLabelValues(outcome).Inc()
}

func ModelPoolWaiting(pool string, delta float64) {
	modelPoolWaiting.WithLabelValues(pool).Add(delta)
}

func ModelPoolAcquired(pool string) { modelPoolInflight.WithLabelValues(pool).Inc() }
func ModelPoolReleased(pool string) { modelPoolInflight.WithLabelValues(pool).Dec() }

func ModelPoolRejected(pool, reason string) {
	modelPoolAdmissionRejected.WithLabelValues(pool, reason).Inc()
}

func ObserveModelPoolProvider(pool, errorClass string, elapsed time.Duration) {
	modelPoolProviderDuration.WithLabelValues(pool).Observe(elapsed.Seconds())
	if errorClass != "" {
		modelPoolProviderErrors.WithLabelValues(pool, errorClass).Inc()
	}
}

func SetModelPoolCircuit(pool string, open bool) {
	value := 0.0
	if open {
		value = 1
	}
	modelPoolCircuitState.WithLabelValues(pool).Set(value)
}

type DerivativeMetricRow struct {
	State, Kind, Pool string
	Count             int64
	OldestWaitSeconds float64
}

func SetDerivativeSnapshot(rows []DerivativeMetricRow) {
	derivativeWorkItems.Reset()
	derivativeOldestWait.Reset()
	for _, row := range rows {
		derivativeWorkItems.WithLabelValues(row.State, row.Kind, row.Pool).Set(float64(row.Count))
		if row.OldestWaitSeconds > 0 {
			derivativeOldestWait.WithLabelValues(row.Kind, row.Pool).Set(row.OldestWaitSeconds)
		}
	}
}

func DerivativeProviderAttempt(kind, pool string) {
	derivativeProviderAttempts.WithLabelValues(kind, pool).Inc()
}
func DerivativeMaterializeRetry(kind string) {
	derivativeMaterializeRetries.WithLabelValues(kind).Inc()
}
func DerivativeFinalizeRetry()         { derivativeFinalizeRetries.Inc() }
func DerivativeDuplicateWake()         { derivativeDuplicateDelivery.Inc() }
func ProcessingSpanInserted()          { processingSpanInserts.Inc() }
func ProcessingSpanUpdated()           { processingSpanUpdates.Inc() }
func SetProcessingSpanRows(rows int64) { processingSpanRows.Set(float64(rows)) }
func ProcessingSpanRetentionDeleted(rows int64) {
	if rows > 0 {
		processingSpanRetentionDeleted.Add(float64(rows))
	}
}
func SetMaintenanceLeader(value bool) {
	if value {
		maintenanceLeader.Set(1)
	} else {
		maintenanceLeader.Set(0)
	}
}

func RegisterDBPool(role string, db *sql.DB) {
	if db != nil {
		dbPools.Store(role, db)
		dbMetricLoopOnce.Do(func() {
			go func() {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					RefreshDBPoolMetrics()
				}
			}()
		})
		RefreshDBPoolMetrics()
	}
}

func RefreshDBPoolMetrics() {
	dbPools.Range(func(key, value any) bool {
		role, okRole := key.(string)
		db, okDB := value.(*sql.DB)
		if !okRole || !okDB || db == nil {
			return true
		}
		stats := db.Stats()
		dbPoolOpen.WithLabelValues(role).Set(float64(stats.OpenConnections))
		dbPoolInUse.WithLabelValues(role).Set(float64(stats.InUse))
		dbPoolWaitCount.WithLabelValues(role).Set(float64(stats.WaitCount))
		dbPoolWaitDuration.WithLabelValues(role).Set(stats.WaitDuration.Seconds())
		return true
	})
}

// BoolLabel is kept here so callers do not grow label cardinality with ad-hoc
// formatting.
func BoolLabel(value bool) string {
	return strconv.FormatBool(value)
}
