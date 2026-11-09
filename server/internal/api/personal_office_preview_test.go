package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

const (
	previewDOCXMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	previewXLSXMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	previewPPTXMediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

func TestPersonalOfficePreviewRouteReturnsBoundedPlainText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		mediaType string
		content   []byte
		want      string
	}{
		{
			name:      "DOCX",
			mediaType: previewDOCXMediaType,
			content: personalOfficePreviewZipFixture(t, map[string]string{
				"word/document.xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Preview report</w:t></w:r></w:p></w:body></w:document>`,
			}),
			want: "Preview report",
		},
		{
			name:      "XLSX",
			mediaType: previewXLSXMediaType,
			content: personalOfficePreviewZipFixture(t, map[string]string{
				"xl/workbook.xml":          `<workbook/>`,
				"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Preview metric</t></is></c></row></sheetData></worksheet>`,
			}),
			want: "A1=Preview metric",
		},
		{
			name:      "PPTX",
			mediaType: previewPPTXMediaType,
			content: personalOfficePreviewZipFixture(t, map[string]string{
				"ppt/presentation.xml":            `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="1" r:id="rId1"/></p:sldIdLst></p:presentation>`,
				"ppt/_rels/presentation.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`,
				"ppt/slides/slide1.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Preview decision</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
			}),
			want: "Preview decision",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := personalOfficePreviewTestRouter(true, "session")
			response := requestPersonalOfficePreview(router, test.mediaType, test.content, true)
			var body personalOfficePreviewResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || !strings.Contains(body.Text, test.want) ||
				response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("preview response=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
			}
		})
	}
}

func TestPersonalOfficePreviewRouteRejectsUnauthenticatedUnsupportedMismatchedAndMalformedRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validDOCX := personalOfficePreviewZipFixture(t, map[string]string{
		"word/document.xml": `<w:document><w:body><w:p><w:r><w:t>safe</w:t></w:r></w:p></w:body></w:document>`,
	})
	validXLSX := personalOfficePreviewZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>safe</t></is></c></row></sheetData></worksheet>`,
	})
	tests := []struct {
		name      string
		authed    bool
		authKind  string
		mediaType string
		content   []byte
		wantCode  int
	}{
		{name: "missing principal", authKind: "session", mediaType: previewDOCXMediaType, content: validDOCX, wantCode: http.StatusUnauthorized},
		{name: "unsupported media type", authed: true, authKind: "client", mediaType: "application/zip", content: validDOCX, wantCode: http.StatusBadRequest},
		{name: "declared MIME does not match document", authed: true, authKind: "client", mediaType: previewDOCXMediaType, content: validXLSX, wantCode: http.StatusBadRequest},
		{name: "malformed Office document", authed: true, authKind: "client", mediaType: previewDOCXMediaType, content: []byte("not a ZIP package"), wantCode: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := personalOfficePreviewTestRouter(test.authed, test.authKind)
			response := requestPersonalOfficePreview(router, test.mediaType, test.content, true)
			if response.Code != test.wantCode {
				t.Fatalf("preview response=%d body=%s, want %d", response.Code, response.Body, test.wantCode)
			}
		})
	}
}

func TestPersonalOfficePreviewRouteEnforcesBodyLimitAndSessionOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := personalOfficePreviewTestRouter(true, "session")
	oversized := bytes.Repeat([]byte("x"), app.PersonalOfficePreviewMaxBytes+1)
	response := requestPersonalOfficePreview(router, previewDOCXMediaType, oversized, true)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized preview response=%d body=%s", response.Code, response.Body)
	}

	missingOrigin := requestPersonalOfficePreview(router, previewDOCXMediaType, []byte("synthetic"), false)
	if missingOrigin.Code != http.StatusForbidden {
		t.Fatalf("missing-origin response=%d body=%s", missingOrigin.Code, missingOrigin.Body)
	}
}

func personalOfficePreviewTestRouter(authed bool, authKind string) *gin.Engine {
	router := gin.New()
	handler := &api{options: Options{APIMaxBodyBytes: 1024}}
	group := router.Group("/api", func(c *gin.Context) {
		c.Set("auth_kind", authKind)
		if authed {
			c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"})
		}
		c.Next()
	}, handler.limitBody, handler.checkOrigin)
	RegisterPersonalOfficePreviewRoute(group.Group("/personal-data"))
	return router
}

func requestPersonalOfficePreview(router http.Handler, mediaType string, content []byte, withOrigin bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/personal-data/office-preview", bytes.NewReader(content))
	request.Header.Set("Content-Type", mediaType)
	if withOrigin {
		request.Header.Set("Origin", "http://example.com")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func personalOfficePreviewZipFixture(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
