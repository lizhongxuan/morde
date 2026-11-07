package app

import (
	"regexp"
	"strings"

	"morde/server/internal/domain/task"
)

var (
	fileDeliveryNegative     = regexp.MustCompile(`(?i)(?:不要|无需|不用|不必|不需要|不再|别(?:再)?(?:生成|创建|制作|写入|写|保存|导出|输出))\s*(?:再\s*)?(?:在工作区\s*)?(?:生成|创建|制作|写入|写|保存|导出|提供|输出|产生)?\s*(?:任何|一个|一份|个|份)?\s*(?:文件|文档|附件|pdf|excel|word|ppt|表格|工作簿|幻灯片)|(?:只要|只需|只需要|仅需|仅用).{0,5}(?:文字|文本|聊天)|(?:don't|do not|no need to)\s+(?:create|write|save|generate|produce|export|output|deliver)\s+(?:(?:any|a|the)\s+)?(?:file|document|attachment|pdf|spreadsheet)|(?:no file|no attachment|text only|text-only|answer in chat|reply in chat)`)
	fileDeliveryOutput       = regexp.MustCompile(`(?i)(?:生成|创建|新建|制作|做一份|做一个|做个|导出|另存|保存|输出|写入|写|提供|交付|create|generate|make|produce|export|save|write|deliver)\s*(?:一份|一个|个|份|a\s+|an\s+|the\s+|one\s+|my\s+|this\s+|me\s+)*\s*(?:pdf|excel|word|ppt|csv|xlsx|docx|pptx|文件|文档|附件|表格文件|电子表格|工作簿|幻灯片|file|document|attachment|spreadsheet|workbook|presentation)`)
	fileDeliveryReadOutput   = regexp.MustCompile(`输出(?:这个|该|当前)?(?:文件|文档)(?:的)?(?:内容|名称|名|大小|路径|格式|类型)`)
	fileDeliveryClaimChinese = regexp.MustCompile(`(?:已经|已|现已|成功).{0,20}(?:保存|存储|存入|写入|创建|生成|导出|上传|交付)`)
	fileDeliveryClaimEnglish = regexp.MustCompile(`(?i)\b(?:saved|created|generated|written|exported|uploaded|stored|delivered)\b`)
	fileDeliveryClaimTarget  = regexp.MustCompile(`(?i)文件|文档|附件|工作区|pdf|excel|word|ppt|csv|xlsx|docx|pptx|file|document|attachment|workspace|spreadsheet|workbook|presentation|/[a-z0-9_.-]+/`)
)

// RoutedFileDeliveryDirective only changes a routed Task's intent for an
// explicit request. Ambiguous requests inherit the previous revision instead
// of turning an ordinary answer into a required file.
func RoutedFileDeliveryDirective(content string, hasWorkspace bool) (task.FileDeliveryIntent, bool) {
	text := strings.ToLower(strings.TrimSpace(content))
	if text == "" {
		return "", false
	}
	negative := fileDeliveryNegative.MatchString(text)
	positiveText := fileDeliveryReadOutput.ReplaceAllString(fileDeliveryNegative.ReplaceAllString(text, " "), " ")
	positive := false
	for _, clause := range splitFileDeliveryClauses(positiveText) {
		classification := ClassifyFileDeliveryIntent(clause, hasWorkspace)
		if classification != task.FileDeliveryRequired {
			continue
		}
		if containsAny(clause, "如何", "怎样", "怎么", "举例", "教程", "how to", "how do", "example") {
			continue
		}
		if fileDeliveryOutput.MatchString(clause) || containsAny(clause, "在工作区", "到工作区", "写入工作区", "保存至工作区",
			"in the workspace", "in the attached workspace", "to the workspace", "to the attached workspace") ||
			hasWorkspace && containsAny(clause, "修改", "修复", "实现", "开发", "重构", "编辑", "更新", "fix", "implement", "develop", "refactor", "edit", "modify", "update") &&
				containsAny(clause, "代码", "源码", "脚本", "项目", "仓库", "code", "source", "script", "project", "repository", "repo") {
			positive = true
		}
	}
	switch {
	case positive && negative:
		return task.FileDeliveryUnclear, true
	case positive:
		return task.FileDeliveryRequired, true
	case negative:
		return task.FileDeliveryNone, true
	default:
		return "", false
	}
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
