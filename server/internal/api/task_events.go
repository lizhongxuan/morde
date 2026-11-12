package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (h *api) streamTaskEvents(service TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		after, ok := eventCursor(c)
		if !ok {
			return
		}
		events, err := service.ListEvents(c.Request.Context(), principal, id, after, 100)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeEventCursorExpired && events.CurrentSequence > after {
				flusher, startErr := beginTaskEventStream(c)
				if startErr != nil {
					writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
					return
				}
				if writeErr := writeTaskSnapshotRequired(c.Writer, events.CurrentSequence); writeErr != nil {
					return
				}
				flusher.Flush()
				return
			}
			writeError(c, err)
			return
		}
		if isTerminalTaskStatus(events.Status) && len(events.Items) == 0 {
			if after >= events.CurrentSequence {
				c.Status(http.StatusNoContent)
				return
			}
			flusher, startErr := beginTaskEventStream(c)
			if startErr != nil {
				writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
				return
			}
			if writeErr := writeTaskSnapshotRequired(c.Writer, events.CurrentSequence); writeErr != nil {
				return
			}
			flusher.Flush()
			return
		}
		if err := validateTaskEvents(events.Items, after); err != nil {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		flusher, err := beginTaskEventStream(c)
		if err != nil {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		if err := writeTaskEvents(c.Writer, events.Items); err != nil {
			return
		}
		flusher.Flush()
		last := after
		if len(events.Items) > 0 {
			last = events.Items[len(events.Items)-1].Sequence
		}
		if isTerminalTaskStatus(events.Status) && last >= events.CurrentSequence {
			return
		}
		poll := time.NewTicker(time.Second)
		keepalive := time.NewTicker(15 * time.Second)
		defer poll.Stop()
		defer keepalive.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-keepalive.C:
				if _, err := fmt.Fprint(c.Writer, ": keep-alive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-poll.C:
				newEvents, err := service.ListEvents(c.Request.Context(), principal, id, last, 100)
				if err != nil {
					if domain.CodeOf(err) == domain.CodeEventCursorExpired && newEvents.CurrentSequence > last {
						if writeErr := writeTaskSnapshotRequired(c.Writer, newEvents.CurrentSequence); writeErr == nil {
							flusher.Flush()
						}
					}
					return
				}
				if len(newEvents.Items) == 0 {
					if isTerminalTaskStatus(newEvents.Status) {
						if newEvents.CurrentSequence > last {
							if writeErr := writeTaskSnapshotRequired(c.Writer, newEvents.CurrentSequence); writeErr == nil {
								flusher.Flush()
							}
						}
						return
					}
					continue
				}
				if err := validateTaskEvents(newEvents.Items, last); err != nil {
					return
				}
				if err := writeTaskEvents(c.Writer, newEvents.Items); err != nil {
					return
				}
				last = newEvents.Items[len(newEvents.Items)-1].Sequence
				flusher.Flush()
				if isTerminalTaskStatus(newEvents.Status) && last >= newEvents.CurrentSequence {
					return
				}
			}
		}
	}
}

func beginTaskEventStream(c *gin.Context) (http.Flusher, error) {
	controller := http.NewResponseController(c.Writer)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("task event stream requires flushing")
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	return flusher, nil
}

func writeTaskSnapshotRequired(writer io.Writer, sequence int64) error {
	if sequence < 1 {
		return errors.New("task event snapshot sequence is invalid")
	}
	_, err := fmt.Fprintf(writer, "id: %d\nevent: task.snapshot_required\ndata: {\"sequence\":%d}\n\n", sequence, sequence)
	return err
}

func isTerminalTaskStatus(status task.Status) bool {
	return status == task.StatusCompleted || status == task.StatusFailed || status == task.StatusCancelled
}

func eventCursor(c *gin.Context) (int64, bool) {
	queryValue, hasQuery := c.Request.URL.Query()["after"]
	var after int64
	if hasQuery {
		if len(queryValue) != 1 {
			writeError(c, domain.InvalidArgument("after", "event cursor must appear once"))
			return 0, false
		}
		parsed, err := parseEventCursor(queryValue[0])
		if err != nil {
			writeError(c, domain.InvalidArgument("after", "event cursor is invalid"))
			return 0, false
		}
		after = parsed
	}
	headerValues := c.Request.Header.Values("Last-Event-ID")
	if len(headerValues) > 1 {
		writeError(c, domain.InvalidArgument("Last-Event-ID", "event cursor header must appear once"))
		return 0, false
	}
	if len(headerValues) == 0 {
		return after, true
	}
	headerAfter, err := parseEventCursor(headerValues[0])
	if err != nil {
		writeError(c, domain.InvalidArgument("Last-Event-ID", "event cursor header is invalid"))
		return 0, false
	}
	if hasQuery && after > headerAfter {
		return after, true
	}
	return headerAfter, true
}

