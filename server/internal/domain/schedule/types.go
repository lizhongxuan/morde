package schedule

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain/task"
)

type Status string

const (
	StatusEnabled   Status = "enabled"
	StatusDisabled  Status = "disabled"
	StatusCompleted Status = "completed"
)

func (s Status) Valid() bool {
	return s == StatusEnabled || s == StatusDisabled || s == StatusCompleted
}

type TimingKind string

const (
	TimingOnce   TimingKind = "once"
	TimingDaily  TimingKind = "daily"
	TimingWeekly TimingKind = "weekly"
)

type Timing struct {
	Kind      TimingKind `json:"kind"`
	RunAt     string     `json:"run_at,omitempty"`
	StartsOn  string     `json:"starts_on,omitempty"`
	LocalTime string     `json:"local_time,omitempty"`
	TimeZone  string     `json:"time_zone"`
	Weekdays  []int      `json:"weekdays,omitempty"`
}

type MisfirePolicy string

// MisfireGrace is the normal polling tolerance before an occurrence is treated
// as missed. A poll observed at or before this duration remains on time.
const MisfireGrace = time.Minute

const (
	MisfireSkip    MisfirePolicy = "skip"
	MisfireRunOnce MisfirePolicy = "run_once"
)

func (p MisfirePolicy) Valid() bool { return p == MisfireSkip || p == MisfireRunOnce }

type NotificationEvent string

const (
	NotifyCompleted   NotificationEvent = "completed"
	NotifyFailed      NotificationEvent = "failed"
	NotifyWaitingUser NotificationEvent = "waiting_user"
)

func (e NotificationEvent) Valid() bool {
	return e == NotifyCompleted || e == NotifyFailed || e == NotifyWaitingUser
}

type ObservationField string

const (
	ObservationText  ObservationField = "text"
	ObservationValue ObservationField = "value"
)

type ObservationOutcome string

const (
	ObservationUnknown  ObservationOutcome = "unknown"
	ObservationBaseline ObservationOutcome = "baseline"
	ObservationChanged  ObservationOutcome = "changed"
	ObservationNoChange ObservationOutcome = "no_change"
)

func (o ObservationOutcome) Valid() bool {
	return o == ObservationUnknown || o == ObservationBaseline || o == ObservationChanged || o == ObservationNoChange
}

// ObservationSpec is a user-selected web field sampled by the trusted runtime.
// The selector is passed to the browser's CSS querySelector API, never evaluated as script.
type ObservationSpec struct {
	URL      string           `json:"url"`
	Selector string           `json:"selector"`
	Field    ObservationField `json:"field"`
}

func (o ObservationSpec) Validate() error {
	if len(o.URL) > 2048 || strings.TrimSpace(o.URL) != o.URL || o.URL == "" || strings.ContainsAny(o.URL, "\r\n\t\x00") {
		return errors.New("observation url must be an absolute HTTP or HTTPS URL")
	}
	parsed, err := url.Parse(o.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return errors.New("observation url must be an absolute HTTP or HTTPS URL without credentials or a fragment")
	}
	if len(o.Selector) == 0 || len(o.Selector) > 1024 || strings.TrimSpace(o.Selector) != o.Selector || strings.ContainsRune(o.Selector, '\x00') {
		return errors.New("observation selector is required and must be at most 1024 bytes")
	}
	if o.Field != ObservationText && o.Field != ObservationValue {
		return errors.New("observation field must be text or value")
	}
	return nil
}

func (o ObservationSpec) ConfigHash() string {
	encoded, _ := json.Marshal(o)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type RunStatus string

const (
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunWaitingUser RunStatus = "waiting_user"
	RunPausing     RunStatus = "pausing"
	RunPaused      RunStatus = "paused"
	RunStopping    RunStatus = "stopping"
	RunCompleted   RunStatus = "completed"
	RunFailed      RunStatus = "failed"
	RunCancelled   RunStatus = "cancelled"
	RunSkipped     RunStatus = "skipped"
	RunMissed      RunStatus = "missed"
)

func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunWaitingUser, RunPausing, RunPaused, RunStopping,
		RunCompleted, RunFailed, RunCancelled, RunSkipped, RunMissed:
		return true
	default:
		return false
	}
}

