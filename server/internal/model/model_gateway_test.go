package model

import (
	"encoding/json"
	"testing"

	"morde/server/internal/app"
)

func TestResponsesRequestPreservesInterleavedTextAndImages(t *testing.T) {
	image := app.ModelImage{MediaType: "image/png", Data: []byte("image")}
	request := app.ModelRequest{Endpoint: app.ModelEndpoint{ModelID: "test-model"}, MaxOutputTokens: 10,
		Input: []app.ModelInput{{Type: "message", Role: "user", Parts: []app.ModelContentPart{
			{Type: "text", Text: "before"}, {Type: "image", Image: &image}, {Type: "text", Text: "after"},
		}}}}
	raw, err := encodeRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	parts := payload.Input[0].Content
	if len(parts) != 3 || parts[0].Text != "before" || parts[1].Type != "input_image" || parts[2].Text != "after" {
		t.Fatalf("message content order changed: %#v", parts)
	}
}