func parseEventCursor(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("event cursor is empty")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errors.New("event cursor is not decimal")
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("event cursor is out of range")
	}
	return parsed, nil
}

func writeTaskEvents(writer gin.ResponseWriter, events []task.Event) error {
	for _, event := range events {
		if !validTaskEvent(event) {
			return errors.New("task event is invalid")
		}
	}
	for _, event := range events {
		payload, err := publicTaskEventPayload(event)
		if err != nil {
			return fmt.Errorf("sanitize task event: %w", err)
		}
		data, err := json.Marshal(taskEventResponse{
			TaskID: event.TaskID, Sequence: event.Sequence, Type: event.Type,
			CreatedAt: event.CreatedAt, Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("encode task event: %w", err)
		}
		if _, err := fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data); err != nil {
			return err
		}
	}
	return nil
}

func publicTaskEventPayload(event task.Event) (json.RawMessage, error) {
	allowed, ok := publicTaskEventFields(event.Type)
	if !ok {
		return json.RawMessage(`{}`), nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload == nil {
		return json.RawMessage(`{}`), nil
	}
	filtered := filterTaskEventFields(payload, allowed)
	if event.Type == "task.step" {
		if detailBytes, exists := filtered["detail"]; exists {
			var detail map[string]json.RawMessage
			if err := json.Unmarshal(detailBytes, &detail); err != nil || detail == nil {
				filtered["detail"] = json.RawMessage(`{}`)
			} else {
				encodedDetail, err := json.Marshal(filterTaskEventFields(detail, []string{"phase", "status", "kind", "tool", "usage_known", "requires_reconciliation"}))
				if err != nil {
					return nil, err
				}
				filtered["detail"] = encodedDetail
			}
		}
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func publicTaskEventFields(eventType string) ([]string, bool) {
	switch eventType {
	case "task.status":
		return []string{"status", "reason", "batch_item_id", "batch_attempt"}, true
	case "task.step":
		return []string{"step_id", "status", "kind", "phase", "tool", "call_id", "title", "summary", "detail"}, true
	case "task.operation":
		return []string{"operation_id", "status", "reason", "requires_reconciliation"}, true
	case "task.message":
		return []string{"role", "message_id", "turn_id", "task_revision", "done"}, true
	case "task.batch_item":
		return []string{"batch_item_id", "key", "status", "attempt"}, true
	case "task.artifact":
		return []string{"artifact_id", "step_id", "label", "media_type", "path", "workspace_id", "revision", "size_bytes", "sha256", "batch_item_id", "batch_attempt", "redacted"}, true
	case "task.waiting_user":
		return []string{"status", "reason", "required_revision", "applied_revision"}, true
	case "task.response":
		return []string{"pending_id", "pending_version", "clarification_id", "clarification_version", "decision", "reason", "status", "idempotency_replayed", "lease_version", "file_delivery_intent"}, true
	case "task.result":
		return []string{"status", "reason", "task_revision"}, true
	default:
		return nil, false
	}
}

func filterTaskEventFields(payload map[string]json.RawMessage, allowed []string) map[string]json.RawMessage {
	filtered := make(map[string]json.RawMessage, len(allowed))
	for _, key := range allowed {
		if value, exists := payload[key]; exists {
			filtered[key] = value
		}
	}
	return filtered
}

func validateTaskEvents(events []task.Event, after int64) error {
	previous := after
	for _, event := range events {
		if !validTaskEvent(event) || event.Sequence != previous+1 {
			return errors.New("task event sequence is invalid")
		}
		previous = event.Sequence
	}
	return nil
}

func validTaskEvent(event task.Event) bool {
	return domain.ValidTaskID(event.TaskID) && event.Sequence > 0 && json.Valid(event.Payload) &&
		validTaskEventName(event.Type) && !event.CreatedAt.IsZero()
}

func validTaskEventName(value string) bool {
	switch value {
	case "task.status", "task.step", "task.operation", "task.message", "task.batch_item", "task.artifact", "task.waiting_user", "task.response", "task.result":
		return true
	default:
		if len(value) > 128 || !strings.HasPrefix(value, "task.") {
			return false
		}
		name := strings.TrimPrefix(value, "task.")
		if name == "" {
			return false
		}
		for _, char := range name {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '_' && char != '.' && char != '-' {
				return false
			}
		}
		return true
	}
}

type taskEventResponse struct {
	TaskID    string          `json:"task_id"`
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}
