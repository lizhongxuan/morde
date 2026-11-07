package app

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	cloudDSHOfficeDOCXMediaType        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	cloudDSHOfficeXLSXMediaType        = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	maxCloudDSHOfficeAttachmentBytes   = 16 << 20
	maxCloudDSHOfficeExpandedBytes     = 32 << 20
	maxCloudDSHOfficeEntryBytes        = 16 << 20
	maxCloudDSHOfficeZipEntries        = 256
	maxCloudDSHOfficeSheets            = 32
	maxCloudDSHOfficeSharedStrings     = 100_000
	maxCloudDSHOfficeWorkbookCells     = 100_000
	maxCloudDSHOfficeExtractedTextSize = 48 << 10
)

var errCloudDSHOfficeDocument = errors.New("office document cannot be read safely")

// extractCloudDSHOfficeText reads only the OOXML text parts needed for a basic
// document/table summary. It never extracts files to disk, follows external
// relationships, loads macros, or evaluates formulas.
func extractCloudDSHOfficeText(mediaType string, content []byte) (string, error) {
	if len(content) == 0 || len(content) > maxCloudDSHOfficeAttachmentBytes {
		return "", errCloudDSHOfficeDocument
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil || len(archive.File) == 0 || len(archive.File) > maxCloudDSHOfficeZipEntries {
		return "", errCloudDSHOfficeDocument
	}
	entries := make(map[string]*zip.File, len(archive.File))
	var expanded uint64
	for _, entry := range archive.File {
		name := entry.Name
		cleanName := path.Clean(name)
		cleanEntryName := cleanName
		if entry.FileInfo().IsDir() {
			cleanEntryName += "/"
		}
		if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || cleanEntryName != name ||
			name == "." || strings.HasPrefix(name, "../") || entry.Flags&1 != 0 {
			return "", errCloudDSHOfficeDocument
		}
		if _, duplicate := entries[name]; duplicate {
			return "", errCloudDSHOfficeDocument
		}
		if entry.UncompressedSize64 > maxCloudDSHOfficeEntryBytes {
			return "", errCloudDSHOfficeDocument
		}
		expanded += entry.UncompressedSize64
		if expanded > maxCloudDSHOfficeExpandedBytes {
			return "", errCloudDSHOfficeDocument
		}
		entries[name] = entry
	}

	switch mediaType {
	case cloudDSHOfficeDOCXMediaType:
		entry := entries["word/document.xml"]
		if entry == nil {
			return "", errCloudDSHOfficeDocument
		}
		raw, err := readCloudDSHOfficePart(entry)
		if err != nil {
			return "", err
		}
		return extractCloudDSHWordXML(raw)
	case cloudDSHOfficeXLSXMediaType:
		if entries["xl/workbook.xml"] == nil {
			return "", errCloudDSHOfficeDocument
		}
		shared := []string(nil)
		if entry := entries["xl/sharedStrings.xml"]; entry != nil {
			raw, err := readCloudDSHOfficePart(entry)
			if err != nil {
				return "", err
			}
			shared, err = parseCloudDSHSharedStrings(raw)
			if err != nil {
				return "", err
			}
		}
		sheets := make([]string, 0)
		for name := range entries {
			if strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml") {
				sheets = append(sheets, name)
			}
		}
		sort.Strings(sheets)
		if len(sheets) == 0 || len(sheets) > maxCloudDSHOfficeSheets {
			return "", errCloudDSHOfficeDocument
		}
		var output strings.Builder
		truncated := false
		parsedCells := 0
		for _, name := range sheets {
			header := "Sheet " + path.Base(name) + ":\n"
			prefix := ""
			if output.Len() > 0 {
				prefix = "\n"
			}
			remaining := maxCloudDSHOfficeExtractedTextSize - output.Len() - len(prefix) - len(header)
			if remaining < 0 {
				truncated = true
				break
			}
			raw, err := readCloudDSHOfficePart(entries[name])
			if err != nil {
				return "", err
			}
			text, wasTruncated, cellCount, err := extractCloudDSHWorksheetXML(raw, shared, remaining,
				maxCloudDSHOfficeWorkbookCells-parsedCells)
			if err != nil {
				return "", err
			}
			parsedCells += cellCount
			output.WriteString(prefix)
			output.WriteString(header)
			output.WriteString(text)
			truncated = truncated || wasTruncated
			if output.Len() >= maxCloudDSHOfficeExtractedTextSize || truncated {
				truncated = true
				break
			}
		}
		if output.Len() == 0 {
			return "", errCloudDSHOfficeDocument
		}
		if truncated {
			output.WriteString("\n[Text extraction is partial; document text limit reached.]")
		}
		return output.String(), nil
	default:
		return "", errCloudDSHOfficeDocument
	}
}

