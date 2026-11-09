package app

import (
	"archive/zip"
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
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

func TestExtractCloudDSHOfficeTextPPTX(t *testing.T) {
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{
		"ppt/slides/slide2.xml", "ppt/slides/slide1.xml", "ppt/slides/slide3.xml",
	})
	content := cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":            presentation,
		"ppt/_rels/presentation.xml.rels": presentationRels,
		"ppt/slides/slide2.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>第二张</a:t></a:r><a:r><a:t> 内容</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide1.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>标题</a:t></a:r></a:p><a:p><a:r><a:t>副标题</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide3.xml":           `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`,
		"ppt/notesSlides/notesSlide1.xml": `<p:notes xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:sp><p:txBody><a:p><a:r><a:t>notes are ignored</a:t></a:r></a:p></p:txBody></p:sp></p:notes>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, content)
	want := "Slide 1:\n第二张 内容\n\nSlide 2:\n标题\n副标题"
	if err != nil || got != want {
		t.Fatalf("extract PPTX = %q, %v; want %q", got, err, want)
	}
}

func TestExtractCloudDSHOfficeTextPPTXChartCachedValues(t *testing.T) {
	chart := `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart><c:plotArea><c:barChart><c:ser><c:tx><c:strRef><c:strCache><c:ptCount val="1"/><c:pt idx="0"><c:v>Revenue</c:v></c:pt></c:strCache></c:strRef></c:tx><c:cat><c:strRef><c:strCache><c:ptCount val="2"/><c:pt idx="1"><c:v>Q2</c:v></c:pt><c:pt idx="0"><c:v>Q1</c:v></c:pt></c:strCache></c:strRef></c:cat><c:val><c:numRef><c:numCache><c:ptCount val="2"/><c:pt idx="1"><c:v>140</c:v></c:pt><c:pt idx="0"><c:v>120</c:v></c:pt></c:numCache></c:numRef></c:val></c:ser></c:barChart></c:plotArea></c:chart></c:chartSpace>`
	relationships := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdChart" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart" Target="../charts/chart1.xml"/></Relationships>`
	content := cloudDSHPPTXChartFixture(t,
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><c:chart r:id="rIdChart"/></p:spTree></p:cSld></p:sld>`, relationships, chart)
	got, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, content)
	want := "Slide 1 chart 1:\nSeries \"Revenue\": \"Q1\"=\"120\"; \"Q2\"=\"140\""
	if err != nil || got != want {
		t.Fatalf("extract PPTX chart = %q, %v; want %q", got, err, want)
	}
}

func TestExtractCloudDSHOfficeTextPPTXChartWithoutCacheAndBounds(t *testing.T) {
	chart := `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart><c:plotArea><c:barChart><c:ser><c:val><c:numRef><c:f>Sheet1!$B$2:$B$3</c:f></c:numRef></c:val></c:ser></c:barChart></c:plotArea></c:chart></c:chartSpace>`
	got, truncated, err := extractCloudDSHPPTXChartSummary([]byte(chart), 256)
	if err != nil || truncated || got != "Series \"Series 1\": [No cached values.]" {
		t.Fatalf("chart without cache = %q, truncated=%v, err=%v", got, truncated, err)
	}

	large := strings.Builder{}
	large.WriteString(`<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart><c:plotArea><c:barChart><c:ser><c:val><c:numRef><c:numCache><c:ptCount val="2"/>`)
	for index := 0; index < 2; index++ {
		large.WriteString(`<c:pt idx="` + strconv.Itoa(index) + `"><c:v>` + strings.Repeat("7", 200) + `</c:v></c:pt>`)
	}
	large.WriteString(`</c:numCache></c:numRef></c:val></c:ser></c:barChart></c:plotArea></c:chart></c:chartSpace>`)
	got, truncated, err = extractCloudDSHPPTXChartSummary([]byte(large.String()), 64)
	if err != nil || !truncated || len(got) > 64 {
		t.Fatalf("bounded chart summary length=%d truncated=%v err=%v", len(got), truncated, err)
	}
}

func TestExtractCloudDSHOfficeTextPPTXRejectsUnsafeChartReferences(t *testing.T) {
	validChart := `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart/></c:chartSpace>`
	slide := `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><c:chart r:id="rIdChart"/></p:spTree></p:cSld></p:sld>`
	tests := []struct {
		name  string
		rels  string
		chart string
	}{
		{
			name:  "missing chart relationship",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
			chart: validChart,
		},
		{
			name:  "wrong relationship type",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdChart" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../charts/chart1.xml"/></Relationships>`,
			chart: validChart,
		},
		{
			name:  "chart target outside chart directory",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdChart" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart" Target="../media/chart1.xml"/></Relationships>`,
			chart: validChart,
		},
		{
			name:  "external relationship",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdChart" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart" Target="https://example.invalid/chart.xml" TargetMode="External"/></Relationships>`,
			chart: validChart,
		},
		{
			name:  "malformed chart XML",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdChart" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart" Target="../charts/chart1.xml"/></Relationships>`,
			chart: `<c:chartSpace><c:chart>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := cloudDSHPPTXChartFixture(t, slide, test.rels, test.chart)
			if _, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, content); err == nil {
				t.Fatal("unsafe chart reference was accepted")
			}
		})
	}

	tooManyPoints := `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart><c:plotArea><c:barChart><c:ser><c:val><c:numRef><c:numCache><c:ptCount val="129"/></c:numCache></c:numRef></c:val></c:ser></c:barChart></c:plotArea></c:chart></c:chartSpace>`
	if _, _, err := extractCloudDSHPPTXChartSummary([]byte(tooManyPoints), 256); err == nil {
		t.Fatal("chart cache exceeding the point limit was accepted")
	}
}

