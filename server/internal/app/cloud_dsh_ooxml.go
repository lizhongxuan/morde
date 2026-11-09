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
	cloudDSHOfficePPTXMediaType        = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	maxCloudDSHOfficeAttachmentBytes   = 16 << 20
	maxCloudDSHOfficeExpandedBytes     = 32 << 20
	maxCloudDSHOfficeEntryBytes        = 16 << 20
	maxCloudDSHOfficeZipEntries        = 256
	maxCloudDSHOfficeSheets            = 32
	maxCloudDSHOfficeSlides            = 32
	maxCloudDSHPPTXMetadataPartBytes   = 256 << 10
	maxCloudDSHPPTXRelationships       = 256
	maxCloudDSHPPTXEmbeddedReferences  = 256
	maxCloudDSHPPTXChartSeries         = 16
	maxCloudDSHPPTXChartPoints         = 128
	maxCloudDSHOfficeSharedStrings     = 100_000
	maxCloudDSHOfficeWorkbookCells     = 100_000
	maxCloudDSHOfficeExtractedTextSize = 48 << 10
)

var errCloudDSHOfficeDocument = errors.New("office document cannot be read safely")

const (
	cloudDSHPackageRelationshipsNamespace     = "http://schemas.openxmlformats.org/package/2006/relationships"
	cloudDSHContentTypesNamespace             = "http://schemas.openxmlformats.org/package/2006/content-types"
	cloudDSHOfficeRelationshipsNamespace      = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	cloudDSHOfficeStrictRelationshipsNS       = "http://purl.oclc.org/ooxml/officeDocument/relationships"
	cloudDSHPresentationNamespace             = "http://schemas.openxmlformats.org/presentationml/2006/main"
	cloudDSHStrictPresentationNamespace       = "http://purl.oclc.org/ooxml/presentationml/main"
	cloudDSHDrawingMLNamespace                = "http://schemas.openxmlformats.org/drawingml/2006/main"
	cloudDSHStrictDrawingMLNamespace          = "http://purl.oclc.org/ooxml/drawingml/main"
	cloudDSHChartNamespace                    = "http://schemas.openxmlformats.org/drawingml/2006/chart"
	cloudDSHStrictChartNamespace              = "http://purl.oclc.org/ooxml/drawingml/chart"
	cloudDSHOfficeImageRelationshipType       = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/image"
	cloudDSHOfficeStrictImageRelationshipType = "http://purl.oclc.org/ooxml/officeDocument/relationships/image"
	cloudDSHOfficeSlideRelationshipType       = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
	cloudDSHOfficeStrictSlideRelationshipType = "http://purl.oclc.org/ooxml/officeDocument/relationships/slide"
	cloudDSHOfficeChartRelationshipType       = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart"
	cloudDSHOfficeStrictChartRelationshipType = "http://purl.oclc.org/ooxml/officeDocument/relationships/chart"
)

// extractCloudDSHOfficeText reads bounded OOXML text and cached chart values.
// It never extracts files to disk, follows external relationships, loads macros,
// opens embedded workbooks, or evaluates formulas.
func extractCloudDSHOfficeText(mediaType string, content []byte) (string, error) {
	entries, err := readCloudDSHOfficePackage(content)
	if err != nil {
		return "", err
	}
	text, _, err := extractCloudDSHOfficeTextFromEntries(mediaType, entries)
	return text, err
}

func extractCloudDSHOfficeTaskContent(mediaType string, content []byte) (string, []ModelImage, error) {
	if mediaType != cloudDSHOfficePPTXMediaType {
		text, err := extractCloudDSHOfficeText(mediaType, content)
		return text, nil, err
	}
	entries, err := readCloudDSHOfficePackage(content)
	if err != nil {
		return "", nil, err
	}
	return extractCloudDSHPowerPointContent(entries)
}

func readCloudDSHOfficePackage(content []byte) (map[string]*zip.File, error) {
	if len(content) == 0 || len(content) > maxCloudDSHOfficeAttachmentBytes {
		return nil, errCloudDSHOfficeDocument
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil || len(archive.File) == 0 || len(archive.File) > maxCloudDSHOfficeZipEntries {
		return nil, errCloudDSHOfficeDocument
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
			return nil, errCloudDSHOfficeDocument
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, errCloudDSHOfficeDocument
		}
		if entry.UncompressedSize64 > maxCloudDSHOfficeEntryBytes {
			return nil, errCloudDSHOfficeDocument
		}
		expanded += entry.UncompressedSize64
		if expanded > maxCloudDSHOfficeExpandedBytes {
			return nil, errCloudDSHOfficeDocument
		}
		entries[name] = entry
	}
	return entries, nil
}

