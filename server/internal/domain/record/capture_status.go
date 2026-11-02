package record

import "time"

type CaptureStatusQuery struct {
	BeforeID string
	Limit    int32
}

type CaptureStatus struct {
	ID          string
	TaskID      string
	Purpose     Purpose
	Kind        ItemKind
	State       QueueState
	FailureCode string
	CapturedAt  time.Time
	ExpiresAt   time.Time
}

type CaptureStatusPage struct {
	Items      []CaptureStatus
	NextBefore string
}
