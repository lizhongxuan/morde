package hub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestDecodeFrameRequiresOneBoundedKnownJSONObject(t *testing.T) {
	valid, err := DecodeFrame([]byte(`{"type":"ping"}`))
	if err != nil || valid.Type != "ping" {
		t.Fatalf("DecodeFrame(valid) = %#v, %v", valid, err)
	}
	for name, input := range map[string][]byte{
		"empty":           nil,
		"invalid utf8":    {0xff},
		"oversized":       bytes.Repeat([]byte("x"), MaxFrameBytes+1),
		"unknown field":   []byte(`{"type":"ping","secret":"ignored"}`),
		"multiple values": []byte(`{"type":"ping"}{"type":"pong"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeFrame(input); err == nil {
				t.Fatalf("DecodeFrame(%s) unexpectedly succeeded", name)
			}
		})
	}
}

func TestValidateHelloBindsIdentityToCredential(t *testing.T) {
	if err := ValidateHello(Frame{Type: "hello"}); err != nil {
		t.Fatalf("valid hello: %v", err)
	}
	if err := ValidateHello(Frame{Type: "hello", ID: "attacker-id"}); err == nil {
		t.Fatal("hello-provided identity was accepted")
	}
}

func TestValidateCallAllowsOnlyBoundedBasicActions(t *testing.T) {
	for name, frame := range map[string]Frame{
		"info":   {Type: "call", ID: "call-1", To: "i1234567", Action: ActionInfo, Args: json.RawMessage(`{}`)},
		"open":   {Type: "call", ID: "call-2", To: "i1234567", Action: ActionOpen, Args: json.RawMessage(`{"url":"https://example.com/path"}`)},
		"notify": {Type: "call", ID: "call-3", To: "i1234567", Action: ActionNotify, Args: json.RawMessage(`{"text":"ready","title":"Morde"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCall(frame); err != nil {
				t.Fatalf("valid %s call: %v", name, err)
			}
		})
	}
	for name, frame := range map[string]Frame{
		"unknown action": {Type: "call", ID: "call-4", To: "i1234567", Action: "shell", Args: json.RawMessage(`{}`)},
		"file URL":       {Type: "call", ID: "call-5", To: "i1234567", Action: ActionOpen, Args: json.RawMessage(`{"url":"file:///etc/passwd"}`)},
		"URL userinfo":   {Type: "call", ID: "call-6", To: "i1234567", Action: ActionOpen, Args: json.RawMessage(`{"url":"https://user:pass@example.com"}`)},
		"extra args":     {Type: "call", ID: "call-7", To: "i1234567", Action: ActionNotify, Args: json.RawMessage(`{"text":"hi","path":"/private"}`)},
		"empty notify":   {Type: "call", ID: "call-8", To: "i1234567", Action: ActionNotify, Args: json.RawMessage(`{"text":""}`)},
		"bad target":     {Type: "call", ID: "call-9", To: "not-an-installation", Action: ActionInfo, Args: json.RawMessage(`{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCall(frame); err == nil {
				t.Fatalf("invalid %s call was accepted", name)
			}
		})
	}
}

func TestValidateResultRequiresDefinitiveShape(t *testing.T) {
	ok := true
	if err := ValidateResult(Frame{Type: "result", ID: "call-1", OK: &ok, Body: json.RawMessage(`{"opened":true}`)}); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	if err := ValidateResult(Frame{Type: "result", ID: "call-1", OK: &ok}); err == nil {
		t.Fatal("result without a body was accepted")
	}
	failed := false
	if err := ValidateResult(Frame{Type: "result", ID: "call-1", OK: &failed}); err == nil {
		t.Fatal("failed result without an error code was accepted")
	}
}

func TestValidateActionResultKeepsResultsWithinActionContract(t *testing.T) {
	ok := true
	failed := false
	for name, test := range map[string]struct {
		action string
		frame  Frame
	}{
		"info":         {action: ActionInfo, frame: Frame{Type: "result", ID: "call-1", OK: &ok, Body: json.RawMessage(`{"name":"Phone","platform":"ios","actions":["info","open"]}`)}},
		"info actions": {action: ActionInfo, frame: Frame{Type: "result", ID: "call-1", OK: &ok, Body: json.RawMessage(`{"name":"PC","platform":"desktop","actions":["info","screenshot","files","file.get","command"],"target_grant_version":2}`)}},
		"open":         {action: ActionOpen, frame: Frame{Type: "result", ID: "call-2", OK: &ok, Body: json.RawMessage(`{"opened":true}`)}},
		"notify":       {action: ActionNotify, frame: Frame{Type: "result", ID: "call-3", OK: &ok, Body: json.RawMessage(`{"notified":true}`)}},
		"action error": {action: ActionOpen, frame: Frame{Type: "result", ID: "call-4", OK: &failed, Error: "denied"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateActionResult(test.action, test.frame); err != nil {
				t.Fatalf("valid result: %v", err)
			}
		})
	}
	for name, test := range map[string]struct {
		action string
		body   string
		error  string
	}{
		"info path leak":        {action: ActionInfo, body: `{"name":"Phone","platform":"ios","path":"/private"}`},
		"info invalid platform": {action: ActionInfo, body: `{"name":"Phone","platform":"server"}`},
		"open incomplete":       {action: ActionOpen, body: `{"opened":false}`},
		"notify incomplete":     {action: ActionNotify, body: `{"notified":false}`},
		"unknown action error":  {action: ActionNotify, error: "shell_executed"},
	} {
		t.Run(name, func(t *testing.T) {
			frame := Frame{Type: "result", ID: "call-5", OK: &ok, Body: json.RawMessage(test.body)}
			if test.error != "" {
				frame.OK, frame.Error = &failed, test.error
			}
			if err := ValidateActionResult(test.action, frame); err == nil {
				t.Fatal("result outside the action contract was accepted")
			}
		})
	}
}

func TestValidateCallDoesNotExposeTaskOnlyActions(t *testing.T) {
	for _, action := range []string{ActionScreenshot, ActionFiles, ActionFileGet, ActionCommand} {
		if err := ValidateCall(Frame{Type: "call", ID: "call-task", To: "i1234567", Action: action, Args: json.RawMessage(`{}`)}); err == nil {
			t.Fatalf("ordinary Hub call exposed Task-only action %q", action)
		}
	}
}

func TestValidateTaskContextRequiresTaskAndGrantFence(t *testing.T) {
	valid := TaskContext{TaskID: "q1234567", AuthorizationVersion: 1, TargetGrantVersion: 3, OperationID: "op_0123456789abcdef"}
	if err := ValidateTaskContext(valid); err != nil {
		t.Fatalf("valid Task context: %v", err)
	}
	for name, value := range map[string]TaskContext{
		"missing task":         {AuthorizationVersion: 1, TargetGrantVersion: 1, OperationID: valid.OperationID},
		"missing auth version": {TaskID: valid.TaskID, TargetGrantVersion: 1, OperationID: valid.OperationID},
		"missing grant":        {TaskID: valid.TaskID, AuthorizationVersion: 1, OperationID: valid.OperationID},
		"invalid operation":    {TaskID: valid.TaskID, AuthorizationVersion: 1, TargetGrantVersion: 1, OperationID: "bad/id"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTaskContext(value); err == nil {
				t.Fatalf("invalid Task context accepted: %#v", value)
			}
		})
	}
}

func TestValidateTaskActionArgsAreStrictlyBounded(t *testing.T) {
	for name, value := range map[string]struct {
		action string
		args   string
	}{
		"screenshot": {action: ActionScreenshot, args: `{}`},
		"files":      {action: ActionFiles, args: `{}`},
		"files path": {action: ActionFiles, args: `{"path":"workspace/docs"}`},
		"file.get":   {action: ActionFileGet, args: `{"path":"workspace/notes.txt","max_bytes":32768}`},
		"command":    {action: ActionCommand, args: `{"command":"echo hello","timeout_ms":300000}`},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTaskActionArgs(value.action, json.RawMessage(value.args)); err != nil {
				t.Fatalf("valid %s args: %v", name, err)
			}
		})
	}
	for name, value := range map[string]struct {
		action string
		args   string
	}{
		"unknown action":      {action: "file.put", args: `{}`},
		"extra screen arg":    {action: ActionScreenshot, args: `{"path":"/private"}`},
		"files absolute path": {action: ActionFiles, args: `{"path":"/private"}`},
		"files parent path":   {action: ActionFiles, args: `{"path":"workspace/../private"}`},
		"files extra field":   {action: ActionFiles, args: `{"path":"docs","root":"/private"}`},
		"absolute path":       {action: ActionFileGet, args: `{"path":"/etc/passwd","max_bytes":10}`},
		"parent path":         {action: ActionFileGet, args: `{"path":"workspace/../secrets","max_bytes":10}`},
		"dot path":            {action: ActionFileGet, args: `{"path":"workspace/./notes","max_bytes":10}`},
		"windows path":        {action: ActionFileGet, args: `{"path":"C:/Users/secret.txt","max_bytes":10}`},
		"backslash path":      {action: ActionFileGet, args: `{"path":"workspace\\secret.txt","max_bytes":10}`},
		"file read too large": {action: ActionFileGet, args: `{"path":"workspace/notes","max_bytes":32769}`},
		"command too long":    {action: ActionCommand, args: `{"command":"` + string(bytes.Repeat([]byte("x"), MaxCommandBytes+1)) + `","timeout_ms":1000}`},
		"timeout too short":   {action: ActionCommand, args: `{"command":"echo ok","timeout_ms":999}`},
		"timeout too long":    {action: ActionCommand, args: `{"command":"echo ok","timeout_ms":300001}`},
		"unknown field":       {action: ActionCommand, args: `{"command":"echo ok","timeout_ms":1000,"shell":"/bin/sh"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTaskActionArgs(value.action, json.RawMessage(value.args)); err == nil {
				t.Fatalf("invalid %s args accepted: %s", name, value.args)
			}
		})
	}
}

func TestValidateProtectedActionResultsRequireGrantFenceAndBounds(t *testing.T) {
	ok := true
	jpegData := taskTestJPEG(t)
	valid := map[string]Frame{
		ActionScreenshot: {Type: "result", ID: "call-1", OK: &ok, Body: mustJSON(t, map[string]any{
			"mime_type": "image/jpeg", "width": 1, "height": 1, "data_base64": jpegData, "target_grant_version": 2,
		})},
		ActionFiles:   {Type: "result", ID: "call-2", OK: &ok, Body: json.RawMessage(`{"entries":[{"path":"workspace/notes.txt","kind":"file","size_bytes":5}],"target_grant_version":2}`)},
		ActionFileGet: {Type: "result", ID: "call-3", OK: &ok, Body: json.RawMessage(`{"path":"workspace/notes.txt","text":"hello","truncated":false,"target_grant_version":2}`)},
		ActionCommand: {Type: "result", ID: "call-4", OK: &ok, Body: json.RawMessage(`{"exit_code":0,"stdout":"ok","stderr":"","timed_out":false,"stopped":true,"target_grant_version":2}`)},
	}
	for action, frame := range valid {
		t.Run(action, func(t *testing.T) {
			if err := ValidateActionResult(action, frame); err != nil {
				t.Fatalf("valid %s result: %v", action, err)
			}
		})
	}

	bad := map[string]struct {
		action string
		body   string
	}{
		"screenshot missing version": {action: ActionScreenshot, body: `{"mime_type":"image/jpeg","width":1,"height":1,"data_base64":"` + jpegData + `"}`},
		"screenshot bad base64":      {action: ActionScreenshot, body: `{"mime_type":"image/jpeg","width":1,"height":1,"data_base64":"not-base64","target_grant_version":2}`},
		"files missing entries":      {action: ActionFiles, body: `{"target_grant_version":2}`},
		"files bad path":             {action: ActionFiles, body: `{"entries":[{"path":"../secret","kind":"file","size_bytes":1}],"target_grant_version":2}`},
		"files duplicate path":       {action: ActionFiles, body: `{"entries":[{"path":"notes.txt","kind":"file","size_bytes":1},{"path":"notes.txt","kind":"file","size_bytes":1}],"target_grant_version":2}`},
		"file.get missing truncated": {action: ActionFileGet, body: `{"path":"notes.txt","text":"hello","target_grant_version":2}`},
		"file.get too large":         {action: ActionFileGet, body: `{"path":"notes.txt","text":"` + string(bytes.Repeat([]byte("x"), MaxFileReadBytes+1)) + `","truncated":false,"target_grant_version":2}`},
		"command missing fields":     {action: ActionCommand, body: `{"exit_code":0,"stdout":"","stderr":"","timed_out":false,"target_grant_version":2}`},
		"command output too large":   {action: ActionCommand, body: `{"exit_code":0,"stdout":"` + string(bytes.Repeat([]byte("x"), MaxCommandOutput+1)) + `","stderr":"","timed_out":false,"stopped":true,"target_grant_version":2}`},
	}
	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			if err := ValidateActionResult(value.action, Frame{Type: "result", ID: "call-5", OK: &ok, Body: json.RawMessage(value.body)}); err == nil {
				t.Fatal("invalid protected action result was accepted")
			}
		})
	}
}

func TestValidateCommandResultReportsUnknownUntilStopIsConfirmed(t *testing.T) {
	ok := true
	err := ValidateActionResult(ActionCommand, Frame{Type: "result", ID: "call-6", OK: &ok,
		Body: json.RawMessage(`{"exit_code":-1,"stdout":"","stderr":"","timed_out":true,"stopped":false,"target_grant_version":2}`)})
	var protocolError ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Code != "unknown_result" {
		t.Fatalf("unconfirmed command stop error = %#v, want unknown_result", err)
	}
}

func taskTestJPEG(t *testing.T) string {
	t.Helper()
	var buffer bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	if err := jpeg.Encode(&buffer, img, nil); err != nil {
		t.Fatal(err)
	}
	return base64Encoded(buffer.Bytes())
}

func base64Encoded(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
