package app

import (
	"context"
	"time"

	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type ScheduleListRequest struct {
	Status schedule.Status
	Page   PageRequest
}

type SchedulePage struct {
	Items      []schedule.Schedule
	NextCursor string
}

type ScheduleRunPage struct {
	Items      []schedule.Run
	NextCursor string
}

type CreateScheduleCommand struct {
	HostID             string
	ModelID            string
	Objective          string
	Timing             schedule.Timing
	Budget             task.Budget
	Observation        *schedule.ObservationSpec
	MisfirePolicy      schedule.MisfirePolicy
	NotificationEvents []schedule.NotificationEvent
}

type UpdateScheduleCommand struct {
	ExpectedVersion    int64
	HostID             *string
	ModelID            *string
	Objective          *string
	Timing             *schedule.Timing
	Budget             *task.Budget
	ObservationSet     bool
	Observation        *schedule.ObservationSpec
	MisfirePolicy      *schedule.MisfirePolicy
	NotificationEvents *[]schedule.NotificationEvent
}

type ScheduleActionCommand struct {
	Action           schedule.Action
	ExpectedVersion  int64
	CurrentRunAction schedule.CurrentRunAction
}

type ScheduleActionResult struct {
	Schedule   schedule.Schedule
	CurrentRun *schedule.Run
}

type ScheduleOccurrenceCommit struct {
	ScheduleID         string
	OwnerID            string
	ExpectedVersion    int64
	ExpectedEpoch      int64
	Occurrence         schedule.Occurrence
	NextOccurrence     *schedule.Occurrence
	NotificationEvents []schedule.NotificationEvent
}

// ScheduleStore owns schedule persistence and atomic creation of the ordinary
// Task graph for one scheduled occurrence.
type ScheduleStore interface {
	CreateSchedule(context.Context, schedule.Schedule, IdempotencyRecord) (schedule.Schedule, bool, error)
	ListSchedules(context.Context, string, []string, schedule.Status, PageRequest) (SchedulePage, error)
	GetSchedule(context.Context, string, string) (schedule.Schedule, error)
	UpdateSchedule(context.Context, string, schedule.Schedule, int64) (schedule.Schedule, error)
	SetScheduleStatus(context.Context, IdempotencyRecord, string, schedule.Status, int64) (schedule.Schedule, error)
	ListScheduleRuns(context.Context, string, string, []string, PageRequest) (ScheduleRunPage, error)
	ListDueSchedules(context.Context, time.Time, int32) ([]schedule.Schedule, error)
	CommitScheduleOccurrence(context.Context, ScheduleOccurrenceCommit) (schedule.Run, bool, error)
	GetTask(context.Context, string, string) (TaskDetails, error)
	RequestTaskAction(context.Context, IdempotencyRecord, string, task.Action, task.Status, string) (TaskActionResult, error)
}
