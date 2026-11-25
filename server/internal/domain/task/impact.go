package task

import (
	"errors"
	"sort"
)

// DependencyGraph is the explicit dependency graph an impact analysis runs
// over: for each file, the files it imports, and the tests that cover it.
// The graph is explicit input, not a scan: the agent declares the edges it
// read from the code (a future step can build the same graph from
// `go list` output). AnalyzeImpact never mutates the graph, so the same
// graph can be queried read-only as often as needed.
//
// The graph answers the two questions a regression check needs: which files
// call into a changed file (transitive reverse dependencies), and which
// tests cover a changed file.
type DependencyGraph struct {
	imports  map[string][]string
	coverage map[string][]string
}

// NewDependencyGraph returns an empty graph.
func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{imports: make(map[string][]string), coverage: make(map[string][]string)}
}

// AddFile records that file imports the given files. Import lists are
// copied; later changes to the caller's slice do not affect the graph.
func (g *DependencyGraph) AddFile(file string, imports []string) {
	if g == nil || !validImpactPath(file) {
		return
	}
	seen := make(map[string]struct{}, len(imports))
	kept := make([]string, 0, len(imports))
	for _, imp := range imports {
		if !validImpactPath(imp) || imp == file {
			continue
		}
		if _, exists := seen[imp]; exists {
			continue
		}
		seen[imp] = struct{}{}
		kept = append(kept, imp)
	}
	sort.Strings(kept)
	g.imports[file] = kept
}

// AddCoverage records that the given tests cover file.
func (g *DependencyGraph) AddCoverage(file string, tests ...string) {
	if g == nil || !validImpactPath(file) {
		return
	}
	seen := make(map[string]struct{}, len(g.coverage[file])+len(tests))
	kept := append([]string(nil), g.coverage[file]...)
	for _, test := range kept {
		seen[test] = struct{}{}
	}
	for _, test := range tests {
		if test == "" || len(test) > 256 {
			continue
		}
		if _, exists := seen[test]; exists {
			continue
		}
		seen[test] = struct{}{}
		kept = append(kept, test)
	}
	sort.Strings(kept)
	g.coverage[file] = kept
}

// AnalyzeImpact maps a change to its impact surface: the changed files
// themselves, every file that (transitively) depends on them, and the tests
// covering any of those files. Unknown files (not in the graph) are still
// listed as changed, with no callers and no covering tests: the analysis
// never pretends to know more than the graph contains.
func (g *DependencyGraph) AnalyzeImpact(changed []string) (ImpactMap, error) {
	if g == nil {
		return ImpactMap{}, errors.New("impact analysis requires a dependency graph")
	}
	changed = dedupeImpactPaths(changed)
	if len(changed) == 0 {
		return ImpactMap{}, errors.New("impact analysis requires at least one changed file")
	}
	for _, file := range changed {
		if !validImpactPath(file) {
			return ImpactMap{}, errors.New("impact analysis changed file is invalid")
		}
	}
	reverse := make(map[string][]string, len(g.imports))
	for file, imports := range g.imports {
		for _, imp := range imports {
			reverse[imp] = append(reverse[imp], file)
		}
	}
	callers := make(map[string]struct{})
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, caller := range reverse[current] {
			if _, seen := callers[caller]; seen {
				continue
			}
			callers[caller] = struct{}{}
			queue = append(queue, caller)
		}
	}
	for _, file := range changed {
		delete(callers, file)
	}
	tests := make(map[string]struct{})
	for _, file := range changed {
		for _, test := range g.coverage[file] {
			tests[test] = struct{}{}
		}
	}
	for caller := range callers {
		for _, test := range g.coverage[caller] {
			tests[test] = struct{}{}
		}
	}
	return ImpactMap{
		ChangedFiles:  changed,
		Callers:       sortedKeys(callers),
		CoveringTests: sortedKeys(tests),
	}, nil
}

// ImpactOf is the read-only impact query: the same analysis as
// AnalyzeImpact, exposed for callers that only need to read the impact
// surface without recording anything.
func (g *DependencyGraph) ImpactOf(changed []string) (ImpactMap, error) {
	return g.AnalyzeImpact(changed)
}

// CoveringTestsOf returns the tests covering one file, if the graph knows
// the file.
func (g *DependencyGraph) CoveringTestsOf(file string) []string {
	if g == nil {
		return nil
	}
	return append([]string(nil), g.coverage[file]...)
}

// CheckImpactCoverage returns the changed files whose covering tests did not
// run: a changed file is covered only when at least one of its covering
// tests is in tested. A changed file the graph does not know (no covering
// tests on record) is reported as uncovered — the regression step must not
// silently pass a file with no known coverage.
func (g *DependencyGraph) CheckImpactCoverage(impact ImpactMap, tested []string) []string {
	ran := make(map[string]struct{}, len(tested))
	for _, test := range tested {
		ran[test] = struct{}{}
	}
	var uncovered []string
	for _, file := range impact.ChangedFiles {
		covering := g.coverage[file]
		covered := false
		for _, test := range covering {
			if _, ok := ran[test]; ok {
				covered = true
				break
			}
		}
		if !covered {
			uncovered = append(uncovered, file)
		}
	}
	return uncovered
}

// FilesOutsideImpact returns the files in files that are not part of the
// recorded impact surface (neither changed nor callers). It is the check the
// minimal-diff gate runs against.
func FilesOutsideImpact(impact *ImpactMap, files []string) []string {
	if impact == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(impact.ChangedFiles)+len(impact.Callers))
	for _, file := range impact.ChangedFiles {
		allowed[file] = struct{}{}
	}
	for _, file := range impact.Callers {
		allowed[file] = struct{}{}
	}
	var outside []string
	for _, file := range files {
		if _, ok := allowed[file]; !ok {
			outside = append(outside, file)
		}
	}
	return outside
}

// Normalized returns a copy of the impact map with sorted, deduplicated
// lists, so regression comparisons are deterministic. Per-file coverage
// is normalized the same way, and entries for files outside the recorded
// impact (neither changed nor a caller) are dropped: coverage for a file
// the impact does not name cannot be checked against it.
func (m *ImpactMap) Normalized() *ImpactMap {
	if m == nil {
		return nil
	}
	changed := dedupeImpactPaths(m.ChangedFiles)
	callers := dedupeImpactPaths(m.Callers)
	inImpact := make(map[string]struct{}, len(changed)+len(callers))
	for _, file := range changed {
		inImpact[file] = struct{}{}
	}
	for _, file := range callers {
		inImpact[file] = struct{}{}
	}
	var coverage map[string][]string
	for file, tests := range m.Coverage {
		if _, ok := inImpact[file]; !ok {
			continue
		}
		kept := dedupeImpactPaths(tests)
		if len(kept) == 0 {
			continue
		}
		if coverage == nil {
			coverage = make(map[string][]string, len(m.Coverage))
		}
		coverage[file] = kept
	}
	return &ImpactMap{
		ChangedFiles:  changed,
		Callers:       callers,
		CoveringTests: dedupeImpactPaths(m.CoveringTests),
		Coverage:      coverage,
	}
}

func dedupeImpactPaths(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		kept = append(kept, value)
	}
	sort.Strings(kept)
	return kept
}

func sortedKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