func readCloudDSHOfficePart(entry *zip.File) ([]byte, error) {
	if entry == nil || entry.UncompressedSize64 > maxCloudDSHOfficeEntryBytes {
		return nil, errCloudDSHOfficeDocument
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, errCloudDSHOfficeDocument
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, maxCloudDSHOfficeEntryBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || uint64(len(content)) != entry.UncompressedSize64 || len(content) > maxCloudDSHOfficeEntryBytes {
		return nil, errCloudDSHOfficeDocument
	}
	return content, nil
}

func extractCloudDSHWordXML(content []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	var output strings.Builder
	var paragraph strings.Builder
	inText := false
	inParagraph := false
	truncated := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errCloudDSHOfficeDocument
		}
		switch value := token.(type) {
		case xml.Directive:
			return "", errCloudDSHOfficeDocument
		case xml.StartElement:
			switch value.Name.Local {
			case "p":
				if inParagraph {
					appendCloudDSHOfficeText(&output, paragraph.String(), maxCloudDSHOfficeExtractedTextSize, &truncated)
					appendCloudDSHOfficeText(&output, "\n", maxCloudDSHOfficeExtractedTextSize, &truncated)
					paragraph.Reset()
				}
				inParagraph = true
			case "t":
				inText = inParagraph
			}
		case xml.CharData:
			if inText && !truncated {
				appendCloudDSHOfficeBytes(&paragraph, value, maxCloudDSHOfficeExtractedTextSize-output.Len()-paragraph.Len(), &truncated)
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				if inParagraph {
					appendCloudDSHOfficeText(&output, paragraph.String(), maxCloudDSHOfficeExtractedTextSize, &truncated)
					appendCloudDSHOfficeText(&output, "\n", maxCloudDSHOfficeExtractedTextSize, &truncated)
					paragraph.Reset()
				}
				inParagraph = false
			}
		}
	}
	if inParagraph || output.Len() == 0 {
		return "", errCloudDSHOfficeDocument
	}
	text := strings.TrimSpace(output.String())
	if text == "" {
		return "", errCloudDSHOfficeDocument
	}
	if truncated {
		text += "\n[Text extraction is partial; document text limit reached.]"
	}
	return text, nil
}

func parseCloudDSHSharedStrings(content []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	values := make([]string, 0)
	var current strings.Builder
	inItem := false
	inText := false
	total := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errCloudDSHOfficeDocument
		}
		switch value := token.(type) {
		case xml.Directive:
			return nil, errCloudDSHOfficeDocument
		case xml.StartElement:
			switch value.Name.Local {
			case "si":
				if inItem || len(values) >= maxCloudDSHOfficeSharedStrings {
					return nil, errCloudDSHOfficeDocument
				}
				current.Reset()
				inItem = true
			case "t":
				inText = inItem
			}
		case xml.CharData:
			if inText {
				if total+len(value) > maxCloudDSHOfficeExpandedBytes {
					return nil, errCloudDSHOfficeDocument
				}
				current.Write(value)
				total += len(value)
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "si":
				if !inItem {
					return nil, errCloudDSHOfficeDocument
				}
				values = append(values, current.String())
				inItem = false
			}
		}
	}
	if inItem {
		return nil, errCloudDSHOfficeDocument
	}
	return values, nil
}

