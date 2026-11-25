package task

import (
	"reflect"
	"testing"
)

func testImpactGraph() *DependencyGraph {
	graph := NewDependencyGraph()
	// a.go is imported by b.go and c.go; c.go is imported by d.go.
	graph.AddFile("a.go", nil)
	graph.AddFile("b.go", []string{"a.go"})
	graph.AddFile("c.go", []string{"a.go"})
	graph.AddFile("d.go", []string{"c.go"})
	graph.AddFile("e.go", nil)
	graph.AddCoverage("a.go", "TestA")
	graph.AddCoverage("b.go", "TestB")
	graph.AddCoverage("c.go", "TestC")
	graph.AddCoverage("d.go", "TestD")
	return graph
}

func TestAnalyzeImpact(t *testing.T) {
	graph := testImpactGraph()
	cases := []struct {
		name    string
		changed []string
		want    ImpactMap
	}{
		{
			name:    "leaf change lists transitive callers and their tests",
			changed: []string{"a.go"},
			want:    ImpactMap{ChangedFiles: []string{"a.go"}, Callers: []string{"b.go", "c.go", "d.go"}, CoveringTests: []string{"TestA", "TestB", "TestC", "TestD"}},
		},
		{
			name:    "middle change lists only downstream callers",
			changed: []string{"c.go"},
			want:    ImpactMap{ChangedFiles: []string{"c.go"}, Callers: []string{"d.go"}, CoveringTests: []string{"TestC", "TestD"}},
		},
		{
			name:    "isolated file has no callers",
			changed: []string{"e.go"},
			want:    ImpactMap{ChangedFiles: []string{"e.go"}, Callers: []string{}, CoveringTests: []string{}},
		},
		{
			name:    "changed file is not listed as its own caller",
			changed: []string{"a.go", "b.go"},
			want:    ImpactMap{ChangedFiles: []string{"a.go", "b.go"}, Callers: []string{"c.go", "d.go"}, CoveringTests: []string{"TestA", "TestB", "TestC", "TestD"}},
		},
		{
			name:    "unknown file is listed with no callers or tests",
			changed: []string{"missing.go"},
			want:    ImpactMap{ChangedFiles: []string{"missing.go"}, Callers: []string{}, CoveringTests: []string{}},
		},
		{
			name:    "duplicated and unsorted input is normalized",
			changed: []string{"c.go", "a.go", "c.go"},
			want:    ImpactMap{ChangedFiles: []string{"a.go", "c.go"}, Callers: []string{"b.go", "d.go"}, CoveringTests: []string{"TestA", "TestB", "TestC", "TestD"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := graph.AnalyzeImpact(tc.changed)
			if err != nil {
				t.Fatalf("AnalyzeImpact() returned unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("AnalyzeImpact() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAnalyzeImpactErrors(t *testing.T) {
	graph := testImpactGraph()
	if _, err := graph.AnalyzeImpact(nil); err == nil {
		t.Fatal("AnalyzeImpact with no changed files must fail")
	}
	if _, err := graph.AnalyzeImpact([]string{"/abs.go"}); err == nil {
		t.Fatal("AnalyzeImpact with an absolute path must fail")
	}
	if _, err := graph.AnalyzeImpact([]string{"../escape.go"}); err == nil {
		t.Fatal("AnalyzeImpact with a parent traversal must fail")
	}
	if _, err := (*DependencyGraph)(nil).AnalyzeImpact([]string{"a.go"}); err == nil {
		t.Fatal("AnalyzeImpact on a nil graph must fail")
	}
}

func TestAnalyzeImpactDoesNotMutateGraph(t *testing.T) {
	graph := testImpactGraph()
	before, err := graph.AnalyzeImpact([]string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := graph.AnalyzeImpact([]string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("repeated AnalyzeImpact calls must return the same result")
	}
	if got := graph.CoveringTestsOf("a.go"); !reflect.DeepEqual(got, []string{"TestA"}) {
		t.Fatalf("CoveringTestsOf() = %v, want [TestA]", got)
	}
}

func TestCheckImpactCoverage(t *testing.T) {
	graph := testImpactGraph()
	impact, err := graph.AnalyzeImpact([]string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		tested []string
		want   []string
	}{
		{name: "covering test ran", tested: []string{"TestA"}, want: nil},
		{name: "only a caller test ran, not the file's own", tested: []string{"TestB"}, want: []string{"a.go"}},
		{name: "no test ran", tested: nil, want: []string{"a.go"}},
		{name: "only other files tests ran", tested: []string{"TestC", "TestD"}, want: []string{"a.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := graph.CheckImpactCoverage(impact, tc.tested); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CheckImpactCoverage() = %v, want %v", got, tc.want)
			}
		})
	}
	// A changed file with no covering tests on record is uncovered even when
	// other tests ran: the regression step must not silently pass it.
	unknown, err := graph.AnalyzeImpact([]string{"missing.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.CheckImpactCoverage(unknown, []string{"TestA"}); !reflect.DeepEqual(got, []string{"missing.go"}) {
		t.Fatalf("CheckImpactCoverage() for an unknown file = %v, want [missing.go]", got)
	}
}

func TestFilesOutsideImpact(t *testing.T) {
	impact := &ImpactMap{ChangedFiles: []string{"a.go"}, Callers: []string{"b.go", "c.go"}, CoveringTests: []string{"TestA"}}
	if got := FilesOutsideImpact(impact, []string{"a.go", "b.go"}); len(got) != 0 {
		t.Fatalf("FilesOutsideImpact() = %v, want empty", got)
	}
	if got := FilesOutsideImpact(impact, []string{"a.go", "d.go", "e.go"}); !reflect.DeepEqual(got, []string{"d.go", "e.go"}) {
		t.Fatalf("FilesOutsideImpact() = %v, want [d.go e.go]", got)
	}
	if got := FilesOutsideImpact(nil, []string{"a.go"}); len(got) != 0 {
		t.Fatalf("FilesOutsideImpact() without a recorded impact = %v, want empty", got)
	}
}

func TestImpactMapNormalized(t *testing.T) {
	impact := &ImpactMap{ChangedFiles: []string{"b.go", "a.go", "b.go"}, Callers: []string{"d.go", "c.go", "d.go"}, CoveringTests: []string{"TestB", "TestA", "TestB"}}
	want := &ImpactMap{ChangedFiles: []string{"a.go", "b.go"}, Callers: []string{"c.go", "d.go"}, CoveringTests: []string{"TestA", "TestB"}}
	if got := impact.Normalized(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalized() = %+v, want %+v", got, want)
	}
	var nilImpact *ImpactMap
	if nilImpact.Normalized() != nil {
		t.Fatal("Normalized() on nil must return nil")
	}
}

func TestImpactMapCoverageNormalized(t *testing.T) {
	impact := &ImpactMap{
		ChangedFiles:  []string{"b.go", "a.go"},
		Callers:       []string{"c.go"},
		CoveringTests: []string{"TestB", "TestA"},
		Coverage: map[string][]string{
			"b.go":        {"TestB", "TestA", "TestB"},
			"a.go":        {"TestA"},
			"outside.go":  {"TestA"},
			"c.go":        {},
			"server/x.go": {"TestA"},
		},
	}
	want := &ImpactMap{
		ChangedFiles:  []string{"a.go", "b.go"},
		Callers:       []string{"c.go"},
		CoveringTests: []string{"TestA", "TestB"},
		Coverage: map[string][]string{
			"a.go": {"TestA"},
			"b.go": {"TestA", "TestB"},
		},
	}
	if got := impact.Normalized(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalized() = %+v, want %+v", got, want)
	}
}