func TestExtractCloudDSHOfficeTextPPTXBoundsAndRejectsUnsafeSlides(t *testing.T) {
	largeText := strings.Repeat("演示", maxCloudDSHOfficeExtractedTextSize)
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	bounded := cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":            presentation,
		"ppt/_rels/presentation.xml.rels": presentationRels,
		"ppt/slides/slide1.xml":           `<p:sld><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` + largeText + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})
	got, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, bounded)
	if err != nil || len(got) > maxCloudDSHOfficeExtractedTextSize+128 || !strings.Contains(got, "[Text extraction is partial") {
		t.Fatalf("bounded PPTX extraction length=%d err=%v, want bounded partial text", len(got), err)
	}

	missingSlidesPresentation, missingSlidesRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	malformedPresentation, malformedRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	tests := []struct {
		name    string
		entries map[string]string
	}{
		{name: "missing presentation", entries: map[string]string{"ppt/slides/slide1.xml": `<p:sld/>`}},
		{name: "missing slides", entries: map[string]string{
			"ppt/presentation.xml": missingSlidesPresentation, "ppt/_rels/presentation.xml.rels": missingSlidesRels,
		}},
		{name: "malformed slide", entries: map[string]string{
			"ppt/presentation.xml": malformedPresentation, "ppt/_rels/presentation.xml.rels": malformedRels,
			"ppt/slides/slide1.xml": `<p:sld><a:p>`,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, cloudDSHOfficeZipFixture(t, test.entries)); err == nil {
				t.Fatal("invalid presentation was accepted")
			}
		})
	}
	tooManySlideNames := make([]string, 0, maxCloudDSHOfficeSlides+1)
	tooManySlides := make(map[string]string)
	for index := 1; index <= maxCloudDSHOfficeSlides+1; index++ {
		name := "ppt/slides/slide" + strconv.Itoa(index) + ".xml"
		tooManySlideNames = append(tooManySlideNames, name)
		tooManySlides[name] = `<p:sld/>`
	}
	tooManyPresentation, tooManyPresentationRels := cloudDSHPPTXPresentationParts(tooManySlideNames)
	tooManySlides["ppt/presentation.xml"] = tooManyPresentation
	tooManySlides["ppt/_rels/presentation.xml.rels"] = tooManyPresentationRels
	if _, err := extractCloudDSHOfficeText(cloudDSHOfficePPTXMediaType, cloudDSHOfficeZipFixture(t, tooManySlides)); err == nil {
		t.Fatal("presentation exceeding the slide limit was accepted")
	}
}

