package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func syntheticDerivative(t *testing.T, kind string, page int, name, mediaType string, content []byte) ProjectConversationDerivativeFile {
	t.Helper()
	digest := sha256.Sum256(content)
	return ProjectConversationDerivativeFile{Kind: kind, Page: page, Name: name, MediaType: mediaType,
		SHA256: hex.EncodeToString(digest[:]), Content: content}
}

func TestProjectConversationDerivativeValidation(t *testing.T) {
	var encoded bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 4, 4))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&encoded, pixels, nil); err != nil {
		t.Fatal(err)
	}
	text := syntheticDerivative(t, "extracted_text", 0, "synthetic.txt", "text/plain", []byte("synthetic text"))
	page := syntheticDerivative(t, "page_preview", 1, "page.jpg", "image/jpeg", encoded.Bytes())
	image := syntheticDerivative(t, "image_conversion", 0, "converted.jpg", "image/jpeg", encoded.Bytes())
	pdf := ProjectConversationDerivatives{Outcome: "completed", Files: []ProjectConversationDerivativeFile{text, page}}
	first, _, err := ValidateProjectConversationDerivatives("pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ValidateProjectConversationDerivatives("pdf", pdf)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("replay fingerprint changed: %v", err)
	}
	if _, _, err := ValidateProjectConversationDerivatives("image", ProjectConversationDerivatives{Outcome: "completed", Files: []ProjectConversationDerivativeFile{image}}); err != nil {
		t.Fatalf("valid image conversion rejected: %v", err)
	}
	if _, _, err := ValidateProjectConversationDerivatives("pdf", ProjectConversationDerivatives{Outcome: "skipped", Files: []ProjectConversationDerivativeFile{}}); err != nil {
		t.Fatalf("explicit skip rejected: %v", err)
	}
	emptyText := syntheticDerivative(t, "extracted_text", 0, "blank.txt", "text/plain", []byte{})
	if _, _, err := ValidateProjectConversationDerivatives("pdf", ProjectConversationDerivatives{Outcome: "completed", Files: []ProjectConversationDerivativeFile{emptyText}}); err != nil {
		t.Fatalf("blank PDF text rejected: %v", err)
	}
	changed := text
	changed.Content = []byte("other bytes")
	malformed := syntheticDerivative(t, "page_preview", 1, "page.jpg", "image/jpeg", []byte("not an image"))
	for name, value := range map[string]ProjectConversationDerivatives{
		"hash mismatch":   {Outcome: "completed", Files: []ProjectConversationDerivativeFile{changed}},
		"duplicate page":  {Outcome: "completed", Files: []ProjectConversationDerivativeFile{text, page, page}},
		"missing text":    {Outcome: "completed", Files: []ProjectConversationDerivativeFile{page}},
		"invalid jpeg":    {Outcome: "completed", Files: []ProjectConversationDerivativeFile{text, malformed}},
		"skip with files": {Outcome: "skipped", Files: []ProjectConversationDerivativeFile{text}},
		"missing files":   {Outcome: "skipped"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ValidateProjectConversationDerivatives("pdf", value); err == nil {
				t.Fatal("invalid set accepted")
			}
		})
	}
}
