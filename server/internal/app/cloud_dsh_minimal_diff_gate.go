package app

import (
	"path"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// normalizeMinimalDiffPath normalizes a workspace-relative path for allow-set
// comparison: backslashes become slashes, leading slashes and "." segments
// are dropped, and redundant separators and ".." segments resolve. Both the
// fix plan entries and the written paths go through it, so "auth/session.go",
// "./auth/session.go" and "/auth/session.go" name the same file and a
// differently spelled path cannot slip past the allow set.
func normalizeMinimalDiffPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.TrimLeft(value, "/")
	value = path.Clean(value)
	value = strings.TrimLeft(value, "/")
	if value == "." {
		return ""
	}
	return value
}

// minimalDiffOutOfScopeFiles returns the normalized changed paths that are
// not in the fix plan allow set, in first-seen order without duplicates. It
// returns nil for tasks without a fix plan, so those tasks are never
// affected, and for an empty changed list. An empty changed_files list allows
// no write at all: every changed path is out of scope.
func minimalDiffOutOfScopeFiles(plan *task.Plan, changed []string) []string {
	if plan == nil || plan.FixPlan == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(plan.FixPlan.ChangedFiles))
	for _, file := range plan.FixPlan.ChangedFiles {
		if normalized := normalizeMinimalDiffPath(file); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	var violations []string
	seen := make(map[string]struct{})
	for _, file := range changed {
		normalized := normalizeMinimalDiffPath(file)
		if normalized == "" {
			continue
		}
		if _, ok := allowed[normalized]; ok {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		violations = append(violations, normalized)
	}
	return violations
}

// requireMinimalDiffWrite is the write gate (E06): a task whose plan carries
// a fix plan may only write files listed in fix_plan.changed_files, the
// minimal change set the agent declared in its fix proposal. An empty
// changed_files list allows no write at all: declaring "no file changes" and
// then writing a file is out of scope, and the agent recovers by adding the
// file to the fix plan. Tasks without a fix plan are unaffected, so ordinary
// writes and feature work are never blocked by this gate. The error tells the
// agent what to do next; it must not surface as a user question.
func requireMinimalDiffWrite(details TaskDetails, writePath string) error {
	if details.Plan == nil || details.Plan.FixPlan == nil {
		return nil
	}
	if violations := minimalDiffOutOfScopeFiles(details.Plan, []string{writePath}); len(violations) > 0 {
		return domain.InvalidArgument("task_plan.changed_files", "file "+violations[0]+" is not in fix_plan.changed_files; add the file to the fix plan before writing it")
	}
	return nil
}

// agentFileEvidencePaths extracts the written paths from file write evidence.
func agentFileEvidencePaths(evidence []AgentFileEvidence) []string {
	paths := make([]string, 0, len(evidence))
	for _, item := range evidence {
		paths = append(paths, item.Path)
	}
	return paths
}

// verifiedArtifactPaths extracts the written paths from verified artifacts.
func verifiedArtifactPaths(artifacts []AgentVerifiedArtifact) []string {
	paths := make([]string, 0, len(artifacts))
	for _, item := range artifacts {
		paths = append(paths, item.Path)
	}
	return paths
}
