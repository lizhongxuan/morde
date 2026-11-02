package hub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const (
	MaxFrameBytes          = 64 << 10
	MaxCallIDBytes         = 80
	MaxOpenURLBytes        = 2048
	MaxNotifyTitleBytes    = 100
	MaxNotifyTextBytes     = 2000
	MaxHubPathBytes        = 1024
	MaxWorkspaceWriteBytes = 300 << 20
	MaxScreenBytes         = 40 << 10
	MaxFileReadBytes       = 32 << 10
	MaxCommandBytes        = 8 << 10
	MaxCommandOutput       = 16 << 10
	MaxFilesPerResult      = 128

	ActionInfo       = "info"
	ActionOpen       = "open"
	ActionNotify     = "notify"
	ActionScreenshot = "screenshot"
	ActionFiles      = "files"
	ActionFileGet    = "file.get"
	ActionFilePut    = "file.put" // Remains unavailable until its target and durable sync fences are integrated.
	ActionCommand    = "command"
)

type Frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	To      string          `json:"to,omitempty"`
	Action  string          `json:"action,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
	Code    string          `json:"code,omitempty"`
}

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Online   bool   `json:"online"`
}

// TaskContext binds a protected Hub action to the current Task authorization
// and target grant. It is carried only in the server-generated caller context.
type TaskContext struct {
	TaskID               string `json:"task_id"`
	AuthorizationVersion int64  `json:"authorization_version"`
	TargetGrantVersion   int64  `json:"target_grant_version"`
	OperationID          string `json:"operation_id"`
}

// WorkspaceWriteArgs binds a future PC write to a Workspace-relative path and
// the exact bytes approved by the Task. Content transport remains F02-T10's
// responsibility and is intentionally not part of the Hub JSON frame.
type WorkspaceWriteArgs struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

// WorkspaceWriteReceipt proves only that the target durably applied these
// bytes locally; cloud sync must be confirmed separately before Task completion.
type WorkspaceWriteReceipt struct {
	OperationID        string `json:"operation_id"`
	WorkspaceID        string `json:"workspace_id"`
	Path               string `json:"path"`
	SizeBytes          int64  `json:"size_bytes"`
	SHA256             string `json:"sha256"`
	TargetGrantVersion int64  `json:"target_grant_version"`
	Stage              string `json:"stage"`
}

type ProtocolError struct {
	Code    string
	Message string
}

func (e ProtocolError) Error() string { return e.Message }

func DecodeFrame(raw []byte) (Frame, error) {
	if len(raw) == 0 || len(raw) > MaxFrameBytes || !utf8.Valid(raw) {
		return Frame{}, ProtocolError{Code: "bad_frame", Message: "frame is empty, too large, or invalid UTF-8"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var frame Frame
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, ProtocolError{Code: "bad_frame", Message: "frame is not valid JSON"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Frame{}, ProtocolError{Code: "bad_frame", Message: "frame must contain one JSON object"}
	}
	if frame.Type == "" {
		return Frame{}, ProtocolError{Code: "bad_frame", Message: "frame type is required"}
	}
	return frame, nil
}

func ValidCallID(id string) bool {
	if len(id) < 4 || len(id) > MaxCallIDBytes {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && !strings.ContainsRune("._:-", char) {
			return false
		}
	}
	return true
}

func ValidateTaskContext(value TaskContext) error {
	if !domain.ValidTaskID(value.TaskID) || value.AuthorizationVersion < 1 || value.TargetGrantVersion < 1 || !ValidCallID(value.OperationID) {
		return ProtocolError{Code: "invalid_argument", Message: "task action context is invalid"}
	}
	return nil
}

func ValidateWorkspaceWriteArgs(raw json.RawMessage) (WorkspaceWriteArgs, error) {
	var value WorkspaceWriteArgs
	if err := decodeArgs(raw, &value); err != nil || !domain.ValidWorkspaceID(value.WorkspaceID) ||
		!validRelativePath(value.Path) || value.SizeBytes < 0 || value.SizeBytes > MaxWorkspaceWriteBytes || !validSHA256(value.SHA256) {
		return WorkspaceWriteArgs{}, ProtocolError{Code: "invalid_argument", Message: "workspace write metadata is invalid"}
	}
	return value, nil
}

func ValidateWorkspaceWriteReceipt(raw json.RawMessage, request WorkspaceWriteArgs, taskContext TaskContext) error {
	if err := ValidateTaskContext(taskContext); err != nil ||
		!domain.ValidWorkspaceID(request.WorkspaceID) || !validRelativePath(request.Path) ||
		request.SizeBytes < 0 || request.SizeBytes > MaxWorkspaceWriteBytes || !validSHA256(request.SHA256) {
		return ProtocolError{Code: "bad_result", Message: "workspace write receipt does not match a valid request"}
	}
	var receipt WorkspaceWriteReceipt
	if err := decodeArgs(raw, &receipt); err != nil || receipt.OperationID != taskContext.OperationID ||
		receipt.WorkspaceID != request.WorkspaceID || receipt.Path != request.Path || receipt.SizeBytes != request.SizeBytes ||
		receipt.SHA256 != request.SHA256 || receipt.TargetGrantVersion != taskContext.TargetGrantVersion || receipt.Stage != "local_applied" {
		return ProtocolError{Code: "bad_result", Message: "workspace write receipt is incomplete or mismatched"}
	}
	return nil
}

func ValidateHello(frame Frame) error {
	if frame.Type != "hello" || frame.ID != "" || frame.To != "" || frame.Action != "" ||
		len(frame.Args) > 0 || frame.OK != nil || len(frame.Body) > 0 || frame.Error != "" || frame.Message != "" || frame.Code != "" {
		return ProtocolError{Code: "bad_hello", Message: "hello must contain only its type"}
	}
	return nil
}

func ValidateCall(frame Frame) error {
	if frame.Type != "call" || !ValidCallID(frame.ID) || !domain.ValidClientInstallationID(frame.To) ||
		frame.OK != nil || len(frame.Body) > 0 || frame.Error != "" || frame.Message != "" || frame.Code != "" {
		return ProtocolError{Code: "bad_frame", Message: "call requires a valid id and target installation"}
	}
	if len(frame.Args) == 0 {
		frame.Args = json.RawMessage(`{}`)
	}
	if !json.Valid(frame.Args) || len(bytes.TrimSpace(frame.Args)) == 0 || bytes.TrimSpace(frame.Args)[0] != '{' {
		return ProtocolError{Code: "bad_frame", Message: "call args must be a JSON object"}
	}
	switch frame.Action {
	case ActionInfo:
		var args struct{}
		if err := decodeArgs(frame.Args, &args); err != nil {
			return ProtocolError{Code: "invalid_argument", Message: "info accepts no arguments"}
		}
	case ActionOpen:
		var args struct {
			URL string `json:"url"`
		}
		if err := decodeArgs(frame.Args, &args); err != nil || len(args.URL) == 0 || len(args.URL) > MaxOpenURLBytes {
			return ProtocolError{Code: "invalid_argument", Message: "open requires one HTTP(S) URL"}
		}
		parsed, err := url.ParseRequestURI(args.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return ProtocolError{Code: "invalid_argument", Message: "open accepts only HTTP(S) URLs without user information"}
		}
	case ActionNotify:
		var args struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := decodeArgs(frame.Args, &args); err != nil || !utf8.ValidString(args.Title) || !utf8.ValidString(args.Text) ||
			args.Text == "" || len(args.Title) > MaxNotifyTitleBytes || len(args.Text) > MaxNotifyTextBytes {
			return ProtocolError{Code: "invalid_argument", Message: fmt.Sprintf("notify requires text up to %d bytes and title up to %d bytes", MaxNotifyTextBytes, MaxNotifyTitleBytes)}
		}
	default:
		return ProtocolError{Code: "unknown_action", Message: "action is not available through the Hub"}
	}
	return nil
}

// ValidateTaskActionArgs validates actions that may only be dispatched by a
// running, authorized Task. ValidateCall intentionally continues to reject them.
func ValidateTaskActionArgs(action string, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return ProtocolError{Code: "invalid_argument", Message: "task action args must be a JSON object"}
	}
	switch action {
	case ActionScreenshot, ActionFiles:
		if action == ActionScreenshot {
			var args struct{}
			if err := decodeArgs(raw, &args); err != nil {
				return ProtocolError{Code: "invalid_argument", Message: "action accepts no arguments"}
			}
			break
		}
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeArgs(raw, &args); err != nil || args.Path != "" && !validRelativePath(args.Path) {
			return ProtocolError{Code: "invalid_argument", Message: "files accepts only an optional relative path"}
		}
	case ActionFileGet:
		var args struct {
			Path     string `json:"path"`
			MaxBytes int    `json:"max_bytes"`
		}
		if err := decodeArgs(raw, &args); err != nil || !validRelativePath(args.Path) || args.MaxBytes < 1 || args.MaxBytes > MaxFileReadBytes {
			return ProtocolError{Code: "invalid_argument", Message: "file.get requires a relative path and bounded max_bytes"}
		}
	case ActionFilePut:
		if _, err := ValidateWorkspaceWriteArgs(raw); err != nil {
			return ProtocolError{Code: "invalid_argument", Message: "file.put requires bounded Workspace write metadata"}
		}
	case ActionCommand:
		var args struct {
			Command   string `json:"command"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := decodeArgs(raw, &args); err != nil || !utf8.ValidString(args.Command) || strings.ContainsRune(args.Command, '\x00') ||
			len(args.Command) < 1 || len(args.Command) > MaxCommandBytes || args.TimeoutMS < 1000 || args.TimeoutMS > 300000 {
			return ProtocolError{Code: "invalid_argument", Message: "command and timeout_ms are outside the allowed bounds"}
		}
	default:
		return ProtocolError{Code: "unknown_action", Message: "task action is not available through the Hub"}
	}
	return nil
}

