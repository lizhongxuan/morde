package postgres

import (
	"math"
	"testing"
)

func TestAddTaskCostAggregatesWithoutOverflow(t *testing.T) {
	bucket := &taskCostAccumulator{rateVersions: make(map[int64]struct{})}
	for _, value := range []struct{ reserved, actual, version int64 }{
		{reserved: 13, actual: 9, version: 2},
		{reserved: 17, actual: 11, version: 3},
		{reserved: 19, actual: 0, version: 2},
	} {
		if err := addTaskCost(bucket, value.reserved, value.actual, value.version); err != nil {
			t.Fatal(err)
		}
	}
	if bucket.calls != 3 || bucket.reservedMicros != 49 || bucket.actualMicros != 20 || len(bucket.rateVersions) != 2 {
		t.Fatalf("Task cost aggregate = %#v", bucket)
	}
	if err := addTaskCost(bucket, math.MaxInt64, 0, 4); err == nil {
		t.Fatal("overflowing Task cost aggregate was accepted")
	}
}