func extractCloudDSHOfficeTextFromEntries(mediaType string, entries map[string]*zip.File) (string, []ModelImage, error) {
	switch mediaType {
	case cloudDSHOfficeDOCXMediaType:
		entry := entries["word/document.xml"]
		if entry == nil {
			return "", nil, errCloudDSHOfficeDocument
		}
		raw, err := readCloudDSHOfficePart(entry)
		if err != nil {
			return "", nil, err
		}
		text, err := extractCloudDSHWordXML(raw)
		return text, nil, err
	case cloudDSHOfficeXLSXMediaType:
		if entries["xl/workbook.xml"] == nil {
			return "", nil, errCloudDSHOfficeDocument
		}
		shared := []string(nil)
		if entry := entries["xl/sharedStrings.xml"]; entry != nil {
			raw, err := readCloudDSHOfficePart(entry)
			if err != nil {
				return "", nil, err
			}
			shared, err = parseCloudDSHSharedStrings(raw)
			if err != nil {
				return "", nil, err
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
			return "", nil, errCloudDSHOfficeDocument
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
				return "", nil, err
			}
			text, wasTruncated, cellCount, err := extractCloudDSHWorksheetXML(raw, shared, remaining,
				maxCloudDSHOfficeWorkbookCells-parsedCells)
			if err != nil {
				return "", nil, err
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
			return "", nil, errCloudDSHOfficeDocument
		}
		if truncated {
			output.WriteString("\n[Text extraction is partial; document text limit reached.]")
		}
		return output.String(), nil, nil
	case cloudDSHOfficePPTXMediaType:
		text, images, err := extractCloudDSHPowerPointContent(entries)
		return text, images, err
	default:
		return "", nil, errCloudDSHOfficeDocument
	}
}

type cloudDSHPPTXRelationship struct {
	ID         string
	Type       string
	TargetPart string
}

type cloudDSHPPTXSlide struct {
	number int
	name   string
}

func readCloudDSHPPTXSlidesInPresentationOrder(entries map[string]*zip.File,
	relationshipParts map[string]map[string]cloudDSHPPTXRelationship) ([]cloudDSHPPTXSlide, error) {
	presentationPart := entries["ppt/presentation.xml"]
	if presentationPart == nil || presentationPart.UncompressedSize64 > maxCloudDSHPPTXMetadataPartBytes {
		return nil, errCloudDSHOfficeDocument
	}
	presentation, err := readCloudDSHOfficePart(presentationPart)
	if err != nil {
		return nil, err
	}
	presentationRelationships := relationshipParts[cloudDSHPPTXRelationshipPart("ppt/presentation.xml")]
	if presentationRelationships == nil {
		return nil, errCloudDSHOfficeDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(presentation))
	depth := 0
	rootSeen, rootClosed := false, false
	slideListSeen, inSlideList := false, false
	seenSlideIDs := make(map[uint64]struct{})
	seenRelationshipIDs := make(map[string]struct{})
	orderedRelationshipIDs := make([]string, 0)
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
			if rootClosed {
				return nil, errCloudDSHOfficeDocument
			}
			if depth == 0 {
				if rootSeen || value.Name.Local != "presentation" || !cloudDSHPresentationNamespaceValid(value.Name.Space) {
					return nil, errCloudDSHOfficeDocument
				}
				rootSeen = true
			} else if depth == 1 && value.Name.Local == "sldIdLst" {
				if slideListSeen || !cloudDSHPresentationNamespaceValid(value.Name.Space) {
					return nil, errCloudDSHOfficeDocument
				}
				slideListSeen, inSlideList = true, true
			} else if inSlideList && depth == 2 {
				if value.Name.Local != "sldId" || !cloudDSHPresentationNamespaceValid(value.Name.Space) ||
					len(orderedRelationshipIDs) >= maxCloudDSHOfficeSlides {
					return nil, errCloudDSHOfficeDocument
				}
				var slideID, relationshipID string
				for _, attribute := range value.Attr {
					if attribute.Name.Local != "id" {
						continue
					}
					switch {
					case attribute.Name.Space == "":
						slideID = attribute.Value
					case cloudDSHOfficeRelationshipNamespace(attribute.Name.Space):
						relationshipID = attribute.Value
					default:
						return nil, errCloudDSHOfficeDocument
					}
				}
				parsedID, err := strconv.ParseUint(slideID, 10, 32)
				if err != nil || parsedID == 0 || relationshipID == "" {
					return nil, errCloudDSHOfficeDocument
				}
				if _, duplicate := seenSlideIDs[parsedID]; duplicate {
					return nil, errCloudDSHOfficeDocument
				}
				if _, duplicate := seenRelationshipIDs[relationshipID]; duplicate {
					return nil, errCloudDSHOfficeDocument
				}
				seenSlideIDs[parsedID] = struct{}{}
				seenRelationshipIDs[relationshipID] = struct{}{}
				orderedRelationshipIDs = append(orderedRelationshipIDs, relationshipID)
			} else if inSlideList && depth >= 2 {
				return nil, errCloudDSHOfficeDocument
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 || !rootSeen {
				return nil, errCloudDSHOfficeDocument
			}
			if depth == 2 && inSlideList {
				if value.Name.Local != "sldId" || !cloudDSHPresentationNamespaceValid(value.Name.Space) {
					return nil, errCloudDSHOfficeDocument
				}
			} else if depth == 1 && inSlideList {
				if value.Name.Local != "sldIdLst" || !cloudDSHPresentationNamespaceValid(value.Name.Space) {
					return nil, errCloudDSHOfficeDocument
				}
				inSlideList = false
			} else if depth == 0 {
				if value.Name.Local != "presentation" || !cloudDSHPresentationNamespaceValid(value.Name.Space) || rootClosed {
					return nil, errCloudDSHOfficeDocument
				}
				rootClosed = true
			}
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return nil, errCloudDSHOfficeDocument
			}
		}
	}
	if !rootSeen || !rootClosed || !slideListSeen || inSlideList || depth != 0 || len(seenRelationshipIDs) == 0 {
		return nil, errCloudDSHOfficeDocument
	}
	slides := make([]cloudDSHPPTXSlide, 0, len(orderedRelationshipIDs))
	seenSlideParts := make(map[string]struct{}, len(orderedRelationshipIDs))
	for _, relationshipID := range orderedRelationshipIDs {
		relationship, ok := presentationRelationships[relationshipID]
		if !ok || !cloudDSHOfficeSlideRelationship(relationship.Type) || !strings.HasPrefix(relationship.TargetPart, "ppt/slides/") ||
			strings.Contains(strings.TrimPrefix(relationship.TargetPart, "ppt/slides/"), "/") ||
			!strings.HasSuffix(relationship.TargetPart, ".xml") || entries[relationship.TargetPart] == nil {
			return nil, errCloudDSHOfficeDocument
		}
		if _, duplicate := seenSlideParts[relationship.TargetPart]; duplicate {
			return nil, errCloudDSHOfficeDocument
		}
		seenSlideParts[relationship.TargetPart] = struct{}{}
		slides = append(slides, cloudDSHPPTXSlide{number: len(slides) + 1, name: relationship.TargetPart})
	}
	if len(slides) == 0 || len(slides) > maxCloudDSHOfficeSlides {
		return nil, errCloudDSHOfficeDocument
	}
	return slides, nil
}

