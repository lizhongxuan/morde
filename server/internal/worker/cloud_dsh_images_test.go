package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"morde/server/internal/app"
)

func cloudDSHTestJPEG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 3, 2))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&encoded, pixels, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func cloudDSHTestJPEGVariant(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 3, 2))
	pixels.Set(0, 0, color.RGBA{B: 255, A: 255})
	if err := jpeg.Encode(&encoded, pixels, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestCloudDSHImageRegistryBindsJPEGBytesToTaskFence(t *testing.T) {
	data := cloudDSHTestJPEG(t)
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 2,
		AgentInstanceID: "agent-1", AgentGeneration: 3, AgentSessionID: "session-1", AuthorizationVersion: 4,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	registry := newCloudDSHImageRegistry(fence)
	result, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: data}})
	if err != nil || len(result) != 1 || result[0].Bytes != len(data) || result[0].Width != 3 || result[0].Height != 2 ||
		result[0].AttachmentID == "" || result[0].DataBase64 != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("registered screenshot = %#v, err=%v", result, err)
	}
	attachment, err := json.Marshal(cloudDSHImageAttachment{AttachmentID: result[0].AttachmentID, MediaType: result[0].MediaType,
		Bytes: result[0].Bytes, Width: result[0].Width, Height: result[0].Height, DataBase64: result[0].DataBase64})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.ResolveTaskModelImage(context.Background(), fence, attachment)
	if err != nil || resolved.MediaType != "image/jpeg" || !bytes.Equal(resolved.Data, data) {
		t.Fatalf("authorized image resolve = %#v, err=%v", resolved, err)
	}
	changedFence := fence
	changedFence.AuthorizationVersion++
	if _, err := registry.ResolveTaskModelImage(context.Background(), changedFence, attachment); err == nil {
		t.Fatal("image crossed an authorization-version change")
	}
	changedAttachment := cloudDSHImageAttachment{AttachmentID: result[0].AttachmentID, MediaType: result[0].MediaType,
		Bytes: result[0].Bytes, Width: result[0].Width, Height: result[0].Height,
		DataBase64: base64.StdEncoding.EncodeToString([]byte("changed"))}
	changed, _ := json.Marshal(changedAttachment)
	for _, invalid := range [][]byte{[]byte(`{"attachmentId":"bad"}`), changed} {
		if _, err := registry.ResolveTaskModelImage(context.Background(), fence, invalid); err == nil {
			t.Fatalf("invalid image attachment was accepted: %s", invalid)
		}
	}
	if _, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: bytes.Repeat([]byte("x"), cloudDSHMaxImageBytes+1)}}); err == nil {
		t.Fatal("oversized image was accepted")
	}
	if _, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: []byte("not jpeg")}}); err == nil {
		t.Fatal("malformed JPEG was accepted")
	}
	full := newCloudDSHImageRegistry(fence)
	full.bytes = cloudDSHMaxImageCacheBytes
	if _, err := full.add([]app.ModelImage{{MediaType: "image/jpeg", Data: data}}); err == nil {
		t.Fatal("image cache limit was not enforced")
	}
}

func TestCloudDSHModelRequestAcceptsOnlyBoundedToolImages(t *testing.T) {
	data := cloudDSHTestJPEG(t)
	registry := newCloudDSHImageRegistry(app.ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567"})
	images, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	attachment := cloudDSHImageAttachment{AttachmentID: images[0].AttachmentID, MediaType: images[0].MediaType,
		Bytes: images[0].Bytes, Width: images[0].Width, Height: images[0].Height, DataBase64: images[0].DataBase64}
	request := map[string]any{"provider": "morde", "model": "p1234567", "tools": []any{}, "messages": []any{
		map[string]any{"role": "tool", "toolCallId": "call_1", "content": []any{
			map[string]any{"type": "image", "attachment": attachment},
		}},
	}}
	encoded, err := json.Marshal(request)
	if err != nil || !validCloudDSHModelRequest(encoded, "p1234567", nil) {
		t.Fatalf("valid tool image request rejected: %s, err=%v", encoded, err)
	}
	request["messages"] = []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image", "attachment": attachment},
	}}}
	encoded, _ = json.Marshal(request)
	if validCloudDSHModelRequest(encoded, "p1234567", nil) {
		t.Fatal("untrusted user image bypassed tool-result provenance")
	}
	tooMany := make([]any, cloudDSHMaxModelImageCount+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"role": "tool", "toolCallId": "call_1", "content": []any{
			map[string]any{"type": "image", "attachment": attachment},
		}}
	}
	request["messages"] = tooMany
	encoded, _ = json.Marshal(request)
	if validCloudDSHModelRequest(encoded, "p1234567", nil) {
		t.Fatal("model request exceeded image count limit")
	}
}

func TestCloudDSHCheckpointReconcilesExactScreenshotReference(t *testing.T) {
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567"}
	registry := newCloudDSHImageRegistry(fence)
	want, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: cloudDSHTestJPEG(t)}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := registry.add([]app.ModelImage{{MediaType: "image/jpeg", Data: cloudDSHTestJPEGVariant(t)}})
	if err != nil {
		t.Fatal(err)
	}
	ref := cloudDSHImageReferences(want)[0]
	callID := "call_screenshot_1"
	toolResult := cloudDSHCheckpointEvent{Sequence: 4, Type: "tool/result", Data: map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"toolCallId": callID, "isError": false, "source": map[string]any{"kind": "tool", "callId": callID},
			"content": []any{map[string]any{"type": "text", "text": `{"mime_type":"image/jpeg"}`},
				map[string]any{"type": "image", "attachment": ref}}},
	}, SurfaceOp: "append", SourceEventSeqs: []int64{3}}
	history := newCloudDSHCheckpointHistory("session_1")
	history.beginRestore()
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		cloudDSHCheckpointEvent{Sequence: 2, Type: "assistant/message", Data: map[string]any{"turn": 1, "step": 1,
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool-call", "id": callID,
				"name": "desktop_screenshot", "arguments": `{}`}}}}},
		cloudDSHCheckpointEvent{Sequence: 3, Type: "tool/call", Data: map[string]any{"turn": 1, "step": 1,
			"callId": callID, "name": "desktop_screenshot", "arguments": `{}`}}, toolResult,
	), 0, 4); err != nil {
		t.Fatal(err)
	}
	history.finishRestore()
	if err := history.noteToolResult(callID, "desktop_screenshot", []byte(`{}`), app.AgentToolSucceeded,
		`{"mime_type":"image/jpeg"}`, other); err == nil {
		t.Fatal("recovered Host screenshot with a different reference was accepted")
	}
}
