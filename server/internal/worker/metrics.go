package worker

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/domain/host"
)

type metrics struct {
	hosts            *prometheus.GaugeVec
	reconcileRuns    *prometheus.CounterVec
	reconcileActions *prometheus.CounterVec
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