func extractCloudDSHPowerPointContent(entries map[string]*zip.File) (string, []ModelImage, error) {
	if entries["ppt/presentation.xml"] == nil {
		return "", nil, errCloudDSHOfficeDocument
	}
	relationships, err := readCloudDSHPPTXRelationships(entries)
	if err != nil {
		return "", nil, err
	}
	slides, err := readCloudDSHPPTXSlidesInPresentationOrder(entries, relationships)
	if err != nil {
		return "", nil, err
	}
	var output strings.Builder
	textTruncated := false
	embeddedReferenceCount := 0
	images := make([]ModelImage, 0, maxCloudDSHTaskAttachmentImages)
	seenImageParts := make(map[string]struct{})
	seenChartParts := make(map[string]struct{})
	incompleteReasons := make(map[string]bool)
	var imageBytes int
	contentTypesLoaded := false
	var defaultContentTypes, overrideContentTypes map[string]string
	for _, slide := range slides {
		raw, err := readCloudDSHOfficePart(entries[slide.name])
		if err != nil {
			return "", nil, err
		}
		text, wasTruncated, embedIDs, chartIDs, err := extractCloudDSHPowerPointSlideXML(raw,
			maxCloudDSHOfficeExtractedTextSize-output.Len(), maxCloudDSHPPTXEmbeddedReferences-embeddedReferenceCount)
		if err != nil {
			return "", nil, err
		}
		embeddedReferenceCount += len(embedIDs) + len(chartIDs)
		if text != "" && !textTruncated {
			header := "Slide " + strconv.Itoa(slide.number) + ":\n"
			separator := ""
			if output.Len() > 0 {
				separator = "\n\n"
			}
			remaining := maxCloudDSHOfficeExtractedTextSize - output.Len() - len(separator) - len(header)
			if remaining < 0 {
				textTruncated = true
			} else {
				appendCloudDSHOfficeText(&output, separator+header, maxCloudDSHOfficeExtractedTextSize, &textTruncated)
				appendCloudDSHOfficeText(&output, text, output.Len()+remaining, &textTruncated)
				textTruncated = textTruncated || wasTruncated
				if output.Len() >= maxCloudDSHOfficeExtractedTextSize {
					textTruncated = true
				}
			}
		}
		if len(embedIDs) == 0 && len(chartIDs) == 0 {
			continue
		}
		relationshipPart := cloudDSHPPTXRelationshipPart(slide.name)
		slideRelationships, ok := relationships[relationshipPart]
		if !ok {
			return "", nil, errCloudDSHOfficeDocument
		}
		for chartIndex, chartID := range chartIDs {
			relationship, ok := slideRelationships[chartID]
			if !ok || !cloudDSHOfficeChartRelationship(relationship.Type) ||
				!strings.HasPrefix(relationship.TargetPart, "ppt/charts/") ||
				strings.Contains(strings.TrimPrefix(relationship.TargetPart, "ppt/charts/"), "/") ||
				!strings.HasSuffix(relationship.TargetPart, ".xml") {
				return "", nil, errCloudDSHOfficeDocument
			}
			if _, duplicate := seenChartParts[relationship.TargetPart]; duplicate {
				continue
			}
			seenChartParts[relationship.TargetPart] = struct{}{}
			chartPart := entries[relationship.TargetPart]
			if chartPart == nil || chartPart.UncompressedSize64 > maxCloudDSHPPTXMetadataPartBytes {
				return "", nil, errCloudDSHOfficeDocument
			}
			chartXML, err := readCloudDSHOfficePart(chartPart)
			if err != nil {
				return "", nil, err
			}
			separator := "\n\n"
			if output.Len() == 0 {
				separator = ""
			}
			header := "Slide " + strconv.Itoa(slide.number) + " chart " + strconv.Itoa(chartIndex+1) + ":\n"
			remaining := maxCloudDSHOfficeExtractedTextSize - output.Len() - len(separator) - len(header)
			if remaining < 0 {
				textTruncated = true
				continue
			}
			chartText, chartTruncated, err := extractCloudDSHPPTXChartSummary(chartXML, remaining)
			if err != nil {
				return "", nil, err
			}
			appendCloudDSHOfficeText(&output, separator+header, maxCloudDSHOfficeExtractedTextSize, &textTruncated)
			appendCloudDSHOfficeText(&output, chartText, maxCloudDSHOfficeExtractedTextSize, &textTruncated)
			textTruncated = textTruncated || chartTruncated
		}
		for _, embedID := range embedIDs {
			relationship, ok := slideRelationships[embedID]
			if !ok || !cloudDSHOfficeImageRelationship(relationship.Type) {
				return "", nil, errCloudDSHOfficeDocument
			}
			if _, duplicate := seenImageParts[relationship.TargetPart]; duplicate {
				continue
			}
			seenImageParts[relationship.TargetPart] = struct{}{}
			imagePart := entries[relationship.TargetPart]
			if imagePart == nil {
				return "", nil, errCloudDSHOfficeDocument
			}
			if !contentTypesLoaded {
				defaultContentTypes, overrideContentTypes, err = readCloudDSHOfficeContentTypes(entries)
				if err != nil {
					return "", nil, err
				}
				contentTypesLoaded = true
			}
			mediaType := cloudDSHOfficeContentType(relationship.TargetPart, defaultContentTypes, overrideContentTypes)
			if mediaType != "image/png" && mediaType != "image/jpeg" {
				return "", nil, errCloudDSHOfficeDocument
			}
			if imagePart.UncompressedSize64 > uint64(maxCloudDSHTaskAttachmentImageBytes) {
				incompleteReasons["byte"] = true
				continue
			}
			if len(images) >= maxCloudDSHTaskAttachmentImages {
				incompleteReasons["count"] = true
				continue
			}
			if imageBytes+int(imagePart.UncompressedSize64) > maxCloudDSHTaskAttachmentImagesBytes {
				incompleteReasons["total"] = true
				continue
			}
			imageData, err := readCloudDSHOfficePart(imagePart)
			if err != nil {
				return "", nil, err
			}
			valid, pixelLimitExceeded := inspectCloudDSHTaskAttachmentImage(mediaType, imageData)
			if pixelLimitExceeded {
				incompleteReasons["pixels"] = true
				continue
			}
			if !valid {
				return "", nil, errCloudDSHOfficeDocument
			}
			imageBytes += len(imageData)
			images = append(images, ModelImage{MediaType: mediaType, Data: imageData})
		}
	}
	if output.Len() == 0 && len(images) == 0 {
		return "", nil, errCloudDSHOfficeDocument
	}
	if output.Len() == 0 {
		output.WriteString("[No slide text was extracted.]")
	}
	if textTruncated {
		output.WriteString("\n[Text extraction is partial; document text limit reached.]")
	}
	if len(incompleteReasons) > 0 {
		var reasons []string
		for _, reason := range []struct{ key, text string }{
			{"count", "image count limit (4)"},
			{"byte", "512 KiB per-image byte limit"},
			{"total", "2 MiB total image byte limit"},
			{"pixels", "921600-pixel image limit"},
		} {
			if incompleteReasons[reason.key] {
				reasons = append(reasons, reason.text)
			}
		}
		output.WriteString("\n[Embedded slide images are incomplete: some images were omitted due to the ")
		output.WriteString(strings.Join(reasons, ", "))
		output.WriteString(".]")
	}
	return output.String(), images, nil
}

