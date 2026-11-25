package app

import (
	"regexp"
	"strings"
)

var (
	fileDeliveryClaimChinese = regexp.MustCompile(`(?:已经|已|现已|成功).{0,20}(?:保存|存储|存入|写入|创建|生成|导出|上传|交付)`)
	fileDeliveryClaimEnglish = regexp.MustCompile(`(?i)\b(?:saved|created|generated|written|exported|uploaded|stored|delivered)\b`)
	fileDeliveryClaimTarget  = regexp.MustCompile(`(?i)文件|文档|附件|工作区|pdf|excel|word|ppt|csv|xlsx|docx|pptx|file|document|attachment|workspace|spreadsheet|workbook|presentation|/[a-z0-9_.-]+/`)
)

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

// HasUnverifiedFileDeliveryClaim recognizes a limited set of explicit claims.
// It is a conservative guard, not a proof that arbitrary prose is truthful.
func HasUnverifiedFileDeliveryClaim(content string) bool {
	for _, clause := range splitFileDeliveryClauses(unquotedFileDeliveryText(strings.ToLower(content))) {
		if !fileDeliveryClaimTarget.MatchString(clause) {
			continue
		}
		for _, pattern := range []*regexp.Regexp{fileDeliveryClaimChinese, fileDeliveryClaimEnglish} {
			for _, match := range pattern.FindAllStringIndex(clause, -1) {
				prefix := clause[:match[0]]
				matched := clause[match[0]:match[1]]
				if containsAny(matched, "没有", "尚未", "并未", "未能", "不能", "无法", "不要", "不会", "不再", "取消") {
					continue
				}
				if containsAny(prefix, "没有", "尚未", "并未", "未曾", "不能", "无法", "不要", "不应", "不会", "没有说", "如何", "怎样", "怎么", "假如", "如果", "例如", "not ", "never ", "cannot ", "can't ", "haven't ", "hasn't ", "how to ", "whether ", "if ", "example") ||
					strings.HasSuffix(strings.TrimSpace(prefix), "未") || strings.HasSuffix(strings.TrimSpace(prefix), "没") || strings.HasSuffix(strings.TrimSpace(prefix), "不") {
					continue
				}
				return true
			}
		}
	}
	return false
}

func splitFileDeliveryClauses(content string) []string {
	return strings.FieldsFunc(content, func(r rune) bool {
		return strings.ContainsRune("，,。！？!?；;\n\r", r)
	})
}

func unquotedFileDeliveryText(content string) string {
	var out strings.Builder
	var quote rune
	for _, r := range content {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '“':
			quote = '”'
		case '「':
			quote = '」'
		case '"', '`':
			quote = r
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
