package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ConversationRouteInternalToolCall is the durable one-call input for a
// read-only route action that bypasses DSH but still executes through Broker.
type ConversationRouteInternalToolCall struct {
	TaskID         string
	OwnerID        string
	MessageID      string
	CallID         string
	ToolName       string
	Arguments      string
	ReplyMessageID string
}

type ConversationRouteInternalToolStore interface {
	GetConversationRouteInternalToolCall(context.Context, string, string) (ConversationRouteInternalToolCall, error)
	PublishPendingConversationRouteInternalToolReplies(context.Context) (bool, error)
}

func ConversationRouteDirectToolReply(name string, result AgentToolResult) string {
	label := "网页查询"
	if name == cloudDSHWebReadToolName {
		label = "网页读取"
	}
	if result.Status == AgentToolFailed {
		label += "失败"
	} else {
		label += "结果"
	}
	var output cloudDSHMCPOutput
	if json.Unmarshal([]byte(result.Output), &output) != nil {
		return boundedConversationRouteToolReply(label+"：\n"+result.Output, MaxAssistantMessageBytes)
	}
	parts := append([]string(nil), output.Text...)
	if len(output.StructuredContent) != 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, output.StructuredContent, "", "  ") == nil {
			parts = append(parts, pretty.String())
		} else {
			parts = append(parts, string(output.StructuredContent))
		}
	}
	content := strings.Join(parts, "\n\n")
	if strings.TrimSpace(content) == "" {
		content = "工具未返回可显示的文字内容。"
	}
	return boundedConversationRouteToolReply(label+"：\n"+content, MaxAssistantMessageBytes)
}

func boundedConversationRouteToolReply(value string, limit int) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if !utf8.ValidString(value) {
		return "网页工具返回的内容无法安全显示。"
	}
	if len(value) <= limit {
		return value
	}
	const suffix = "\n\n（结果过长，已截断。）"
	cut := limit - len(suffix)
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + suffix
}