func cloudDSHPPTXRelationshipPart(ownerPart string) string {
	return path.Join(path.Dir(ownerPart), "_rels", path.Base(ownerPart)+".rels")
}

func readCloudDSHPPTXRelationships(entries map[string]*zip.File) (map[string]map[string]cloudDSHPPTXRelationship, error) {
	parts := make([]string, 0)
	for name := range entries {
		if strings.HasSuffix(name, ".rels") {
			parts = append(parts, name)
		}
	}
	sort.Strings(parts)
	result := make(map[string]map[string]cloudDSHPPTXRelationship, len(parts))
	relationshipCount := 0
	for _, name := range parts {
		owner, err := cloudDSHPPTXRelationshipOwner(name)
		if err != nil {
			return nil, err
		}
		entry := entries[name]
		if entry.UncompressedSize64 > maxCloudDSHPPTXMetadataPartBytes {
			return nil, errCloudDSHOfficeDocument
		}
		content, err := readCloudDSHOfficePart(entry)
		if err != nil {
			return nil, err
		}
		relationships, err := parseCloudDSHPPTXRelationships(content, owner, maxCloudDSHPPTXRelationships-relationshipCount)
		if err != nil {
			return nil, err
		}
		relationshipCount += len(relationships)
		result[name] = relationships
	}
	return result, nil
}

func cloudDSHPPTXRelationshipOwner(name string) (string, error) {
	if name == "_rels/.rels" {
		return "", nil
	}
	index := strings.LastIndex(name, "/_rels/")
	if index < 0 {
		return "", errCloudDSHOfficeDocument
	}
	base := name[index+len("/_rels/"):]
	if base == "" || strings.Contains(base, "/") || !strings.HasSuffix(base, ".rels") {
		return "", errCloudDSHOfficeDocument
	}
	owner := path.Join(name[:index], strings.TrimSuffix(base, ".rels"))
	if owner == "." || strings.HasPrefix(owner, "../") || strings.HasPrefix(owner, "/") {
		return "", errCloudDSHOfficeDocument
	}
	return owner, nil
}

