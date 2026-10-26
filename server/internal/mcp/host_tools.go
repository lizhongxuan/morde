package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"io"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *service) hostTool(ctx context.Context, principal identity.Principal, protocolSessionID string, spec toolSpec, args map[string]any) (any, *sdk.ImageContent, error) {
	if spec.runtime == nil || !app.ValidMCPRuntimeSessionID(protocolSessionID) {
		return nil, nil, domain.NewError(domain.CodeInvalidState, "MCP protocol session is unavailable")
	}
	hostID := stringArg(args, "host_id")
	idempotencyKey := stringArg(args, "idempotency_key")
	if !domain.ValidHostID(hostID) || !app.ValidMCPIdempotencyKey(idempotencyKey) {
		return nil, nil, domain.NewError(domain.CodeInvalidArgument, "MCP runtime operation identifiers are invalid")
	}
	input := make(map[string]any, len(args)-2)
	for name, value := range args {
		if name != "host_id" && name != "idempotency_key" {
			input[name] = value
		}
	}
	arguments, err := json.Marshal(input)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInvalidArgument, "runtime tool arguments are invalid")
	}
	result, err := s.deps.RuntimeTools.Execute(ctx, principal, protocolSessionID, hostID, idempotencyKey, spec.name, arguments)
	if err != nil {
		return nil, nil, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(result.Output))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, domain.NewError(domain.CodeUpstreamError, "Host runtime returned an invalid result")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, nil, domain.NewError(domain.CodeUpstreamError, "Host runtime returned an invalid result")
	}
	if result.IsError {
		return nil, nil, domain.NewError(domain.CodeUpstreamError, "Host runtime operation failed")
	}
	if len(result.Images) > 1 {
		return nil, nil, domain.NewError(domain.CodeUpstreamError, "Host runtime returned too many images")
	}
	if len(result.Images) == 1 {
		image := result.Images[0]
		config, err := jpeg.DecodeConfig(bytes.NewReader(image.Data))
		if image.MediaType != "image/jpeg" || len(image.Data) == 0 || err != nil || config.Width < 1 || config.Width > s.deps.Config.ScreenshotMaxWidth {
			return nil, nil, domain.NewError(domain.CodeUpstreamError, "Host runtime returned an invalid image")
		}
		return value, &sdk.ImageContent{Data: append([]byte(nil), image.Data...), MIMEType: image.MediaType}, nil
	}
	return value, nil, nil
}