func extractCloudDSHWorksheetXML(content []byte, shared []string, limit, cellBudget int) (string, bool, int, error) {
	if limit < 0 || cellBudget < 0 {
		return "", false, 0, errCloudDSHOfficeDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(content))
	var output strings.Builder
	var cellRef, cellType string
	var value, inline strings.Builder
	var row strings.Builder
	rowCells := 0
	rowTruncated := false
	cellTruncated := false
	cellTextLimit := 0
	cells := 0
	inRow := false
	inCell := false
	inValue := false
	inInline := false
	truncated := false
	appendRowPart := func(part string) {
		rowLimit := limit - output.Len() - 1 // Keep one byte for the row separator.
		if rowLimit < 0 {
			rowLimit = 0
		}
		appendCloudDSHOfficeText(&row, part, rowLimit, &rowTruncated)
	}
	flushCell := func() error {
		if !inCell {
			return nil
		}
		text := value.String()
		switch cellType {
		case "s":
			index, err := strconv.Atoi(text)
			if err != nil || index < 0 || index >= len(shared) {
				return errCloudDSHOfficeDocument
			}
			text = shared[index]
		case "inlineStr":
			text = inline.String()
		case "str":
			// Formula cached values may be stored as a string.
		case "b":
			switch text {
			case "1":
				text = "TRUE"
			case "0":
				text = "FALSE"
			}
		}
		if cellTruncated {
			rowTruncated = true
		}
		if text != "" {
			if rowCells > 0 {
				appendRowPart(" | ")
			}
			appendRowPart(cellRef + "=")
			appendRowPart(text)
			rowCells++
			rowTruncated = rowTruncated || cellTruncated
		}
		return nil
	}
	flushRow := func() {
		if rowCells == 0 {
			truncated = truncated || rowTruncated
			row.Reset()
			rowTruncated = false
			return
		}
		output.WriteString(row.String())
		if output.Len() < limit {
			output.WriteByte('\n')
		} else {
			truncated = true
		}
		truncated = truncated || rowTruncated
		row.Reset()
		rowCells = 0
		rowTruncated = false
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, cells, errCloudDSHOfficeDocument
		}
		switch token := token.(type) {
		case xml.Directive:
			return "", false, cells, errCloudDSHOfficeDocument
		case xml.StartElement:
			switch token.Name.Local {
			case "row":
				if inRow || inCell {
					return "", false, cells, errCloudDSHOfficeDocument
				}
				inRow = true
				row.Reset()
				rowCells = 0
				rowTruncated = false
			case "c":
				if !inRow || inCell {
					return "", false, cells, errCloudDSHOfficeDocument
				}
				cells++
				if cells > cellBudget {
					return "", false, cells, errCloudDSHOfficeDocument
				}
				inCell = true
				cellRef, cellType = "", ""
				value.Reset()
				inline.Reset()
				cellTruncated = false
				for _, attribute := range token.Attr {
					switch attribute.Name.Local {
					case "r":
						cellRef = attribute.Value
					case "t":
						cellType = attribute.Value
					}
				}
				if !validCloudDSHSpreadsheetCellReference(cellRef) || !validCloudDSHSpreadsheetCellType(cellType) {
					return "", false, cells, errCloudDSHOfficeDocument
				}
				cellTextLimit = limit - output.Len() - row.Len() - len(cellRef) - 1 - 1
				if rowCells > 0 {
					cellTextLimit -= len(" | ")
				}
				if cellTextLimit < 0 {
					cellTextLimit = 0
				}
			case "v":
				inValue = inCell
			case "t":
				inInline = inCell && cellType == "inlineStr"
			}
		case xml.CharData:
			if inValue {
				if cellType == "s" {
					if value.Len()+len(token) > 16 {
						return "", false, cells, errCloudDSHOfficeDocument
					}
					value.Write(token)
				} else {
					appendCloudDSHOfficeBytes(&value, token, cellTextLimit, &cellTruncated)
				}
			}
			if inInline {
				appendCloudDSHOfficeBytes(&inline, token, cellTextLimit, &cellTruncated)
			}
		case xml.EndElement:
			switch token.Name.Local {
			case "v":
				inValue = false
			case "t":
				inInline = false
			case "c":
				if err := flushCell(); err != nil {
					return "", false, cells, err
				}
				inCell = false
			case "row":
				if !inRow || inCell {
					return "", false, cells, errCloudDSHOfficeDocument
				}
				flushRow()
				inRow = false
			}
		}
	}
	if inCell || inRow {
		return "", false, cells, errCloudDSHOfficeDocument
	}
	if output.Len() == 0 {
		return "", truncated, cells, nil
	}
	return strings.TrimSpace(output.String()), truncated, cells, nil
}

func appendCloudDSHOfficeText(output *strings.Builder, value string, limit int, truncated *bool) {
	if limit < 0 || output.Len() >= limit {
		*truncated = true
		return
	}
	if len(value) > limit-output.Len() {
		value = value[:limit-output.Len()]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		*truncated = true
	}
	output.WriteString(value)
}

func appendCloudDSHOfficeBytes(output *strings.Builder, value []byte, limit int, truncated *bool) {
	if limit < 0 || output.Len() >= limit {
		*truncated = true
		return
	}
	if len(value) > limit-output.Len() {
		value = value[:limit-output.Len()]
		for !utf8.Valid(value) {
			value = value[:len(value)-1]
		}
		*truncated = true
	}
	_, _ = output.Write(value)
}

func validCloudDSHSpreadsheetCellReference(value string) bool {
	if value == "" || len(value) > 10 {
		return false
	}
	index := 0
	column := 0
	for index < len(value) && value[index] >= 'A' && value[index] <= 'Z' {
		column = column*26 + int(value[index]-'A'+1)
		index++
	}
	if index == 0 || index > 3 || column > 16384 || index == len(value) {
		return false
	}
	row, err := strconv.Atoi(value[index:])
	return err == nil && row > 0 && row <= 1_048_576
}

func validCloudDSHSpreadsheetCellType(value string) bool {
	switch value {
	case "", "s", "inlineStr", "str", "b", "n", "d", "e":
		return true
	default:
		return false
	}
}
