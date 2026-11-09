package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

func cloudDSHMCPTestPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.RGBA{B: 255, A: 255})
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestCloudDSHMCPImageOutputIsDurableAndSeparatedFromModelText(t *testing.T) {
	imageData := cloudDSHMCPTestPNG(t)
	stored, err := encodeCloudDSHMCPOutput(MCPConnectorToolResult{
		Text:   []string{"A chart is attached."},
		Images: []ModelImage{{MediaType: "image/png", Data: imageData}},
	})
	if err != nil {
		t.Fatalf("encode MCP image result: %v", err)
	}
	if !strings.Contains(stored, "data_base64") || !strings.Contains(stored, base64.StdEncoding.EncodeToString(imageData)) {
		t.Fatalf("durable MCP result omitted the image bytes: %s", stored)
	}

	result, err := cloudDSHMCPResult(task.OperationSucceeded, stored)
	if err != nil || result.Status != AgentToolSucceeded || len(result.Images) != 1 ||
		result.Images[0].MediaType != "image/png" || !bytes.Equal(result.Images[0].Data, imageData) {
		t.Fatalf("reconstructed MCP result = %#v, err=%v", result, err)
	}
	if strings.Contains(result.Output, "data_base64") || strings.Contains(result.Output, base64.StdEncoding.EncodeToString(imageData)) ||
		!strings.Contains(result.Output, "verified image for visual analysis") {
		t.Fatalf("model-facing MCP output leaked image data or omitted its marker: %s", result.Output)
	}
	var modelOutput cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(result.Output), &modelOutput); err != nil || len(modelOutput.Images) != 0 {
		t.Fatalf("model-facing result contains stored images: %#v err=%v", modelOutput, err)
	}
}

func TestCloudDSHMCPImageOutputBoundsAndRejectsInvalidData(t *testing.T) {
	imageData := cloudDSHMCPTestPNG(t)
	if _, err := encodeCloudDSHMCPOutput(MCPConnectorToolResult{Images: []ModelImage{{MediaType: "image/jpeg", Data: imageData}}}); err == nil {
		t.Fatal("MCP image with mismatched MIME type was accepted")
	}
	if _, err := encodeCloudDSHMCPOutput(MCPConnectorToolResult{IsError: true, Images: []ModelImage{{MediaType: "image/png", Data: imageData}}}); err == nil {
		t.Fatal("MCP error result with an image was accepted")
	}
	if _, err := encodeCloudDSHMCPOutput(MCPConnectorToolResult{Images: []ModelImage{{MediaType: "image/png", Data: bytes.Repeat([]byte("x"), maxCloudDSHMCPImageBytes+1)}}}); err == nil {
		t.Fatal("oversized MCP image was accepted")
	}

	stored, err := encodeCloudDSHMCPOutput(MCPConnectorToolResult{
		Text:   []string{strings.Repeat("page text ", 300)},
		Images: []ModelImage{{MediaType: "image/png", Data: imageData}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := boundCloudDSHMCPContextOutput(stored, 1024)
	if err != nil || len(bounded) > 1024 {
		t.Fatalf("bound MCP context output: bytes=%d err=%v", len(bounded), err)
	}
	var truncated cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(bounded), &truncated); err != nil || len(truncated.Images) != 1 ||
		!strings.Contains(strings.Join(truncated.Text, "\n"), "page content truncated") {
		t.Fatalf("bounded MCP output lost image or truncation marker: %#v err=%v", truncated, err)
	}
	if _, err := cloudDSHMCPResult(task.OperationSucceeded, bounded); err != nil {
		t.Fatalf("bounded MCP image result could not be reconstructed: %v", err)
	}

	var invalid cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(stored), &invalid); err != nil {
		t.Fatal(err)
	}
	invalid.Images[0].DataBase64 += "!"
	corrupt, _ := json.Marshal(invalid)
	if _, err := cloudDSHMCPResult(task.OperationSucceeded, string(corrupt)); err == nil {
		t.Fatal("corrupt persisted MCP image was accepted")
	}
	invalid.Images = append(invalid.Images, invalid.Images[0])
	duplicate, _ := json.Marshal(invalid)
	if _, err := cloudDSHMCPResult(task.OperationSucceeded, string(duplicate)); err == nil {
		t.Fatal("duplicate persisted MCP images were accepted")
	}
}