func parseCloudDSHPPTXRelationships(content []byte, owner string, limit int) (map[string]cloudDSHPPTXRelationship, error) {
	if limit < 0 {
		return nil, errCloudDSHOfficeDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(content))
	relationships := make(map[string]cloudDSHPPTXRelationship)
	depth := 0
	rootSeen, rootClosed := false, false
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
			if rootClosed {
				return nil, errCloudDSHOfficeDocument
			}
			if depth == 0 {
				if rootSeen || value.Name.Local != "Relationships" || value.Name.Space != cloudDSHPackageRelationshipsNamespace {
					return nil, errCloudDSHOfficeDocument
				}
				rootSeen = true
			} else {
				if depth != 1 || value.Name.Local != "Relationship" || value.Name.Space != cloudDSHPackageRelationshipsNamespace {
					return nil, errCloudDSHOfficeDocument
				}
				attributes := make(map[string]string, len(value.Attr))
				for _, attribute := range value.Attr {
					if attribute.Name.Space == "xmlns" || attribute.Name.Local == "xmlns" {
						continue
					}
					if attribute.Name.Space != "" {
						return nil, errCloudDSHOfficeDocument
					}
					if _, duplicate := attributes[attribute.Name.Local]; duplicate {
						return nil, errCloudDSHOfficeDocument
					}
					attributes[attribute.Name.Local] = attribute.Value
				}
				id, relationType, target := attributes["Id"], attributes["Type"], attributes["Target"]
				targetMode := attributes["TargetMode"]
				if id == "" || relationType == "" || target == "" ||
					(targetMode != "" && !strings.EqualFold(targetMode, "internal")) ||
					strings.EqualFold(targetMode, "external") {
					return nil, errCloudDSHOfficeDocument
				}
				targetPart, err := resolveCloudDSHPPTXTarget(owner, target)
				if err != nil {
					return nil, err
				}
				if _, duplicate := relationships[id]; duplicate {
					return nil, errCloudDSHOfficeDocument
				}
				if len(relationships) >= limit {
					return nil, errCloudDSHOfficeDocument
				}
				relationships[id] = cloudDSHPPTXRelationship{ID: id, Type: relationType, TargetPart: targetPart}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 || !rootSeen {
				return nil, errCloudDSHOfficeDocument
			}
			if depth == 0 {
				if value.Name.Local != "Relationships" || value.Name.Space != cloudDSHPackageRelationshipsNamespace || rootClosed {
					return nil, errCloudDSHOfficeDocument
				}
				rootClosed = true
			} else if depth != 1 || value.Name.Local != "Relationship" || value.Name.Space != cloudDSHPackageRelationshipsNamespace {
				return nil, errCloudDSHOfficeDocument
			}
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return nil, errCloudDSHOfficeDocument
			}
		}
	}
	if !rootSeen || !rootClosed || depth != 0 {
		return nil, errCloudDSHOfficeDocument
	}
	return relationships, nil
}

func resolveCloudDSHPPTXTarget(owner, target string) (string, error) {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\\\x00?#%:") {
		return "", errCloudDSHOfficeDocument
	}
	directory := path.Dir(owner)
	if owner == "" {
		directory = "."
	}
	resolved := path.Clean(path.Join(directory, target))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
		return "", errCloudDSHOfficeDocument
	}
	return resolved, nil
}

func readCloudDSHOfficeContentTypes(entries map[string]*zip.File) (map[string]string, map[string]string, error) {
	entry := entries["[Content_Types].xml"]
	if entry == nil || entry.UncompressedSize64 > maxCloudDSHPPTXMetadataPartBytes {
		return nil, nil, errCloudDSHOfficeDocument
	}
	content, err := readCloudDSHOfficePart(entry)
	if err != nil {
		return nil, nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(content))
	defaults, overrides := make(map[string]string), make(map[string]string)
	depth := 0
	rootSeen, rootClosed := false, false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, errCloudDSHOfficeDocument
		}
		switch value := token.(type) {
		case xml.Directive:
			return nil, nil, errCloudDSHOfficeDocument
		case xml.StartElement:
			if rootClosed {
				return nil, nil, errCloudDSHOfficeDocument
			}
			if depth == 0 {
				if rootSeen || value.Name.Local != "Types" || value.Name.Space != cloudDSHContentTypesNamespace {
					return nil, nil, errCloudDSHOfficeDocument
				}
				rootSeen = true
			} else {
				if depth != 1 || value.Name.Space != cloudDSHContentTypesNamespace {
					return nil, nil, errCloudDSHOfficeDocument
				}
				attributes := make(map[string]string, len(value.Attr))
				for _, attribute := range value.Attr {
					if attribute.Name.Space == "xmlns" || attribute.Name.Local == "xmlns" {
						continue
					}
					if attribute.Name.Space != "" {
						return nil, nil, errCloudDSHOfficeDocument
					}
					if _, duplicate := attributes[attribute.Name.Local]; duplicate {
						return nil, nil, errCloudDSHOfficeDocument
					}
					attributes[attribute.Name.Local] = attribute.Value
				}
				contentType := attributes["ContentType"]
				if contentType == "" {
					return nil, nil, errCloudDSHOfficeDocument
				}
				switch value.Name.Local {
				case "Default":
					extension := strings.ToLower(attributes["Extension"])
					if extension == "" || strings.ContainsAny(extension, "./\\\x00: ") || len(attributes) != 2 {
						return nil, nil, errCloudDSHOfficeDocument
					}
					if _, duplicate := defaults[extension]; duplicate {
						return nil, nil, errCloudDSHOfficeDocument
					}
					defaults[extension] = contentType
				case "Override":
					partName := attributes["PartName"]
					if len(attributes) != 2 || !strings.HasPrefix(partName, "/") || strings.HasPrefix(partName, "//") ||
						strings.ContainsAny(partName, "\\\x00?#%:") || path.Clean(strings.TrimPrefix(partName, "/")) != strings.TrimPrefix(partName, "/") {
						return nil, nil, errCloudDSHOfficeDocument
					}
					partName = strings.TrimPrefix(partName, "/")
					if _, duplicate := overrides[partName]; duplicate {
						return nil, nil, errCloudDSHOfficeDocument
					}
					overrides[partName] = contentType
				default:
					return nil, nil, errCloudDSHOfficeDocument
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 || !rootSeen {
				return nil, nil, errCloudDSHOfficeDocument
			}
			if depth == 0 {
				if value.Name.Local != "Types" || value.Name.Space != cloudDSHContentTypesNamespace || rootClosed {
					return nil, nil, errCloudDSHOfficeDocument
				}
				rootClosed = true
			} else if depth != 1 || value.Name.Space != cloudDSHContentTypesNamespace ||
				(value.Name.Local != "Default" && value.Name.Local != "Override") {
				return nil, nil, errCloudDSHOfficeDocument
			}
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return nil, nil, errCloudDSHOfficeDocument
			}
		}
	}
	if !rootSeen || !rootClosed || depth != 0 {
		return nil, nil, errCloudDSHOfficeDocument
	}
	return defaults, overrides, nil
}