func (s RunStatus) Active() bool {
	switch s {
	case RunQueued, RunRunning, RunWaitingUser, RunPausing, RunPaused, RunStopping:
		return true
	default:
		return false
	}
}

type SkipReason string

const (
	SkipDSTGap             SkipReason = "dst_gap"
	SkipOverlap            SkipReason = "overlap"
	SkipAuthorizationEnded SkipReason = "authorization_ended"
)

type Action string

const (
	ActionEnable  Action = "enable"
	ActionDisable Action = "disable"
)

type CurrentRunAction string

const (
	CurrentRunContinue CurrentRunAction = "continue"
	CurrentRunCancel   CurrentRunAction = "cancel"
)

type Schedule struct {
	ID                    string
	OwnerID               string
	Version               int64
	Status                Status
	HostID                string
	ModelID               string
	Objective             string
	Timing                Timing
	Budget                task.Budget
	Authorization         task.Authorization
	MisfirePolicy         MisfirePolicy
	NotificationEvents    []NotificationEvent
	Observation           *ObservationSpec
	ObservationConfigHash string
	BaselineFingerprint   string
	NextOccurrenceKey     string
	NextRunAt             *time.Time
	NextCheckAt           *time.Time
	LastRun               *Run
	CurrentRun            *Run
	CreatedAt             time.Time
	UpdatedAt             time.Time
	ExecutionEpoch        int64
}

type Run struct {
	ID                     string
	ScheduleID             string
	ScheduleVersion        int64
	OccurrenceKey          string
	ScheduledAt            *time.Time
	CoalescedCount         int64
	Status                 RunStatus
	SkipReason             *SkipReason
	TaskID                 *string
	NotificationEvents     []NotificationEvent
	Observation            *ObservationSpec
	ObservationConfigHash  string
	ObservationOutcome     ObservationOutcome
	ObservationFingerprint string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	FinishedAt             *time.Time
}

func IsMisfire(scheduledAt, observedAt time.Time) bool {
	return scheduledAt.Before(observedAt.Add(-MisfireGrace))
}

type Occurrence struct {
	Key            string
	ScheduledAt    *time.Time
	CheckAt        time.Time
	Status         RunStatus
	SkipReason     *SkipReason
	CoalescedCount int64
}

var (
	datePattern      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	localTimePattern = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)
	idAlphabet       = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func NewID() (string, error)    { return newID('j') }
func NewRunID() (string, error) { return newID('y') }

func ValidID(value string) bool    { return validID(value, 'j') }
func ValidRunID(value string) bool { return validID(value, 'y') }

func newID(prefix byte) (string, error) {
	var value [8]byte
	value[0] = prefix
	max := big.NewInt(int64(len(idAlphabet)))
	for i := 1; i < len(value); i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate schedule id: %w", err)
		}
		value[i] = idAlphabet[n.Int64()]
	}
	return string(value[:]), nil
}

