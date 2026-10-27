package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *ModelGatewayService) CheckModel(ctx context.Context, principal identity.Principal, modelID, key string) (ModelCapabilities, error) {
	return s.CheckModelCapability(ctx, principal, modelID, key, "all")
}

func (s *ModelGatewayService) CheckModelCapability(ctx context.Context, principal identity.Principal, modelID, key, capability string) (ModelCapabilities, error) {
	if !idempotencyKeyPattern.MatchString(key) {
		return ModelCapabilities{}, domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	if capability != "all" && capability != "text" && capability != "tools" && capability != "vision" {
		return ModelCapabilities{}, domain.InvalidArgument("capability", "invalid model capability")
	}
	endpoint, err := s.models.ResolveProbeEndpoint(ctx, principal, modelID)
	if err != nil {
		return ModelCapabilities{}, err
	}
	const probeTool = "morde_capability_probe"
	request := ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "Reply exactly OK."}}, MaxOutputTokens: 64}
	if capability == "tools" || capability == "all" {
		request.Instructions = "Call the provided function with color set to red."
		request.Tools = []ModelTool{{Name: probeTool, Description: "Report the requested color.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"color":{"type":"string","enum":["red","blue","green"]}},"required":["color"],"additionalProperties":false}`), Strict: true}}
		request.RequiredTool = probeTool
	}
	if capability == "vision" || capability == "all" {
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			}
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			return ModelCapabilities{}, err
		}
		request.Input[0].Text = "Identify the square's color. Reply with the color only."
		request.Input[0].Images = []ModelImage{{MediaType: "image/png", Data: encoded.Bytes()}}
		if capability == "all" {
			request.Instructions = "Inspect the supplied image. Describe its color in text, then call the function with the color visible in the image."
		}
	}
	digest := sha256.Sum256([]byte("morde-model-probe-v1\n" + principal.UserID + "\n" + key))
	response, err := s.Complete(ctx, ModelCall{
		Source: ModelCallProbe, Principal: principal, ModelID: modelID, CallID: "probe_" + hex.EncodeToString(digest[:]),
		Request: request,
	})
	if err != nil {
		return ModelCapabilities{}, err
	}
	if !response.Usage.Known {
		return ModelCapabilities{}, domain.NewError(domain.CodeInvalidState, "model probe usage is unknown and requires reconciliation")
	}
	if capability == "tools" || capability == "all" {
		if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != probeTool {
			return ModelCapabilities{}, domain.NewError(domain.CodeUpstreamError, "model capability probe failed")
		}
		var args struct {
			Color string `json:"color"`
		}
		if err := json.Unmarshal([]byte(response.ToolCalls[0].Arguments), &args); err != nil || args.Color != "red" {
			return ModelCapabilities{}, domain.NewError(domain.CodeUpstreamError, "model capability probe failed")
		}
		if capability == "all" && strings.TrimSpace(response.Text) == "" {
			return ModelCapabilities{}, domain.NewError(domain.CodeUpstreamError, "model text capability was not verified")
		}
	} else {
		want := "OK"
		if capability == "vision" {
			want = "RED"
		}
		if len(response.ToolCalls) != 0 || !strings.EqualFold(strings.TrimSpace(response.Text), want) {
			return ModelCapabilities{}, domain.NewError(domain.CodeUpstreamError, "model capability probe failed")
		}
	}
	capabilities := ModelCapabilities{Text: capability != "tools", Tools: capability == "tools" || capability == "all", Vision: capability == "vision" || capability == "all", ModelID: modelID}
	return s.models.SaveProbeCapabilities(ctx, principal, modelID, endpoint.Version, capabilities)
}
