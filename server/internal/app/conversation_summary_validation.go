package app

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxConversationSummaryFailurePreviewRunes bounds the model-text preview
// carried by a summary validation failure: enough to identify the failing
// output in server logs, never the whole summary.
const MaxConversationSummaryFailurePreviewRunes = 1024

// Named reasons for an invalid conversation summary model output. The
// gateway used to collapse all six conditions into one generic message,
// which made every summary failure undiagnosable ("0 versions" with no
// failing condition named). Each reason names the first failed condition.
const (
	ConversationSummaryFailureStopReason  = "summary_stop_reason"
	ConversationSummaryFailureToolCalls   = "summary_tool_calls"
	ConversationSummaryFailureBlankText   = "summary_blank_text"
	ConversationSummaryFailureInvalidUTF8 = "summary_invalid_utf8"
	ConversationSummaryFailureNULByte     = "summary_nul_byte"
	ConversationSummaryFailureTooLarge    = "summary_text_too_large"
)

// ConversationSummaryValidationFailure names the first failed condition of
// an invalid conversation summary, with the evidence needed to tell the
// conditions apart in a log line: the stop reason, tool call count, text
// byte count and a bounded preview of the text.
type ConversationSummaryValidationFailure struct {
	Reason     string
	StopReason string
	ToolCalls  int
	TextBytes  int
	Preview    string
}

// Error renders the failure as a single diagnostic line. The preview is
// server-log evidence only; it is never returned to the user.
func (f *ConversationSummaryValidationFailure) Error() string {
	if f.StopReason == "" {
		return fmt.Sprintf("%s (text_bytes=%d, preview=%q)", f.Reason, f.TextBytes, f.Preview)
	}
	return fmt.Sprintf("%s (stop_reason=%s, tool_calls=%d, text_bytes=%d, preview=%q)",
		f.Reason, f.StopReason, f.ToolCalls, f.TextBytes, f.Preview)
}

// ValidateConversationSummaryModelResponse checks a model response offered
// as a conversation summary. It returns nil when the response is a usable
// summary, otherwise the first failed condition in the order the gateway
// used to check them.
func ValidateConversationSummaryModelResponse(response ModelResponse) *ConversationSummaryValidationFailure {
	if response.StopReason != "stop" {
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureStopReason,
			StopReason: response.StopReason, ToolCalls: len(response.ToolCalls),
			TextBytes: len(response.Text), Preview: summaryFailurePreview(response.Text)}
	}
	if len(response.ToolCalls) != 0 {
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureToolCalls,
			StopReason: response.StopReason, ToolCalls: len(response.ToolCalls),
			TextBytes: len(response.Text), Preview: summaryFailurePreview(response.Text)}
	}
	return validateConversationSummaryText(response.Text)
}

// validateConversationSummaryText checks only the text-validity conditions
// shared by the gateway response check and the Seal functions.
func validateConversationSummaryText(text string) *ConversationSummaryValidationFailure {
	switch {
	case strings.TrimSpace(text) == "":
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureBlankText,
			TextBytes: len(text), Preview: summaryFailurePreview(text)}
	case !utf8.ValidString(text):
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureInvalidUTF8,
			TextBytes: len(text), Preview: summaryFailurePreview(text)}
	case strings.ContainsRune(text, '\x00'):
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureNULByte,
			TextBytes: len(text), Preview: summaryFailurePreview(text)}
	case len(text) > MaxConversationSummaryBytes:
		return &ConversationSummaryValidationFailure{Reason: ConversationSummaryFailureTooLarge,
			TextBytes: len(text), Preview: summaryFailurePreview(text)}
	}
	return nil
}

// summaryFailurePreview truncates text to a bounded rune preview for
// diagnostics. Invalid UTF-8 is rendered with replacement runes; the
// preview identifies the output, it is not a copy of it.
func summaryFailurePreview(text string) string {
	return truncateRunes(text, MaxConversationSummaryFailurePreviewRunes)
}