func cloudDSHOfficeContentType(part string, defaults, overrides map[string]string) string {
	if value, ok := overrides[part]; ok {
		return value
	}
	extension := strings.TrimPrefix(strings.ToLower(path.Ext(part)), ".")
	return defaults[extension]
}

func cloudDSHOfficeRelationshipNamespace(value string) bool {
	return value == cloudDSHOfficeRelationshipsNamespace || value == cloudDSHOfficeStrictRelationshipsNS
}

func cloudDSHPresentationNamespaceValid(value string) bool {
	return value == cloudDSHPresentationNamespace || value == cloudDSHStrictPresentationNamespace
}

func cloudDSHDrawingMLNamespaceValid(value string) bool {
	return value == cloudDSHDrawingMLNamespace || value == cloudDSHStrictDrawingMLNamespace
}

func cloudDSHOfficeImageRelationship(value string) bool {
	return value == cloudDSHOfficeImageRelationshipType || value == cloudDSHOfficeStrictImageRelationshipType
}

func cloudDSHOfficeSlideRelationship(value string) bool {
	return value == cloudDSHOfficeSlideRelationshipType || value == cloudDSHOfficeStrictSlideRelationshipType
}

func cloudDSHOfficeChartRelationship(value string) bool {
	return value == cloudDSHOfficeChartRelationshipType || value == cloudDSHOfficeStrictChartRelationshipType
}

func cloudDSHChartNamespaceValid(value string) bool {
	return value == cloudDSHChartNamespace || value == cloudDSHStrictChartNamespace
}

type cloudDSHPPTXChartField uint8

const (
	cloudDSHPPTXChartFieldNone cloudDSHPPTXChartField = iota
	cloudDSHPPTXChartFieldTitle
	cloudDSHPPTXChartFieldCategory
	cloudDSHPPTXChartFieldValue
)

type cloudDSHPPTXChartSeries struct {
	title      string
	titleSeen  bool
	categories map[int]string
	values     map[int]string
	fields     map[cloudDSHPPTXChartField]bool
}