func TestExtractCloudDSHOfficeTaskContentPPTXEmbeddedImages(t *testing.T) {
	pngBytes := cloudDSHTestImage(t, "image/png", 2, 2)
	jpegBytes := cloudDSHTestImage(t, "image/jpeg", 3, 2)
	content := cloudDSHPPTXImageFixture(t,
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Slide text stays available</a:t></a:r></a:p></p:txBody></p:sp><p:pic><p:blipFill><a:blip r:embed="rIdPng"/></p:blipFill></p:pic><p:pic><p:blipFill><a:blip r:embed="rIdJpeg"/></p:blipFill></p:pic></p:spTree></p:cSld></p:sld>`,
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdPng" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/diagram.png"/><Relationship Id="rIdJpeg" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/photo.jpeg"/></Relationships>`,
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/png"/><Default Extension="jpeg" ContentType="image/jpeg"/></Types>`,
		map[string][]byte{"ppt/media/diagram.png": pngBytes, "ppt/media/photo.jpeg": jpegBytes})
	text, images, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, content)
	if err != nil || !strings.Contains(text, "Slide text stays available") || len(images) != 2 ||
		images[0].MediaType != "image/png" || !bytes.Equal(images[0].Data, pngBytes) ||
		images[1].MediaType != "image/jpeg" || !bytes.Equal(images[1].Data, jpegBytes) {
		t.Fatalf("PPTX task content text=%q images=%#v err=%v", text, images, err)
	}
}

func TestExtractCloudDSHOfficeTaskContentPPTXRejectsUnsafeImages(t *testing.T) {
	imageBytes := cloudDSHTestImage(t, "image/png", 2, 2)
	tests := []struct {
		name  string
		rels  string
		types string
		files map[string][]byte
	}{
		{
			name:  "external relationship",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="https://example.invalid/image.png" TargetMode="External"/></Relationships>`,
			types: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/png"/></Types>`,
		},
		{
			name:  "relationship path escapes package",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../../../../image.png"/></Relationships>`,
			types: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/png"/></Types>`,
		},
		{
			name:  "unknown image MIME",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image.png"/></Relationships>`,
			types: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/gif"/></Types>`,
			files: map[string][]byte{"ppt/media/image.png": imageBytes},
		},
		{
			name:  "corrupt image bytes",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image.png"/></Relationships>`,
			types: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/png"/></Types>`,
			files: map[string][]byte{"ppt/media/image.png": []byte("corrupt")},
		},
		{
			name:  "image has no declared MIME",
			rels:  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image.png"/></Relationships>`,
			types: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
			files: map[string][]byte{"ppt/media/image.png": imageBytes},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := cloudDSHPPTXImageFixture(t,
				`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>keep text</a:t></a:r></a:p></p:txBody></p:sp><p:pic><p:blipFill><a:blip r:embed="rId1"/></p:blipFill></p:pic></p:spTree></p:cSld></p:sld>`,
				test.rels, test.types, test.files)
			if _, _, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, content); err == nil {
				t.Fatal("unsafe or malformed image relationship was accepted")
			}
		})
	}

	linked := cloudDSHPPTXImageFixture(t,
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:pic><p:blipFill><a:blip r:link="rId1"/></p:blipFill></p:pic></p:spTree></p:cSld></p:sld>`,
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`, nil)
	if _, _, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, linked); err == nil {
		t.Fatal("linked external image was accepted")
	}
}

