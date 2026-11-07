package app

import (
	"archive/zip"
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestExtractCloudDSHOfficeTextDOCX(t *testing.T) {
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello </w:t></w:r><w:r><w:t>世界</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p></w:body></w:document>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficeDOCXMediaType, content)
	if err != nil || got != "Hello 世界\nSecond paragraph" {
		t.Fatalf("extract DOCX = %q, %v", got, err)
	}
}

func TestExtractCloudDSHOfficeTextXLSX(t *testing.T) {
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"></workbook>`,
		"xl/sharedStrings.xml":     `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Revenue</t></si><si><r><t>年度</t></r><r><t>汇总</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="inlineStr"><is><t>2026</t></is></c></row><row r="2"><c r="B2" t="s"><v>1</v></c><c r="C2" t="b"><v>1</v></c></row></sheetData></worksheet>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficeXLSXMediaType, content)
	want := "Sheet sheet1.xml:\nA1=Revenue | B1=2026\nB2=年度汇总 | C2=TRUE"
	if err != nil || got != want {
		t.Fatalf("extract XLSX = %q, %v; want %q", got, err, want)
	}
}

func TestExtractCloudDSHOfficeTextXLSXBoundsRepeatedSharedStringExpansion(t *testing.T) {
	shared := strings.Repeat("x", 4<<20)
	var sheet strings.Builder
	sheet.WriteString(`<worksheet><sheetData><row>`)
	for index := 1; index <= 100; index++ {
		sheet.WriteString(`<c r="A`)
		sheet.WriteString(strconv.Itoa(index))
		sheet.WriteString(`" t="s"><v>0</v></c>`)
	}
	sheet.WriteString(`</row></sheetData></worksheet>`)
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/sharedStrings.xml":     `<sst><si><t>` + shared + `</t></si></sst>`,
		"xl/worksheets/sheet1.xml": sheet.String(),
	})
	if len(content) > maxCloudDSHOfficeAttachmentBytes {
		t.Fatalf("fixture compressed size = %d, want <= %d", len(content), maxCloudDSHOfficeAttachmentBytes)
	}
	got, err := extractCloudDSHOfficeText(cloudDSHOfficeXLSXMediaType, content)
	if err != nil || len(got) > maxCloudDSHOfficeExtractedTextSize+128 || !strings.Contains(got, "[Text extraction is partial") {
		t.Fatalf("repeated shared string extraction length=%d err=%v, want bounded partial text", len(got), err)
	}
}

func TestExtractCloudDSHOfficeTextXLSXAllowsLastValidCellAndBoundsCellCount(t *testing.T) {
	lastCell := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="XFD1048576"><v>ok</v></c></row></sheetData></worksheet>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficeXLSXMediaType, lastCell)
	if err != nil || !strings.Contains(got, "XFD1048576=ok") {
		t.Fatalf("last valid cell extraction = %q, %v", got, err)
	}

	var sheet strings.Builder
	sheet.WriteString(`<worksheet><sheetData><row>`)
	for range maxCloudDSHOfficeWorkbookCells + 1 {
		sheet.WriteString(`<c r="A1"><v>1</v></c>`)
	}
	sheet.WriteString(`</row></sheetData></worksheet>`)
	tooManyCells := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/worksheets/sheet1.xml": sheet.String(),
	})
	if _, err := extractCloudDSHOfficeText(cloudDSHOfficeXLSXMediaType, tooManyCells); err == nil {
		t.Fatal("worksheet exceeding the cell-count budget was accepted")
	}
}

func TestExtractCloudDSHOfficeTextRejectsMalformedAndUnsafePackages(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		content   []byte
	}{
		{name: "bad zip", mediaType: cloudDSHOfficeDOCXMediaType, content: []byte("not a zip")},
		{name: "wrong media type", mediaType: "application/zip", content: cloudDSHOfficeZipFixture(t, map[string]string{"word/document.xml": `<w:p><w:t>hidden</w:t></w:p>`})},
		{name: "malformed xml", mediaType: cloudDSHOfficeDOCXMediaType, content: cloudDSHOfficeZipFixture(t, map[string]string{"word/document.xml": `<w:p><w:t>broken</w:p>`})},
		{name: "xml directive", mediaType: cloudDSHOfficeDOCXMediaType, content: cloudDSHOfficeZipFixture(t, map[string]string{"word/document.xml": `<!DOCTYPE x><w:p><w:t>text</w:t></w:p>`})},
		{name: "missing xlsx workbook", mediaType: cloudDSHOfficeXLSXMediaType, content: cloudDSHOfficeZipFixture(t, map[string]string{"xl/worksheets/sheet1.xml": `<worksheet/>`})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := extractCloudDSHOfficeText(test.mediaType, test.content); err == nil {
				t.Fatal("unsafe or malformed Office document was accepted")
			}
		})
	}

	if _, err := extractCloudDSHOfficeText(cloudDSHOfficeDOCXMediaType, bytes.Repeat([]byte("x"), maxCloudDSHOfficeAttachmentBytes+1)); err == nil {
		t.Fatal("oversized compressed input was accepted")
	}
}

func TestExtractCloudDSHOfficeTextRejectsTraversalDuplicateAndBadCells(t *testing.T) {
	traversal := cloudDSHOfficeZipFixture(t, map[string]string{
		"../word/document.xml": `<w:document><w:p><w:t>text</w:t></w:p></w:document>`,
	})
	if _, err := extractCloudDSHOfficeText(cloudDSHOfficeDOCXMediaType, traversal); err == nil {
		t.Fatal("traversal package entry was accepted")
	}

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for range 2 {
		entry, err := writer.Create("word/document.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(`<w:document><w:p><w:t>text</w:t></w:p></w:document>`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractCloudDSHOfficeText(cloudDSHOfficeDOCXMediaType, output.Bytes()); err == nil {
		t.Fatal("duplicate package entry was accepted")
	}

	badCell := cloudDSHOfficeZipFixture(t, map[string]string{
		"xl/workbook.xml":          `<workbook/>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="XFE1"><v>1</v></c></row></sheetData></worksheet>`,
	})
	if _, err := extractCloudDSHOfficeText(cloudDSHOfficeXLSXMediaType, badCell); err == nil {
		t.Fatal("out-of-range XLSX cell reference was accepted")
	}
}

func TestExtractCloudDSHOfficeTextBoundsOutputAndListsSupportedTypes(t *testing.T) {
	text := strings.Repeat("x", maxCloudDSHOfficeExtractedTextSize+4096)
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"word/document.xml": `<w:document><w:p><w:t>` + text + `</w:t></w:p></w:document>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficeDOCXMediaType, content)
	if err != nil || len(got) > maxCloudDSHOfficeExtractedTextSize+128 || !strings.Contains(got, "[Text extraction is partial") {
		t.Fatalf("bounded extraction length=%d err=%v, want explicit partial result", len(got), err)
	}
	if !validCloudDSHTaskAttachmentMediaType(cloudDSHOfficeDOCXMediaType) ||
		!validCloudDSHTaskAttachmentMediaType(cloudDSHOfficeXLSXMediaType) ||
		validCloudDSHTaskAttachmentMediaType("application/zip") {
		t.Fatal("Office attachment media type allowlist is incorrect")
	}
}

func cloudDSHOfficeZipFixture(t *testing.T, entries map[string]string) []byte {
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
