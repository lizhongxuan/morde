package app

import (
	"context"
	"regexp"
	"time"

	"morde/server/internal/domain"
)

const (
	ApplicationLaunchStarting                   = "starting"
	ApplicationLaunchRunning                    = "running"
	ApplicationLaunchStopping                   = "stopping"
	ApplicationLaunchStopped                    = "stopped"
	ApplicationLaunchExited                     = "exited"
	ApplicationLaunchFailed                     = "failed"
	ApplicationLaunchUnknown                    = "unknown"
	WechatApplicationID                         = "wechat"
	applicationLaunchIDMaxSize                  = 128
	ApplicationLaunchErrNotInstalled            = "not_installed"
	ApplicationLaunchErrArchitectureUnsupported = "architecture_unsupported"
	ApplicationLaunchErrCgroupUnavailable       = "cgroup_unavailable"
	ApplicationLaunchErrLaunchFailed            = "launch_failed"
	ApplicationLaunchErrStopUnconfirmed         = "stop_unconfirmed"
	ApplicationLaunchErrStorageError            = "storage_error"
)

var applicationLaunchIDPattern = regexp.MustCompile(`^launch_[A-Za-z0-9][A-Za-z0-9._:-]{0,120}$`)

// ApplicationLaunch is the durable Server view of one fixed-catalog process
// launch, pinned to the Host runtime identity that accepted it.
type ApplicationLaunch struct {
	HostID            string
	OwnerID           string
	ID                string
	ApplicationID     string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	Status            string
	ErrorCode         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func ValidApplicationLaunchID(value string) bool {
	return len(value) <= applicationLaunchIDMaxSize && applicationLaunchIDPattern.MatchString(value)
}

func ValidHostApplicationID(value string) bool {
	return value == WechatApplicationID
}

func (value ApplicationLaunch) Validate() error {
	if !domain.ValidHostID(value.HostID) || !domain.ValidUserID(value.OwnerID) || !ValidApplicationLaunchID(value.ID) ||
		!ValidHostApplicationID(value.ApplicationID) || value.RuntimeGeneration < 1 || value.RuntimeInstanceID == "" ||
		len(value.RuntimeInstanceID) > 128 || value.ControlVersion < 0 || !validApplicationLaunchStatus(value.Status) || !validApplicationLaunchErrorCode(value.ErrorCode) {
		return domain.InvalidArgument("application_launch", "application launch identity or status is invalid")
	}
	return nil
}

func validApplicationLaunchErrorCode(value string) bool {
	switch value {
	case "", ApplicationLaunchErrNotInstalled, ApplicationLaunchErrArchitectureUnsupported, ApplicationLaunchErrCgroupUnavailable,
		ApplicationLaunchErrLaunchFailed, ApplicationLaunchErrStopUnconfirmed, ApplicationLaunchErrStorageError:
		return true
	default:
		return false
	}
}

func validApplicationLaunchStatus(value string) bool {
	switch value {
	case ApplicationLaunchStarting, ApplicationLaunchRunning, ApplicationLaunchStopping, ApplicationLaunchStopped,
		ApplicationLaunchExited, ApplicationLaunchFailed, ApplicationLaunchUnknown:
		return true
	default:
		return false
	}
}

type ApplicationLaunchStore interface {
	ReserveApplicationLaunch(context.Context, ApplicationLaunch) (ApplicationLaunch, bool, error)
	GetLatestApplicationLaunch(context.Context, string, string, string) (ApplicationLaunch, error)
	GetApplicationLaunch(context.Context, string, string, string, string) (ApplicationLaunch, error)
	UpdateApplicationLaunch(context.Context, ApplicationLaunch) (ApplicationLaunch, error)
}