func TestExtractCloudDSHOfficeTaskContentPPTXImageLimitsAreReported(t *testing.T) {
	contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="png" ContentType="image/png"/></Types>`
	imageBytes := cloudDSHTestImage(t, "image/png", 2, 2)
	var slide strings.Builder
	slide.WriteString(`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>all original text</a:t></a:r></a:p></p:txBody></p:sp>`)
	var rels strings.Builder
	rels.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	files := make(map[string][]byte)
	for index := 1; index <= maxCloudDSHTaskAttachmentImages+1; index++ {
		id := "rId" + strconv.Itoa(index)
		name := "image" + strconv.Itoa(index) + ".png"
		slide.WriteString(`<p:pic><p:blipFill><a:blip r:embed="` + id + `"/></p:blipFill></p:pic>`)
		rels.WriteString(`<Relationship Id="` + id + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/` + name + `"/>`)
		files["ppt/media/"+name] = imageBytes
	}
	slide.WriteString(`</p:spTree></p:cSld></p:sld>`)
	rels.WriteString(`</Relationships>`)
	content := cloudDSHPPTXImageFixture(t, slide.String(), rels.String(), contentTypes, files)
	text, images, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, content)
	if err != nil || len(images) != maxCloudDSHTaskAttachmentImages ||
		!strings.Contains(text, "all original text") || !strings.Contains(text, "image count limit") || !strings.Contains(text, "incomplete") {
		t.Fatalf("count-limited PPTX content text=%q images=%d err=%v", text, len(images), err)
	}

	largeImage := append(append([]byte(nil), imageBytes...), bytes.Repeat([]byte("x"), maxCloudDSHTaskAttachmentImageBytes)...)
	large := cloudDSHPPTXImageFixture(t,
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>text remains</a:t></a:r></a:p></p:txBody></p:sp><p:pic><p:blipFill><a:blip r:embed="rId1"/></p:blipFill></p:pic></p:spTree></p:cSld></p:sld>`,
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image.png"/></Relationships>`,
		contentTypes, map[string][]byte{"ppt/media/image.png": largeImage})
	text, images, err = extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, large)
	if err != nil || len(images) != 0 || !strings.Contains(text, "text remains") || !strings.Contains(text, "per-image byte limit") {
		t.Fatalf("byte-limited PPTX content text=%q images=%d err=%v", text, len(images), err)
	}

	largePixels := cloudDSHTestImage(t, "image/png", 1281, 1)
	tooManyPixels := cloudDSHPPTXImageFixture(t,
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>pixel text remains</a:t></a:r></a:p></p:txBody></p:sp><p:pic><p:blipFill><a:blip r:embed="rId1"/></p:blipFill></p:pic></p:spTree></p:cSld></p:sld>`,
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image.png"/></Relationships>`,
		contentTypes, map[string][]byte{"ppt/media/image.png": largePixels})
	text, images, err = extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, tooManyPixels)
	if err != nil || len(images) != 0 || !strings.Contains(text, "pixel text remains") || !strings.Contains(text, "pixel image limit") {
		t.Fatalf("pixel-limited PPTX content text=%q images=%d err=%v", text, len(images), err)
	}
}

