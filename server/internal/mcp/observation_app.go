package mcp

import (
	"context"
	"embed"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	observationResourceURI    = "ui://morde/desktop-observation"
	observationResourceMIME   = "text/html;profile=mcp-app"
	observationMetaKey        = "io.morde/observation"
	desktopScreenshotToolName = "desktop_screenshot"
)

//go:embed observation_app.html
var observationApp embed.FS

func addObservationResource(server *sdk.Server) error {
	html, err := observationApp.ReadFile("observation_app.html")
	if err != nil {
		return fmt.Errorf("read MCP observation app: %w", err)
	}
	resourceMeta := sdk.Meta{"ui": map[string]any{"prefersBorder": true}}
	server.AddResource(&sdk.Resource{
		URI: observationResourceURI, Name: "morde-desktop-observation",
		Description: "Read-only snapshot of the authorized Host desktop.", MIMEType: observationResourceMIME,
		Meta: resourceMeta,
	}, func(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI: observationResourceURI, MIMEType: observationResourceMIME, Text: string(html),
			Meta: sdk.Meta{"ui": map[string]any{"prefersBorder": true}},
		}}}, nil
	})
	return nil
}
