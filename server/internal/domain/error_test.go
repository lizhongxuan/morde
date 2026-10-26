package domain

import (
	"errors"
	"testing"
)

func TestErrorAndCodeOf(t *testing.T) {
	inner := errors.New("database detail")
	err := NewError(CodeInvalidArgument, "Bad value.")
	err.Err = inner
	if got := err.Error(); got != "bad value" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(err, inner) || CodeOf(err) != CodeInvalidArgument || CodeOf(errors.New("other")) != CodeInternal {
		t.Fatal("error wrapping or code extraction failed")
	}
}

func TestInvalidArgument(t *testing.T) {
	err := InvalidArgument("name", "required")
	if err.Details["field"] != "name" || len(err.Details) != 1 {
		t.Fatalf("details = %#v", err.Details)
	}
}