func ValidateResult(frame Frame) error {
	if frame.Type != "result" || !ValidCallID(frame.ID) || frame.OK == nil || len(frame.Body) > MaxFrameBytes ||
		frame.To != "" || frame.Action != "" || len(frame.Args) > 0 || frame.Code != "" {
		return ProtocolError{Code: "bad_frame", Message: "result requires a valid id and ok value"}
	}
	if *frame.OK {
		body := bytes.TrimSpace(frame.Body)
		if frame.Error != "" || frame.Message != "" || len(body) == 0 || !json.Valid(body) || body[0] != '{' {
			return ProtocolError{Code: "bad_frame", Message: "successful result requires an object body"}
		}
	} else if frame.Error == "" || len(frame.Error) > 64 || len(frame.Message) > 200 || len(frame.Body) > 0 {
		return ProtocolError{Code: "bad_frame", Message: "failed result requires a short error code"}
	}
	return nil
}

func ValidateActionResult(action string, frame Frame) error {
	if frame.OK == nil {
		return ProtocolError{Code: "bad_result", Message: "result status is missing"}
	}
	if !*frame.OK {
		if action == ActionCommand && frame.Error == "timeout" {
			return ProtocolError{Code: "unknown_result", Message: "command timed out before stop was confirmed"}
		}
		if !validResultError(frame.Error) {
			return ProtocolError{Code: "bad_result", Message: "result error code is not allowed"}
		}
		return nil
	}
	if len(frame.Body) == 0 || len(frame.Body) > MaxFrameBytes || !jsonObject(frame.Body) {
		return ProtocolError{Code: "bad_result", Message: "result body is empty or too large"}
	}
	switch action {
	case ActionInfo:
		var result struct {
			Name               string   `json:"name"`
			Platform           string   `json:"platform"`
			OS                 string   `json:"os"`
			Version            string   `json:"version"`
			Actions            []string `json:"actions"`
			TargetGrantVersion *int64   `json:"target_grant_version,omitempty"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || !utf8.ValidString(result.Name) || len(result.Name) > 100 ||
			!utf8.ValidString(result.Platform) || !validPlatform(result.Platform) ||
			!utf8.ValidString(result.OS) || len(result.OS) > 64 || !utf8.ValidString(result.Version) || len(result.Version) > 64 ||
			len(result.Actions) > 3+4 || (result.TargetGrantVersion != nil && *result.TargetGrantVersion < 1) {
			return ProtocolError{Code: "bad_result", Message: "info result may contain only bounded device metadata"}
		}
		seen := make(map[string]struct{}, len(result.Actions))
		for _, action := range result.Actions {
			if !validAdvertisedAction(action) {
				return ProtocolError{Code: "bad_result", Message: "info result contains an unavailable action"}
			}
			if _, exists := seen[action]; exists {
				return ProtocolError{Code: "bad_result", Message: "info result contains a duplicate action"}
			}
			seen[action] = struct{}{}
		}
	case ActionOpen:
		var result struct {
			Opened bool `json:"opened"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || !result.Opened {
			return ProtocolError{Code: "bad_result", Message: "open success must confirm that the URL opened"}
		}
	case ActionNotify:
		var result struct {
			Notified bool `json:"notified"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || !result.Notified {
			return ProtocolError{Code: "bad_result", Message: "notify success must confirm that the notification was shown"}
		}
	case ActionScreenshot:
		var result struct {
			MIMEType           string `json:"mime_type"`
			Width              int    `json:"width"`
			Height             int    `json:"height"`
			DataBase64         string `json:"data_base64"`
			TargetGrantVersion int64  `json:"target_grant_version"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || result.MIMEType != "image/jpeg" ||
			result.Width < 1 || result.Width > 4096 || result.Height < 1 || result.Height > 4096 ||
			len(result.DataBase64) == 0 || len(result.DataBase64) > base64.StdEncoding.EncodedLen(MaxScreenBytes) || result.TargetGrantVersion < 1 {
			return ProtocolError{Code: "bad_result", Message: "screenshot result must be a bounded JPEG image"}
		}
		decoded, err := base64.StdEncoding.DecodeString(result.DataBase64)
		if err != nil || len(decoded) > MaxScreenBytes {
			return ProtocolError{Code: "bad_result", Message: "screenshot result must be a bounded JPEG image"}
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
		if err != nil || config.Width != result.Width || config.Height != result.Height {
			return ProtocolError{Code: "bad_result", Message: "screenshot result JPEG metadata does not match its image"}
		}
	case ActionFiles:
		var result struct {
			TargetGrantVersion int64 `json:"target_grant_version"`
			Entries            []struct {
				Path      *string `json:"path"`
				Kind      *string `json:"kind"`
				SizeBytes *int64  `json:"size_bytes"`
			} `json:"entries"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || result.Entries == nil || len(result.Entries) > MaxFilesPerResult || result.TargetGrantVersion < 1 {
			return ProtocolError{Code: "bad_result", Message: "files result exceeds its entry limit"}
		}
		seen := make(map[string]struct{}, len(result.Entries))
		for _, entry := range result.Entries {
			if entry.Path == nil || entry.Kind == nil || entry.SizeBytes == nil || !validRelativePath(*entry.Path) ||
				(*entry.Kind != "file" && *entry.Kind != "directory") || *entry.SizeBytes < 0 {
				return ProtocolError{Code: "bad_result", Message: "files result contains an invalid entry"}
			}
			if _, exists := seen[*entry.Path]; exists {
				return ProtocolError{Code: "bad_result", Message: "files result contains a duplicate path"}
			}
			seen[*entry.Path] = struct{}{}
		}
	case ActionFileGet:
		var result struct {
			Path               string  `json:"path"`
			Text               *string `json:"text"`
			Truncated          *bool   `json:"truncated"`
			TargetGrantVersion int64   `json:"target_grant_version"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || !validRelativePath(result.Path) || result.Text == nil || result.Truncated == nil ||
			!utf8.ValidString(*result.Text) || len(*result.Text) > MaxFileReadBytes || result.TargetGrantVersion < 1 {
			return ProtocolError{Code: "bad_result", Message: "file.get result must contain bounded UTF-8 text and a relative path"}
		}
	case ActionFilePut:
		var receipt struct {
			OperationID        *string `json:"operation_id"`
			WorkspaceID        *string `json:"workspace_id"`
			Path               *string `json:"path"`
			SizeBytes          *int64  `json:"size_bytes"`
			SHA256             *string `json:"sha256"`
			TargetGrantVersion *int64  `json:"target_grant_version"`
			Stage              *string `json:"stage"`
		}
		if err := decodeArgs(frame.Body, &receipt); err != nil || receipt.OperationID == nil || !ValidCallID(*receipt.OperationID) ||
			receipt.WorkspaceID == nil || !domain.ValidWorkspaceID(*receipt.WorkspaceID) || receipt.Path == nil || !validRelativePath(*receipt.Path) ||
			receipt.SizeBytes == nil || *receipt.SizeBytes < 0 || *receipt.SizeBytes > MaxWorkspaceWriteBytes || receipt.SHA256 == nil || !validSHA256(*receipt.SHA256) ||
			receipt.TargetGrantVersion == nil || *receipt.TargetGrantVersion < 1 || receipt.Stage == nil || *receipt.Stage != "local_applied" {
			return ProtocolError{Code: "bad_result", Message: "file.put result must contain a valid local Workspace write receipt"}
		}
	case ActionCommand:
		var result struct {
			ExitCode           *int    `json:"exit_code"`
			Stdout             *string `json:"stdout"`
			Stderr             *string `json:"stderr"`
			TimedOut           *bool   `json:"timed_out"`
			Stopped            *bool   `json:"stopped"`
			TargetGrantVersion int64   `json:"target_grant_version"`
		}
		if err := decodeArgs(frame.Body, &result); err != nil || result.ExitCode == nil || result.Stdout == nil || result.Stderr == nil ||
			result.TimedOut == nil || result.Stopped == nil || !utf8.ValidString(*result.Stdout) || !utf8.ValidString(*result.Stderr) ||
			len(*result.Stdout) > MaxCommandOutput || len(*result.Stderr) > MaxCommandOutput || result.TargetGrantVersion < 1 {
			return ProtocolError{Code: "bad_result", Message: "command result exceeds its output bounds"}
		}
		if *result.TimedOut && !*result.Stopped {
			return ProtocolError{Code: "unknown_result", Message: "command timed out before stop was confirmed"}
		}
	default:
		return ProtocolError{Code: "unknown_action", Message: "action is not available through the Hub"}
	}
	return nil
}

func validAdvertisedAction(action string) bool {
	switch action {
	case ActionInfo, ActionOpen, ActionNotify, ActionScreenshot, ActionFiles, ActionFileGet, ActionCommand:
		return true
	default:
		return false
	}
}

func validRelativePath(value string) bool {
	if value == "" || len(value) > MaxHubPathBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") ||
		strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") ||
		(len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':') {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func validPlatform(platform string) bool {
	return platform == "desktop" || platform == "ios" || platform == "android"
}

func validResultError(code string) bool {
	switch code {
	case "cancelled", "denied", "failed", "notification_denied", "not_allowed", "permission_required", "unsupported", "timeout":
		return true
	default:
		return false
	}
}

func decodeArgs(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}