func validID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !strings.ContainsRune(idAlphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

func (t Timing) Validate() error {
	if t.TimeZone == "" || len(t.TimeZone) > 64 {
		return errors.New("time_zone is required")
	}
	location, err := time.LoadLocation(t.TimeZone)
	if err != nil {
		return errors.New("time_zone must be an IANA timezone")
	}
	switch t.Kind {
	case TimingOnce:
		if t.RunAt == "" || t.StartsOn != "" || t.LocalTime != "" || len(t.Weekdays) != 0 || !hasRFC3339Offset(t.RunAt) {
			return errors.New("once timing requires run_at with an explicit RFC3339 offset and time_zone")
		}
		if _, err := time.Parse(time.RFC3339Nano, t.RunAt); err != nil {
			return errors.New("run_at must be RFC3339")
		}
	case TimingDaily, TimingWeekly:
		if t.RunAt != "" || !datePattern.MatchString(t.StartsOn) || !validDate(t.StartsOn) || !localTimePattern.MatchString(t.LocalTime) {
			return errors.New("recurring timing requires starts_on and local_time")
		}
		if t.Kind == TimingDaily && len(t.Weekdays) != 0 {
			return errors.New("daily timing cannot specify weekdays")
		}
		if t.Kind == TimingWeekly {
			if len(t.Weekdays) < 1 || len(t.Weekdays) > 7 {
				return errors.New("weekly timing requires one to seven weekdays")
			}
			seen := make(map[int]struct{}, len(t.Weekdays))
			for _, day := range t.Weekdays {
				if day < 1 || day > 7 {
					return errors.New("weekday must use ISO weekday numbering")
				}
				if _, ok := seen[day]; ok {
					return errors.New("weekdays must be unique")
				}
				seen[day] = struct{}{}
			}
		}
	default:
		return errors.New("timing kind is invalid")
	}
	_ = location
	return nil
}

func Validate(s Schedule) error {
	if !ValidID(s.ID) || s.OwnerID == "" || s.Version < 1 || !s.Status.Valid() ||
		s.HostID == "" || s.ModelID == "" || strings.TrimSpace(s.Objective) == "" || len(s.Objective) > 32768 ||
		s.Authorization.Kind == "" || !s.MisfirePolicy.Valid() || s.ExecutionEpoch < 1 {
		return errors.New("schedule fields are invalid")
	}
	if err := s.Timing.Validate(); err != nil {
		return err
	}
	if err := s.Budget.Validate(); err != nil {
		return err
	}
	seen := make(map[NotificationEvent]struct{}, len(s.NotificationEvents))
	for _, event := range s.NotificationEvents {
		if !event.Valid() {
			return errors.New("notification event is invalid")
		}
		if _, ok := seen[event]; ok {
			return errors.New("notification events must be unique")
		}
		seen[event] = struct{}{}
	}
	if s.Observation == nil {
		if s.ObservationConfigHash != "" || s.BaselineFingerprint != "" {
			return errors.New("schedule without observation cannot have observation state")
		}
	} else if err := s.Observation.Validate(); err != nil {
		return err
	} else if s.ObservationConfigHash != s.Observation.ConfigHash() || (s.BaselineFingerprint != "" && !validFingerprint(s.BaselineFingerprint)) {
		return errors.New("schedule observation state is invalid")
	}
	if s.Status == StatusEnabled && (s.NextOccurrenceKey == "" || s.NextCheckAt == nil) {
		return errors.New("enabled schedule requires a next occurrence")
	}
	return nil
}

func validFingerprint(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func FirstOccurrence(t Timing, now time.Time) (Occurrence, error) {
	if err := t.Validate(); err != nil {
		return Occurrence{}, err
	}
	if t.Kind == TimingOnce {
		runAt, _ := time.Parse(time.RFC3339Nano, t.RunAt)
		runAt = runAt.UTC()
		return Occurrence{Key: "once:" + runAt.Format(time.RFC3339Nano), ScheduledAt: &runAt, CheckAt: runAt, Status: RunQueued}, nil
	}
	date, _ := time.Parse("2006-01-02", t.StartsOn)
	location, _ := time.LoadLocation(t.TimeZone)
	localNow := now.In(location)
	if today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC); today.After(date) {
		date = today
	}
	for tries := 0; tries < 370; tries++ {
		if t.Kind != TimingWeekly || containsWeekday(t.Weekdays, isoWeekday(date)) {
			occurrence, err := occurrenceOnDate(t, date)
			if err != nil {
				return Occurrence{}, err
			}
			if occurrence.CheckAt.After(now) || occurrence.CheckAt.Equal(now) {
				return occurrence, nil
			}
		}
		date = date.AddDate(0, 0, 1)
	}
	return Occurrence{}, errors.New("could not find next schedule occurrence")
}

func NextOccurrence(t Timing, currentKey string) (Occurrence, error) {
	if err := t.Validate(); err != nil {
		return Occurrence{}, err
	}
	if t.Kind == TimingOnce {
		return Occurrence{}, nil
	}
	date, err := dateFromOccurrenceKey(currentKey)
	if err != nil {
		return Occurrence{}, err
	}
	for tries := 0; tries < 370; tries++ {
		date = date.AddDate(0, 0, 1)
		if t.Kind != TimingWeekly || containsWeekday(t.Weekdays, isoWeekday(date)) {
			return occurrenceOnDate(t, date)
		}
	}
	return Occurrence{}, errors.New("could not find next schedule occurrence")
}

// LatestDue coalesces all elapsed occurrences into the latest elapsed local slot.
func LatestDue(t Timing, current Occurrence, now time.Time) (Occurrence, int64, error) {
	if t.Kind == TimingOnce || current.CheckAt.After(now) {
		return current, 0, nil
	}
	latest := current
	var count int64
	for count < 100000 {
		next, err := NextOccurrence(t, latest.Key)
		if err != nil {
			return Occurrence{}, 0, err
		}
		if next.CheckAt.After(now) {
			return latest, count, nil
		}
		latest = next
		count++
	}
	return Occurrence{}, 0, errors.New("too many missed schedule occurrences")
}

func occurrenceOnDate(t Timing, date time.Time) (Occurrence, error) {
	hour, minute, _ := timeFields(t.LocalTime)
	location, err := time.LoadLocation(t.TimeZone)
	if err != nil {
		return Occurrence{}, errors.New("time_zone must be an IANA timezone")
	}
	dateKey := date.Format("2006-01-02")
	key := dateKey + "T" + t.LocalTime
	scheduled, check, gap := resolveLocal(date.Year(), date.Month(), date.Day(), hour, minute, location)
	if gap {
		reason := SkipDSTGap
		return Occurrence{Key: key, CheckAt: check, Status: RunSkipped, SkipReason: &reason}, nil
	}
	return Occurrence{Key: key, ScheduledAt: &scheduled, CheckAt: scheduled, Status: RunQueued}, nil
}

func resolveLocal(year int, month time.Month, day, hour, minute int, location *time.Location) (time.Time, time.Time, bool) {
	civil := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	offsets := make(map[int]struct{})
	for instant := civil.Add(-48 * time.Hour); !instant.After(civil.Add(48 * time.Hour)); instant = instant.Add(time.Hour) {
		_, offset := instant.In(location).Zone()
		offsets[offset] = struct{}{}
	}
	candidates := make([]time.Time, 0, 2)
	for offset := range offsets {
		instant := civil.Add(-time.Duration(offset) * time.Second)
		local := instant.In(location)
		if local.Year() == year && local.Month() == month && local.Day() == day && local.Hour() == hour && local.Minute() == minute {
			candidates = append(candidates, instant.UTC())
		}
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
		return candidates[0], candidates[0], false
	}
	// For a skipped wall time, schedule the ledger write at the transition's first valid local minute.
	wall := civil.Format("2006-01-02 15:04")
	for instant := civil.Add(-24 * time.Hour); !instant.After(civil.Add(48 * time.Hour)); instant = instant.Add(time.Minute) {
		local := instant.In(location)
		if local.Format("2006-01-02 15:04") > wall {
			return time.Time{}, instant.UTC(), true
		}
	}
	return time.Time{}, civil.Add(24 * time.Hour), true
}

func dateFromOccurrenceKey(key string) (time.Time, error) {
	if len(key) < 16 || key[10] != 'T' {
		return time.Time{}, errors.New("occurrence key is invalid")
	}
	date, err := time.Parse("2006-01-02", key[:10])
	if err != nil || !localTimePattern.MatchString(key[11:]) {
		return time.Time{}, errors.New("occurrence key is invalid")
	}
	return date, nil
}

func timeFields(value string) (int, int, int) {
	parsed, _ := time.Parse("15:04", value)
	return parsed.Hour(), parsed.Minute(), 0
}

func isoWeekday(date time.Time) int {
	day := int(date.Weekday())
	if day == 0 {
		return 7
	}
	return day
}

func containsWeekday(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validDate(value string) bool {
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func hasRFC3339Offset(value string) bool {
	return strings.HasSuffix(value, "Z") || regexp.MustCompile(`[+-]\d{2}:\d{2}$`).MatchString(value)
}
