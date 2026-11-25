package app

import (
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func repeatedErrorTestTime(hour int) time.Time {
	return time.Date(2026, time.November, 25, hour, 0, 0, 0, time.UTC)
}

func TestRepeatedErrorLedgerRecordsAndQueries(t *testing.T) {
	ledger := NewRepeatedErrorLedger()
	signature := RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "test_failed"}
	if ledger.IsRepeated(signature) {
		t.Fatal("unrecorded error reports repeated")
	}
	record, err := ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 1, At: repeatedErrorTestTime(10)})
	if err != nil || record.Repeated() || len(record.Occurrences) != 1 {
		t.Fatalf("first occurrence = %+v err=%v, want one occurrence, not repeated", record, err)
	}
	record, err = ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q7654321", Sequence: 2, At: repeatedErrorTestTime(11)})
	if err != nil || !record.Repeated() || len(record.Occurrences) != 2 {
		t.Fatalf("second occurrence = %+v err=%v, want two occurrences, repeated", record, err)
	}
	if !record.FirstSeenAt().Equal(repeatedErrorTestTime(10)) || !record.LastSeenAt().Equal(repeatedErrorTestTime(11)) {
		t.Fatalf("first/last seen = %v/%v, want 10:00/11:00", record.FirstSeenAt(), record.LastSeenAt())
	}
	if record.TaskAttempts("q1234567") != 1 || record.TaskAttempts("q7654321") != 1 || record.TaskAttempts("q0000000") != 0 {
		t.Fatalf("task attempts = %+v, want 1/1/0", record)
	}
	// Idempotent replay: the same (task, sequence) recorded again stays one.
	record, err = ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 1, At: repeatedErrorTestTime(12)})
	if err != nil || len(record.Occurrences) != 2 {
		t.Fatalf("replayed occurrence = %+v err=%v, want still two occurrences", record, err)
	}
	found, ok := ledger.Find(signature)
	if !ok || !found.Repeated() || len(found.Occurrences) != 2 {
		t.Fatalf("Find() = %+v %t, want the repeated record", found, ok)
	}
	if _, ok := ledger.Find(RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "other"}); ok {
		t.Fatal("Find() returned a record for an unrecorded reason")
	}
	// A different reason is a different error, not a third occurrence.
	other, err := ledger.Record(RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "compile_failed"},
		RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 3, At: repeatedErrorTestTime(12)})
	if err != nil || other.Repeated() {
		t.Fatalf("different reason = %+v err=%v, want a separate single-occurrence record", other, err)
	}
	listed := ledger.List()
	if len(listed) != 2 || listed[0].Signature.Reason != "test_failed" || listed[1].Signature.Reason != "compile_failed" {
		t.Fatalf("List() = %+v, want two records in first-seen order", listed)
	}
}

func TestRepeatedErrorLedgerRejectsUnkeyableErrors(t *testing.T) {
	ledger := NewRepeatedErrorLedger()
	at := repeatedErrorTestTime(10)
	for name, signature := range map[string]RepeatedErrorSignature{
		"empty kind":   {Kind: "other", Tool: "go_test", Reason: "test_failed"},
		"empty tool":   {Kind: RepeatedErrorKindTool, Tool: "", Reason: "test_failed"},
		"blank tool":   {Kind: RepeatedErrorKindTool, Tool: "  ", Reason: "test_failed"},
		"empty reason": {Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 1, At: at}); err == nil {
				t.Fatalf("Record(%+v) accepted an unkeyable signature", signature)
			}
		})
	}
	for name, occurrence := range map[string]RepeatedErrorOccurrence{
		"empty task":   {TaskID: "", Sequence: 1, At: at},
		"zero time":    {TaskID: "q1234567", Sequence: 1},
		"zero seq":     {TaskID: "q1234567", Sequence: 0, At: at},
		"negative seq": {TaskID: "q1234567", Sequence: -1, At: at},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ledger.Record(RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "test_failed"}, occurrence); err == nil {
				t.Fatalf("Record(%+v) accepted an invalid occurrence", occurrence)
			}
		})
	}
	// Surrounding whitespace is trimmed, not a separate key.
	record, err := ledger.Record(RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: " go_test ", Reason: " test_failed "},
		RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 1, At: at})
	if err != nil || record.Signature.Tool != "go_test" || record.Signature.Reason != "test_failed" {
		t.Fatalf("whitespace signature = %+v err=%v, want trimmed", record, err)
	}
}

