package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type workflowMetrics struct {
	eventResumes *prometheus.CounterVec
	taskRetries  *prometheus.CounterVec
	approvalWait *prometheus.HistogramVec
}

func newWorkflowMetrics(registerer prometheus.Registerer) (*workflowMetrics, error) {
	result := &workflowMetrics{
		eventResumes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_task_event_resume_requests_total", Help: "Authorized Task event-stream requests resumed from a nonzero cursor.",
		}, []string{"result"}),
		taskRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cloudhost_task_retries_total", Help: "Task retry requests by bounded operation kind and outcome.",
		}, []string{"kind", "result"}),
		approvalWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cloudhost_task_approval_wait_seconds", Help: "Seconds from a pending approval's creation until its accepted decision.",
		}, []string{"decision"}),
	}
	var err error
	if result.eventResumes, err = registerWorkflowCollector(registerer, result.eventResumes); err != nil {
		return nil, err
	}
	if result.taskRetries, err = registerWorkflowCollector(registerer, result.taskRetries); err != nil {
		return nil, err
	}
	if result.approvalWait, err = registerWorkflowCollector(registerer, result.approvalWait); err != nil {
		return nil, err
	}
	return result, nil
}

func registerWorkflowCollector[T prometheus.Collector](registerer prometheus.Registerer, collector T) (T, error) {
	if err := registerer.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := any(already.ExistingCollector).(T); ok {
				return existing, nil
			}
			var zero T
			return zero, errors.New("workflow metric is already registered with an incompatible collector")
		}
		var zero T
		return zero, fmt.Errorf("register workflow metric: %w", err)
	}
	return collector, nil
}

func (m *workflowMetrics) observeEventResume(result string) {
	if m != nil {
		m.eventResumes.WithLabelValues(result).Inc()
	}
}

func (m *workflowMetrics) observeTaskRetry(kind string, err error) {
	if m == nil {
		return
	}
	result := "accepted"
	if err != nil {
		result = "rejected"
	}
	m.taskRetries.WithLabelValues(kind, result).Inc()
}

func (m *workflowMetrics) observeApprovalWait(decision string, createdAt time.Time) {
	if m == nil || createdAt.IsZero() || time.Now().Before(createdAt) {
		return
	}
	if decision != "approve" && decision != "reject" {
		return
	}
	m.approvalWait.WithLabelValues(decision).Observe(time.Since(createdAt).Seconds())
}
