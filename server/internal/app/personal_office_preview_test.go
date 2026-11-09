package app

import (
	"bytes"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestPreviewPersonalOfficeTextExtractsSupportedFormats(t *testing.T) {
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	tests := []struct {
		name      string
		mediaType string
		content   []byte
		want      string
	}{
		{
			name:      "DOCX",
			mediaType: cloudDSHOfficeDOCXMediaType,
			content: cloudDSHOfficeZipFixture(t, map[string]string{
				"word/document.xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Preview words</w:t></w:r></w:p></w:body></w:document>`,
			}),
			want: "Preview words",
		},
		{
			name:      "XLSX",
			mediaType: cloudDSHOfficeXLSXMediaType,
			content: cloudDSHOfficeZipFixture(t, map[string]string{
				"xl/workbook.xml":          `<workbook/>`,
				"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Preview cell</t></is></c></row></sheetData></worksheet>`,
			}),
			want: "A1=Preview cell",
		},
		{
			name:      "PPTX",
			mediaType: cloudDSHOfficePPTXMediaType,
			content: cloudDSHOfficeZipFixture(t, map[string]string{
				"ppt/presentation.xml":            presentation,
				"ppt/_rels/presentation.xml.rels": presentationRels,
				"ppt/slides/slide1.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Preview slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
			}),
			want: "Preview slide",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := bytes.Clone(test.content)
			got, err := PreviewPersonalOfficeText(test.mediaType, test.content)
			if err != nil || !strings.Contains(got, test.want) {
				t.Fatalf("preview = %q, %v; want text containing %q", got, err, test.want)
			}
			if !bytes.Equal(test.content, before) {
				t.Fatal("preview changed the supplied document bytes")
			}
		})
	}
}

func TestPreviewPersonalOfficeTextPreservesPPTXPresentationOrder(t *testing.T) {
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{
		"ppt/slides/slide2.xml", "ppt/slides/slide1.xml",
	})
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":            presentation,
		"ppt/_rels/presentation.xml.rels": presentationRels,
		"ppt/slides/slide1.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Second presentation slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide2.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>First presentation slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})
	got, err := PreviewPersonalOfficeText(cloudDSHOfficePPTXMediaType, content)
	first, second := strings.Index(got, "First presentation slide"), strings.Index(got, "Second presentation slide")
	if err != nil || first < 0 || second < 0 || first >= second {
		t.Fatalf("preview = %q, %v; want slides in relationship order", got, err)
	}
}

func TestPreviewPersonalOfficeTextRejectsUnsupportedMIMEAndMalformedDocuments(t *testing.T) {
	validDOCX := cloudDSHOfficeZipFixture(t, map[string]string{
		"word/document.xml": `<w:document><w:body><w:p><w:r><w:t>safe</w:t></w:r></w:p></w:body></w:document>`,
	})
	validXLSX := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>safe</t></is></c></row></sheetData></worksheet>`,
	})
	tests := []struct {
		name      string
		mediaType string
		content   []byte
	}{
		{name: "unsupported MIME", mediaType: "application/zip", content: validDOCX},
		{name: "MIME does not match OOXML package", mediaType: cloudDSHOfficeDOCXMediaType, content: validXLSX},
		{name: "bad ZIP", mediaType: cloudDSHOfficeDOCXMediaType, content: []byte("not a ZIP")},
		{name: "empty", mediaType: cloudDSHOfficeDOCXMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PreviewPersonalOfficeText(test.mediaType, test.content); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("preview error = %v; want invalid_argument", err)
			}
		})
	}
}

func TestPreviewPersonalOfficeTextRejectsOversizedDocument(t *testing.T) {
	content := bytes.Repeat([]byte("x"), maxCloudDSHOfficeAttachmentBytes+1)
	if _, err := PreviewPersonalOfficeText(cloudDSHOfficeDOCXMediaType, content); domain.CodeOf(err) != domain.CodeRequestTooLarge {
		t.Fatalf("preview error = %v; want request_too_large", err)
	}
}
