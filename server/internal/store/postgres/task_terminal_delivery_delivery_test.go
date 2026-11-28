package postgres

import (
	"strings"
	"testing"

	"morde/server/internal/app"
)

func TestBoundedTerminalDeliveryObjectiveTrimsChatWhitespace(t *testing.T) {
	for _, input := range []string{
		"\n  你看看今天有什么新闻\n",
		"\u3000你看看今天有什么新闻\u3000",
	} {
		got, truncated := boundedTerminalDeliveryObjective(input)
		if truncated || got != "你看看今天有什么新闻" {
			t.Fatalf("boundedTerminalDeliveryObjective(%q) = %q, %t", input, got, truncated)
		}
	}
}

func TestBoundedTerminalDeliveryObjectiveTrimsTruncatedPreview(t *testing.T) {
	input := "  " + strings.Repeat("你", app.MaxTaskTerminalDeliveryObjectiveSummaryBytes/3+1) + "\n"
	got, truncated := boundedTerminalDeliveryObjective(input)
	if !truncated || got == "" || strings.TrimSpace(got) != got || len(got) > app.MaxTaskTerminalDeliveryObjectiveSummaryBytes {
		t.Fatalf("truncated objective is not a bounded, trimmed UTF-8 preview: len=%d truncated=%t", len(got), truncated)
	}
	if !strings.HasPrefix(got, "你") {
		t.Fatalf("truncated objective lost its first content rune: %q", got[:min(len(got), 16)])
	}
}

func TestBoundedTerminalDeliveryObjectiveHandlesWhitespaceOnlyInput(t *testing.T) {
	got, truncated := boundedTerminalDeliveryObjective(" \n\t\u3000")
	if truncated || got != "user request unavailable" {
		t.Fatalf("whitespace-only objective = %q, %t", got, truncated)
	}
}