func TestExtractCloudDSHOfficeTaskContentPPTXMetadataLimitsFailClosed(t *testing.T) {
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	slide := `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree>`
	for index := 0; index <= maxCloudDSHPPTXEmbeddedReferences; index++ {
		slide += `<p:pic><p:blipFill><a:blip r:embed="rId` + strconv.Itoa(index) + `"/></p:blipFill></p:pic>`
	}
	slide += `</p:spTree></p:cSld></p:sld>`
	tooManyReferences := cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":            presentation,
		"ppt/_rels/presentation.xml.rels": presentationRels,
		"ppt/slides/slide1.xml":           slide,
	})
	if _, _, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, tooManyReferences); err == nil {
		t.Fatal("slide XML exceeding the embedded reference limit was accepted")
	}

	var relationships strings.Builder
	relationships.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for index := 0; index < maxCloudDSHPPTXRelationships; index++ {
		id := "rIdUnused" + strconv.Itoa(index)
		relationships.WriteString(`<Relationship Id="` + id + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/unused` + strconv.Itoa(index) + `.png"/>`)
	}
	relationships.WriteString(`</Relationships>`)
	tooManyRelationships := cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":             presentation,
		"ppt/_rels/presentation.xml.rels":  presentationRels,
		"ppt/slides/slide1.xml":            `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree/></p:cSld></p:sld>`,
		"ppt/slides/_rels/slide1.xml.rels": relationships.String(),
	})
	if _, _, err := extractCloudDSHOfficeTaskContent(cloudDSHOfficePPTXMediaType, tooManyRelationships); err == nil {
		t.Fatal("package exceeding the relationship limit was accepted")
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
		{name: "missing pptx presentation", mediaType: cloudDSHOfficePPTXMediaType, content: cloudDSHOfficeZipFixture(t, map[string]string{"ppt/slides/slide1.xml": `<p:sld/>`})},
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
		!validCloudDSHTaskAttachmentMediaType(cloudDSHOfficePPTXMediaType) ||
		validCloudDSHTaskAttachmentMediaType("application/zip") {
		t.Fatal("Office attachment media type allowlist is incorrect")
	}
}

func cloudDSHOfficeZipFixture(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	binaryEntries := make(map[string][]byte, len(entries))
	for name, content := range entries {
		binaryEntries[name] = []byte(content)
	}
	return cloudDSHOfficeZipBytesFixture(t, binaryEntries)
}

func cloudDSHOfficeZipBytesFixture(t *testing.T, entries map[string][]byte) []byte {
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

func cloudDSHPPTXImageFixture(t *testing.T, slide, relationships, contentTypes string, media map[string][]byte) []byte {
	t.Helper()
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	entries := map[string][]byte{
		"[Content_Types].xml":              []byte(contentTypes),
		"ppt/presentation.xml":             []byte(presentation),
		"ppt/_rels/presentation.xml.rels":  []byte(presentationRels),
		"ppt/slides/slide1.xml":            []byte(slide),
		"ppt/slides/_rels/slide1.xml.rels": []byte(relationships),
	}
	for name, content := range media {
		entries[name] = content
	}
	return cloudDSHOfficeZipBytesFixture(t, entries)
}

func cloudDSHPPTXChartFixture(t *testing.T, slide, relationships, chart string) []byte {
	t.Helper()
	presentation, presentationRels := cloudDSHPPTXPresentationParts([]string{"ppt/slides/slide1.xml"})
	return cloudDSHOfficeZipFixture(t, map[string]string{
		"ppt/presentation.xml":             presentation,
		"ppt/_rels/presentation.xml.rels":  presentationRels,
		"ppt/slides/slide1.xml":            slide,
		"ppt/slides/_rels/slide1.xml.rels": relationships,
		"ppt/charts/chart1.xml":            chart,
	})
}

func cloudDSHPPTXPresentationParts(slideNames []string) (string, string) {
	var presentation, relationships strings.Builder
	presentation.WriteString(`<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>`)
	relationships.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for index, slideName := range slideNames {
		relationshipID := "rIdSlide" + strconv.Itoa(index+1)
		presentation.WriteString(`<p:sldId id="` + strconv.Itoa(index+1) + `" r:id="` + relationshipID + `"/>`)
		relationships.WriteString(`<Relationship Id="` + relationshipID + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="` + strings.TrimPrefix(slideName, "ppt/") + `"/>`)
	}
	presentation.WriteString(`</p:sldIdLst></p:presentation>`)
	relationships.WriteString(`</Relationships>`)
	return presentation.String(), relationships.String()
}

func cloudDSHTestImage(t *testing.T, mediaType string, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	var err error
	switch mediaType {
	case "image/png":
		err = png.Encode(&output, value)
	case "image/jpeg":
		err = jpeg.Encode(&output, value, &jpeg.Options{Quality: 80})
	default:
		t.Fatalf("unsupported fixture image MIME %q", mediaType)
	}
	if err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
