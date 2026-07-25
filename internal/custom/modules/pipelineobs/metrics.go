package pipelineobs

import (
	"strconv"
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

// BoolLabel is kept here so callers do not grow label cardinality with ad-hoc
// formatting.
func BoolLabel(value bool) string {
	return strconv.FormatBool(value)
}