func TestRepeatedErrorLedgerRecordsTaskSteps(t *testing.T) {
	failed := func(tool, reason string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: tool, FailureReason: reason, Status: "failed", Sequence: sequence}
	}
	succeeded := func(tool string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: tool, Status: "succeeded", Sequence: sequence}
	}
	ledger := NewRepeatedErrorLedger()
	// One repeated pair plus one single failure plus one later success: only
	// the repeated pair comes back, and the success is not an occurrence.
	repeated, skipped, err := ledger.RecordTaskSteps("q1234567", []TaskValidationStepRecord{
		failed("go_test", "test_failed", 1),
		failed("go_test", "test_failed", 2),
		failed("go_build", "compile_failed", 3),
		succeeded("go_test", 4),
	}, repeatedErrorTestTime(10))
	if err != nil || skipped != 0 || len(repeated) != 1 || repeated[0].Signature.Tool != "go_test" {
		t.Fatalf("RecordTaskSteps() = %+v skipped=%d err=%v, want one repeated go_test record", repeated, skipped, err)
	}
	// A second task failing the same way makes it repeated across tasks.
	repeated, skipped, err = ledger.RecordTaskSteps("q7654321", []TaskValidationStepRecord{
		failed("go_build", "compile_failed", 1),
	}, repeatedErrorTestTime(11))
	if err != nil || skipped != 0 || len(repeated) != 1 || repeated[0].Signature.Tool != "go_build" {
		t.Fatalf("cross-task RecordTaskSteps() = %+v skipped=%d err=%v, want one repeated go_build record", repeated, skipped, err)
	}
	// Unkeyable failures are counted as skipped, not recorded under a blank key.
	_, skipped, err = ledger.RecordTaskSteps("q1234567", []TaskValidationStepRecord{
		{FailureReason: "test_failed", Status: "failed", Sequence: 5},
		failed("go_test", "", 6),
		{ToolName: "go_test", FailureReason: "test_failed", Status: "failed", Sequence: 0},
	}, repeatedErrorTestTime(12))
	if err != nil || skipped != 3 {
		t.Fatalf("unkeyable steps skipped=%d err=%v, want 3", skipped, err)
	}
	if _, _, err := ledger.RecordTaskSteps("", nil, repeatedErrorTestTime(12)); err == nil {
		t.Fatal("RecordTaskSteps() accepted an empty task")
	}
	if _, _, err := ledger.RecordTaskSteps("q1234567", nil, time.Time{}); err == nil {
		t.Fatal("RecordTaskSteps() accepted a zero time")
	}
}

// The ledger agrees with E09 wherever both can key the error — and the one
// place they differ is deliberate: E09 counts an empty reason as repeated
// by tool, while the ledger refuses a blank key (different unknown reasons
// would collapse into one bucket), so it counts those steps as skipped.
func TestRepeatedErrorLedgerAgreesWithCorrectionFacts(t *testing.T) {
	failed := func(tool, reason string, sequence int64) TaskValidationStepRecord {
		return TaskValidationStepRecord{ToolName: tool, FailureReason: reason, Status: "failed", Sequence: sequence}
	}
	for _, tc := range []struct {
		name       string
		steps      []TaskValidationStepRecord
		wantRepeat bool
	}{
		{"single failure", []TaskValidationStepRecord{failed("go_test", "test_failed", 1)}, false},
		{"repeated failure", []TaskValidationStepRecord{failed("go_test", "test_failed", 1), failed("go_test", "test_failed", 2)}, true},
		{"different reasons", []TaskValidationStepRecord{failed("go_test", "test_failed", 1), failed("go_test", "compile_failed", 2)}, false},
		{"unnamed tool", []TaskValidationStepRecord{{FailureReason: "test_failed", Status: "failed", Sequence: 1}, {FailureReason: "test_failed", Status: "failed", Sequence: 2}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := DeriveTaskValidationCorrectionFacts(tc.steps)
			ledger := NewRepeatedErrorLedger()
			repeated, _, err := ledger.RecordTaskSteps("q1234567", tc.steps, repeatedErrorTestTime(10))
			if err != nil {
				t.Fatal(err)
			}
			e09Repeated := state == TaskValidationFactPresent
			ledgerRepeated := len(repeated) > 0
			if e09Repeated != tc.wantRepeat || ledgerRepeated != tc.wantRepeat {
				t.Fatalf("E09 repeated=%t ledger repeated=%t, want %t", e09Repeated, ledgerRepeated, tc.wantRepeat)
			}
		})
	}
	t.Run("empty reason diverges by design", func(t *testing.T) {
		steps := []TaskValidationStepRecord{failed("go_test", "", 1), failed("go_test", "", 2)}
		state, _ := DeriveTaskValidationCorrectionFacts(steps)
		if state != TaskValidationFactPresent {
			t.Fatalf("E09 state = %q, want present (empty reason repeats by tool)", state)
		}
		ledger := NewRepeatedErrorLedger()
		repeated, skipped, err := ledger.RecordTaskSteps("q1234567", steps, repeatedErrorTestTime(10))
		if err != nil || len(repeated) != 0 || skipped != 2 {
			t.Fatalf("ledger repeated=%v skipped=%d err=%v, want none repeated and 2 skipped", repeated, skipped, err)
		}
	})
}

