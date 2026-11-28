package worker

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

type metrics struct {
	hosts            *prometheus.GaugeVec
	reconcileRuns    *prometheus.CounterVec
	reconcileActions *prometheus.CounterVec
}

type taskClaimMetrics struct {
	wait *prometheus.HistogramVec
}

type conversationRouteMetrics struct {
	actions                *prometheus.CounterVec
	aggregationStates      *prometheus.CounterVec
	aggregationWaitTime    prometheus.Histogram
	receiptAgeAtClaim      prometheus.Histogram
	classificationTime     prometheus.Histogram
	failureExplanationTime prometheus.Histogram
}

func validHostState(state host.State) bool {
	switch state {
	case host.StateCreating, host.StateRunning, host.StateStopping, host.StateStopped, host.StateFailed, host.StateDeleting, host.StateDeleted:
		return true
	default:
		return false
	}
}

func validReconcileAction(action string) bool {
	switch action {
	case "lost", "orphan_deleted", "renew", "release", "volume_deleted", "create_timeout", "model_call_unknown":
		return true
	default:
		return false
	}
}

func newMetrics(registerer prometheus.Registerer) (*metrics, error) {
	result := &metrics{
		hosts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cloudhost_hosts",
			Help: "Current number of hosts by state.",
		}, []string{"state"}),
		reconcileRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_reconcile_runs_total",
			Help: "Reconciler iterations by result.",
		}, []string{"result"}),
		reconcileActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_reconcile_actions_total",
			Help: "Reconciliation actions performed.",
		}, []string{"action"}),
	}
	for _, collector := range []prometheus.Collector{result.hosts, result.reconcileRuns, result.reconcileActions} {
		if err := registerer.Register(collector); err != nil {
			return nil, fmt.Errorf("register worker metric: %w", err)
		}
	}
	return result, nil
}

func newTaskClaimMetrics(registerer prometheus.Registerer) (*taskClaimMetrics, error) {
	if registerer == nil {
		return nil, nil
	}
	result := &taskClaimMetrics{wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "cloudhost_task_claim_wait_seconds",
		Help: "Seconds since the task entered the queued state before claim; non-queued recovery is omitted.",
	}, []string{"queue_priority"})}
	if err := registerer.Register(result.wait); err != nil {
		return nil, fmt.Errorf("register task claim metric: %w", err)
	}
	return result, nil
}

func (m *taskClaimMetrics) observeClaim(value task.Task, now time.Time) {
	if m == nil || value.QueueEnteredAt.IsZero() || !value.QueueEnteredAt.Before(now) {
		return
	}
	priority := value.QueuePriority
	if !priority.Valid() {
		priority = task.QueuePriorityNormal
	}
	m.wait.WithLabelValues(string(priority)).Observe(now.Sub(value.QueueEnteredAt).Seconds())
}

func newConversationRouteMetrics(registerer prometheus.Registerer) (*conversationRouteMetrics, error) {
	if registerer == nil {
		return nil, nil
	}
	result := &conversationRouteMetrics{
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_conversation_route_actions_total",
			Help: "Conversation routing decisions applied or rejected by the durable route store.",
		}, []string{"action", "result"}),
		aggregationStates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_conversation_aggregation_transitions_total",
			Help: "Conversation aggregation and correction transitions observed by route workers.",
		}, []string{"state"}),
		aggregationWaitTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cloudhost_conversation_aggregation_wait_seconds",
			Help: "Seconds from aggregation start until a route decision is durably applied.",
		}),
		receiptAgeAtClaim: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cloudhost_conversation_route_receipt_age_at_claim_seconds",
			Help: "Seconds from the persisted user message receipt until a route worker claims it.",
		}),
		classificationTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cloudhost_conversation_route_classification_duration_seconds",
			Help: "Seconds spent waiting for the DSH conversation route classification to complete.",
		}),
		failureExplanationTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cloudhost_conversation_route_failure_explanation_duration_seconds",
			Help: "Seconds spent waiting for the DSH no-tools explanation after a route output remains invalid.",
		}),
	}
	for _, collector := range []prometheus.Collector{result.actions, result.aggregationStates, result.aggregationWaitTime,
		result.receiptAgeAtClaim, result.classificationTime, result.failureExplanationTime} {
		if err := registerer.Register(collector); err != nil {
			return nil, fmt.Errorf("register conversation route metric: %w", err)
		}
	}
	return result, nil
}