func extractCloudDSHPPTXChartSummary(content []byte, limit int) (string, bool, error) {
	if limit < 0 || len(content) > maxCloudDSHPPTXMetadataPartBytes {
		return "", false, errCloudDSHOfficeDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(content))
	stack := make([]xml.Name, 0, 16)
	series := make([]cloudDSHPPTXChartSeries, 0)
	var currentSeries *cloudDSHPPTXChartSeries
	var seriesDepth int
	var activeField cloudDSHPPTXChartField
	var activeFieldDepth int
	var pointIndex = -1
	var valueActive bool
	var valueIndex int
	var value strings.Builder
	rootSeen, rootClosed := false, false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, errCloudDSHOfficeDocument
		}
		switch item := token.(type) {
		case xml.Directive:
			return "", false, errCloudDSHOfficeDocument
		case xml.StartElement:
			depth := len(stack)
			if rootClosed {
				return "", false, errCloudDSHOfficeDocument
			}
			if !rootSeen {
				if item.Name.Local != "chartSpace" || !cloudDSHChartNamespaceValid(item.Name.Space) {
					return "", false, errCloudDSHOfficeDocument
				}
				rootSeen = true
			}
			if item.Name.Local == "ser" && cloudDSHChartNamespaceValid(item.Name.Space) {
				if currentSeries != nil || len(series) >= maxCloudDSHPPTXChartSeries {
					return "", false, errCloudDSHOfficeDocument
				}
				series = append(series, cloudDSHPPTXChartSeries{
					categories: make(map[int]string),
					values:     make(map[int]string),
					fields:     make(map[cloudDSHPPTXChartField]bool),
				})
				currentSeries = &series[len(series)-1]
				seriesDepth = depth
			}
			if currentSeries != nil && depth == seriesDepth+1 && cloudDSHChartNamespaceValid(item.Name.Space) {
				field := cloudDSHPPTXChartFieldNone
				switch item.Name.Local {
				case "tx":
					field = cloudDSHPPTXChartFieldTitle
				case "cat":
					field = cloudDSHPPTXChartFieldCategory
				case "val":
					field = cloudDSHPPTXChartFieldValue
				}
				if field != cloudDSHPPTXChartFieldNone {
					if activeField != cloudDSHPPTXChartFieldNone || currentSeries.fields[field] {
						return "", false, errCloudDSHOfficeDocument
					}
					activeField, activeFieldDepth = field, depth
					currentSeries.fields[field] = true
				}
			}
			if currentSeries != nil && activeField != cloudDSHPPTXChartFieldNone &&
				item.Name.Local == "ptCount" && cloudDSHChartNamespaceValid(item.Name.Space) {
				var pointCount string
				for _, attribute := range item.Attr {
					if attribute.Name.Local == "val" && attribute.Name.Space == "" {
						pointCount = attribute.Value
					}
				}
				parsedCount, err := strconv.Atoi(pointCount)
				if err != nil || parsedCount < 0 || parsedCount > maxCloudDSHPPTXChartPoints {
					return "", false, errCloudDSHOfficeDocument
				}
			}
			if currentSeries != nil && activeField != cloudDSHPPTXChartFieldNone &&
				item.Name.Local == "pt" && cloudDSHChartNamespaceValid(item.Name.Space) {
				if pointIndex >= 0 {
					return "", false, errCloudDSHOfficeDocument
				}
				var rawIndex string
				for _, attribute := range item.Attr {
					if attribute.Name.Local == "idx" && attribute.Name.Space == "" {
						rawIndex = attribute.Value
					}
				}
				parsedIndex, err := strconv.Atoi(rawIndex)
				if err != nil || parsedIndex < 0 || parsedIndex >= maxCloudDSHPPTXChartPoints {
					return "", false, errCloudDSHOfficeDocument
				}
				pointIndex = parsedIndex
			}
			if currentSeries != nil && activeField != cloudDSHPPTXChartFieldNone &&
				item.Name.Local == "v" && cloudDSHChartNamespaceValid(item.Name.Space) {
				if valueActive {
					return "", false, errCloudDSHOfficeDocument
				}
				directTitleValue := activeField == cloudDSHPPTXChartFieldTitle && len(stack) > 0 &&
					stack[len(stack)-1].Local == "tx" && cloudDSHChartNamespaceValid(stack[len(stack)-1].Space)
				if pointIndex >= 0 || directTitleValue {
					value.Reset()
					valueActive = true
					valueIndex = pointIndex
					if valueIndex < 0 {
						valueIndex = 0
					}
				}
			}
			stack = append(stack, item.Name)
		case xml.CharData:
			if valueActive {
				if value.Len()+len(item) > 1024 {
					return "", false, errCloudDSHOfficeDocument
				}
				value.Write(item)
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != item.Name {
				return "", false, errCloudDSHOfficeDocument
			}
			depth := len(stack) - 1
			if valueActive && item.Name.Local == "v" && cloudDSHChartNamespaceValid(item.Name.Space) {
				data := strings.TrimSpace(value.String())
				switch activeField {
				case cloudDSHPPTXChartFieldTitle:
					if valueIndex != 0 || currentSeries.titleSeen {
						return "", false, errCloudDSHOfficeDocument
					}
					currentSeries.title, currentSeries.titleSeen = data, true
				case cloudDSHPPTXChartFieldCategory:
					if _, duplicate := currentSeries.categories[valueIndex]; duplicate {
						return "", false, errCloudDSHOfficeDocument
					}
					currentSeries.categories[valueIndex] = data
				case cloudDSHPPTXChartFieldValue:
					if _, duplicate := currentSeries.values[valueIndex]; duplicate {
						return "", false, errCloudDSHOfficeDocument
					}
					currentSeries.values[valueIndex] = data
				}
				valueActive = false
			}
			if currentSeries != nil && pointIndex >= 0 && depth > activeFieldDepth &&
				item.Name.Local == "pt" && cloudDSHChartNamespaceValid(item.Name.Space) {
				pointIndex = -1
			}
			if currentSeries != nil && activeField != cloudDSHPPTXChartFieldNone && depth == activeFieldDepth &&
				cloudDSHChartNamespaceValid(item.Name.Space) {
				activeField, activeFieldDepth = cloudDSHPPTXChartFieldNone, 0
			}
			if currentSeries != nil && depth == seriesDepth && item.Name.Local == "ser" && cloudDSHChartNamespaceValid(item.Name.Space) {
				if activeField != cloudDSHPPTXChartFieldNone || pointIndex >= 0 || valueActive {
					return "", false, errCloudDSHOfficeDocument
				}
				currentSeries = nil
			}
			if depth == 0 {
				if item.Name.Local != "chartSpace" || !cloudDSHChartNamespaceValid(item.Name.Space) || rootClosed {
					return "", false, errCloudDSHOfficeDocument
				}
				rootClosed = true
			}
			stack = stack[:depth]
		}
	}
	if !rootSeen || !rootClosed || len(stack) != 0 || currentSeries != nil {
		return "", false, errCloudDSHOfficeDocument
	}
	var output strings.Builder
	truncated := false
	if len(series) == 0 {
		appendCloudDSHOfficeText(&output, "[No cached chart data; formulas are not evaluated.]", limit, &truncated)
		return output.String(), truncated, nil
	}
	for seriesIndex, item := range series {
		if seriesIndex > 0 {
			appendCloudDSHOfficeText(&output, "\n", limit, &truncated)
		}
		label := strings.Join(strings.Fields(strings.TrimSpace(item.title)), " ")
		if label == "" {
			label = "Series " + strconv.Itoa(seriesIndex+1)
		}
		appendCloudDSHOfficeText(&output, "Series "+strconv.Quote(label)+": ", limit, &truncated)
		if len(item.values) == 0 {
			appendCloudDSHOfficeText(&output, "[No cached values.]", limit, &truncated)
			continue
		}
		first := true
		for index := 0; index < maxCloudDSHPPTXChartPoints; index++ {
			value, ok := item.values[index]
			if !ok {
				continue
			}
			category, ok := item.categories[index]
			if !ok || strings.TrimSpace(category) == "" {
				category = "#" + strconv.Itoa(index)
			}
			category = strings.Join(strings.Fields(strings.TrimSpace(category)), " ")
			value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
			separator := "; "
			if first {
				separator, first = "", false
			}
			appendCloudDSHOfficeText(&output, separator+strconv.Quote(category)+"="+strconv.Quote(value), limit, &truncated)
		}
	}
	return output.String(), truncated, nil
}

