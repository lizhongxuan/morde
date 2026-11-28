package postgres

import (
	"testing"

	"morde/server/internal/app"
)

func TestIncompleteObservabilitySpanTree(t *testing.T) {
	rootID, childID := "0000000000000001", "0000000000000002"
	for _, test := range []struct {
		name  string
		spans []app.AdminObservabilitySpan
		want  bool
	}{
		{name: "empty", spans: []app.AdminObservabilitySpan{}, want: false},
		{name: "complete", spans: []app.AdminObservabilitySpan{{SpanID: rootID}, {SpanID: childID, ParentSpanID: &rootID}}, want: false},
		{name: "orphan", spans: []app.AdminObservabilitySpan{{SpanID: childID, ParentSpanID: &rootID}}, want: true},
		{name: "cycle", spans: []app.AdminObservabilitySpan{{SpanID: rootID, ParentSpanID: &childID}, {SpanID: childID, ParentSpanID: &rootID}}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := incompleteObservabilitySpanTree(test.spans); got != test.want {
				t.Fatalf("incompleteObservabilitySpanTree() = %v, want %v", got, test.want)
			}
		})
	}
}