func TestRepeatedErrorLedgerConvertsToCheckItem(t *testing.T) {
	ledger := NewRepeatedErrorLedger()
	signature := RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "test_failed"}
	if _, err := ledger.ConvertToCheckItem(signature, repeatedErrorTestTime(12)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("convert unrecorded error returned %v, want not-found", err)
	}
	if _, err := ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: 1, At: repeatedErrorTestTime(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ConvertToCheckItem(signature, repeatedErrorTestTime(12)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("convert single occurrence returned %v, want invalid-state", err)
	}
	if _, err := ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q7654321", Sequence: 2, At: repeatedErrorTestTime(11)}); err != nil {
		t.Fatal(err)
	}
	item, err := ledger.ConvertToCheckItem(signature, repeatedErrorTestTime(12))
	if err != nil {
		t.Fatalf("convert repeated error: %v", err)
	}
	if !strings.HasPrefix(item.ID, "rec-") || len(item.ID) != 20 {
		t.Fatalf("check item id = %q, want rec- + 16 hex", item.ID)
	}
	if item.Title != "Repeated error in go_test: test_failed" || item.FirstTaskID != "q1234567" || item.Occurrences != 2 ||
		!item.CreatedAt.Equal(repeatedErrorTestTime(12)) || item.Signature != signature {
		t.Fatalf("check item = %+v, want deterministic title/task/count/time", item)
	}
	// Converting again returns the same item, not a duplicate.
	again, err := ledger.ConvertToCheckItem(signature, repeatedErrorTestTime(13))
	if err != nil || again != item {
		t.Fatalf("second convert = %+v err=%v, want the same item %+v", again, err, item)
	}
	record, ok := ledger.Find(signature)
	if !ok || record.Status != RepeatedErrorStatusConverted || record.CheckItemID != item.ID {
		t.Fatalf("converted record = %+v %t, want converted with check item id", record, ok)
	}
	if _, err := ledger.ConvertToCheckItem(signature, time.Time{}); err == nil {
		t.Fatal("convert with a zero time must fail")
	}
}

func TestRepeatedErrorLedgerModelFeedCountsAttemptsAcrossPauses(t *testing.T) {
	ledger := NewRepeatedErrorLedger()
	signature := RepeatedErrorSignature{Kind: RepeatedErrorKindModel, Tool: "executor", Reason: "rate_limited"}
	record, err := ledger.RecordModelFailure("q1234567", "executor", "rate_limited", 1, repeatedErrorTestTime(10))
	if err != nil || record.Repeated() || record.TaskAttempts("q1234567") != 1 {
		t.Fatalf("first model failure = %+v err=%v, want one attempt", record, err)
	}
	record, err = ledger.RecordModelFailure("q1234567", "executor", "rate_limited", 2, repeatedErrorTestTime(11))
	if err != nil || !record.Repeated() || record.TaskAttempts("q1234567") != 2 {
		t.Fatalf("second model failure = %+v err=%v, want two attempts (cross-pause count)", record, err)
	}
	if got := ledger.TaskAttempts("q1234567", signature); got != 2 {
		t.Fatalf("TaskAttempts() = %d, want 2", got)
	}
	if got := ledger.TaskAttempts("q7654321", signature); got != 0 {
		t.Fatalf("TaskAttempts(other task) = %d, want 0", got)
	}
	if _, err := ledger.RecordModelFailure("q1234567", "executor", "rate_limited", 0, repeatedErrorTestTime(12)); err == nil {
		t.Fatal("model failure with sequence 0 must fail")
	}
	item, err := ledger.ConvertToCheckItem(signature, repeatedErrorTestTime(12))
	if err != nil || item.Title != "Repeated model error (executor): rate_limited" {
		t.Fatalf("model check item = %+v err=%v, want the model title", item, err)
	}
}

func TestRepeatedErrorLedgerIsSafeForConcurrentUse(t *testing.T) {
	ledger := NewRepeatedErrorLedger()
	signature := RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: "go_test", Reason: "test_failed"}
	var group sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		group.Add(1)
		go func(sequence int64) {
			defer group.Done()
			if _, err := ledger.Record(signature, RepeatedErrorOccurrence{TaskID: "q1234567", Sequence: sequence, At: repeatedErrorTestTime(10)}); err != nil {
				t.Error(err)
			}
		}(int64(worker + 1))
	}
	group.Wait()
	record, ok := ledger.Find(signature)
	if !ok || len(record.Occurrences) != 10 {
		t.Fatalf("concurrent record = %+v %t, want 10 occurrences", record, ok)
	}
}