func extractCloudDSHPowerPointSlideXML(content []byte, limit, referenceLimit int) (string, bool, []string, []string, error) {
	if limit < 0 || referenceLimit < 0 {
		return "", false, nil, nil, errCloudDSHOfficeDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(content))
	var output strings.Builder
	var paragraph strings.Builder
	rootSeen := false
	inParagraph := false
	inText := false
	truncated := false
	embedIDs := make([]string, 0)
	seenEmbedIDs := make(map[string]struct{})
	chartIDs := make([]string, 0)
	seenChartIDs := make(map[string]struct{})
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, nil, nil, errCloudDSHOfficeDocument
		}
		switch value := token.(type) {
		case xml.Directive:
			return "", false, nil, nil, errCloudDSHOfficeDocument
		case xml.StartElement:
			if !rootSeen {
				rootSeen = true
				if value.Name.Local != "sld" {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
			}
			if value.Name.Local == "blip" {
				if !cloudDSHDrawingMLNamespaceValid(value.Name.Space) {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
				var embedID string
				for _, attribute := range value.Attr {
					if attribute.Name.Local != "embed" && attribute.Name.Local != "link" {
						continue
					}
					if !cloudDSHOfficeRelationshipNamespace(attribute.Name.Space) || attribute.Value == "" {
						return "", false, nil, nil, errCloudDSHOfficeDocument
					}
					if attribute.Name.Local == "link" || embedID != "" {
						return "", false, nil, nil, errCloudDSHOfficeDocument
					}
					embedID = attribute.Value
				}
				if embedID != "" {
					if _, duplicate := seenChartIDs[embedID]; duplicate {
						return "", false, nil, nil, errCloudDSHOfficeDocument
					}
					if _, duplicate := seenEmbedIDs[embedID]; !duplicate {
						if len(embedIDs)+len(chartIDs) >= referenceLimit {
							return "", false, nil, nil, errCloudDSHOfficeDocument
						}
						seenEmbedIDs[embedID] = struct{}{}
						embedIDs = append(embedIDs, embedID)
					}
				}
			}
			if value.Name.Local == "chart" && cloudDSHChartNamespaceValid(value.Name.Space) {
				var chartID string
				for _, attribute := range value.Attr {
					if attribute.Name.Local != "id" {
						continue
					}
					if !cloudDSHOfficeRelationshipNamespace(attribute.Name.Space) || attribute.Value == "" || chartID != "" {
						return "", false, nil, nil, errCloudDSHOfficeDocument
					}
					chartID = attribute.Value
				}
				if chartID == "" {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
				if _, duplicate := seenEmbedIDs[chartID]; duplicate {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
				if _, duplicate := seenChartIDs[chartID]; !duplicate {
					if len(embedIDs)+len(chartIDs) >= referenceLimit {
						return "", false, nil, nil, errCloudDSHOfficeDocument
					}
					seenChartIDs[chartID] = struct{}{}
					chartIDs = append(chartIDs, chartID)
				}
			}
			switch value.Name.Local {
			case "p":
				if inParagraph {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
				inParagraph = true
				paragraph.Reset()
			case "t":
				inText = inParagraph
			}
		case xml.CharData:
			if inText && !truncated {
				appendCloudDSHOfficeBytes(&paragraph, value, limit-output.Len()-paragraph.Len(), &truncated)
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				if !inParagraph {
					return "", false, nil, nil, errCloudDSHOfficeDocument
				}
				text := strings.TrimSpace(paragraph.String())
				if text != "" {
					appendCloudDSHOfficeText(&output, text, limit, &truncated)
					appendCloudDSHOfficeText(&output, "\n", limit, &truncated)
				}
				paragraph.Reset()
				inParagraph = false
			}
		}
	}
	if !rootSeen || inParagraph {
		return "", false, nil, nil, errCloudDSHOfficeDocument
	}
	return strings.TrimSpace(output.String()), truncated, embedIDs, chartIDs, nil
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
