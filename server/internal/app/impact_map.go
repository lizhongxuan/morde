// Impact mapping for the "fix a core component" flow (E02).
//
// AnalyzeImpactMap builds the impact surface of one workspace component from
// text and import references: possible dependencies and callers, plus
// candidate tests. The mapping is
// heuristic, not an exact call graph: imports are matched textually against
// workspace paths, callers are found by import or by the appearance of the
// component's exported symbol names, and test candidates are found by file
// location or reference. It does not prove a test ran or covered a file.
//
// The analysis never touches the DSH runtime or the database. It only reads
// through the FileIndex supplied by the caller; production callers adapt a
// workspace snapshot to that interface, tests use an in-memory fake.
package app

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"morde/server/internal/domain"
)

// FileIndex is the read-only view of a workspace that impact analysis needs:
// the file list plus file contents by workspace-relative path. It deliberately
// has no write operation: impact mapping is a read-only analysis.
type FileIndex interface {
	Files() []string
	ReadFile(path string) (string, error)
}

// ImpactMap is a heuristic impact surface. CoveringTests is retained as a
// field name for compatibility, but it lists candidates, not proven coverage.
// Every list is sorted and deduplicated; empty lists are empty, never nil.
type ImpactMap struct {
	Component     string   `json:"component"`
	Dependencies  []string `json:"dependencies"`
	Callers       []string `json:"callers"`
	CoveringTests []string `json:"covering_tests"`
}

// AnalyzeImpactMap returns the impact surface of componentPath in the
// workspace described by files. An empty workspace, or a component with no
// references, yields empty lists without an error. A path outside the
// workspace, or a component that is not in the workspace, is an error. Files
// whose contents cannot be read are skipped, except for the component itself,
// whose unreadable contents are an error.
func AnalyzeImpactMap(files FileIndex, componentPath string) (ImpactMap, error) {
	result := ImpactMap{Component: componentPath, Dependencies: []string{}, Callers: []string{}, CoveringTests: []string{}}
	if files == nil {
		return ImpactMap{}, domain.InvalidArgument("impact_map.files", "impact map file index is required")
	}
	if err := validateCloudDSHWorkspacePath(componentPath); err != nil {
		return ImpactMap{}, domain.InvalidArgument("impact_map.component", "impact map component path is outside the workspace")
	}
	all := sortedUnique(files.Files())
	if len(all) == 0 {
		return result, nil
	}
	present := false
	for _, file := range all {
		if file == componentPath {
			present = true
			break
		}
	}
	if !present {
		return ImpactMap{}, domain.NewError(domain.CodeNotFound, "impact map component is not in the workspace")
	}
	content, err := files.ReadFile(componentPath)
	if err != nil {
		return ImpactMap{}, fmt.Errorf("read impact map component %s: %w", componentPath, err)
	}
	known := make(map[string]bool, len(all))
	for _, file := range all {
		known[file] = true
	}
	directories := make(map[string]bool, len(all))
	for _, file := range all {
		directories[path.Dir(file)] = true
	}
	result.Dependencies = sortedUnique(resolveImports(known, directories, componentPath, parseImports(componentPath, content)))
	symbols := exportedSymbols(componentPath, content)
	componentDir := path.Dir(componentPath)
	for _, file := range all {
		if file == componentPath || !isImpactCodeFile(file) {
			continue
		}
		if isImpactTestFile(file) && path.Dir(file) == componentDir {
			result.CoveringTests = append(result.CoveringTests, file)
			continue
		}
		fileContent, readErr := files.ReadFile(file)
		if readErr != nil {
			continue
		}
		if !referencesComponent(known, directories, file, fileContent, componentPath, symbols) {
			continue
		}
		if isImpactTestFile(file) {
			result.CoveringTests = append(result.CoveringTests, file)
		} else {
			result.Callers = append(result.Callers, file)
		}
	}
	result.Callers = sortedUnique(result.Callers)
	result.CoveringTests = sortedUnique(result.CoveringTests)
	return result, nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func isImpactCodeFile(file string) bool {
	switch path.Ext(file) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return true
	default:
		return false
	}
}

var impactTestFilePattern = regexp.MustCompile(`\.(test|spec)\.(ts|tsx|js|jsx|mjs|cjs)$`)

func isImpactTestFile(file string) bool {
	return strings.HasSuffix(file, "_test.go") || impactTestFilePattern.MatchString(file)
}

// referencesComponent reports whether the file imports the component's
// package or mentions one of the component's exported symbol names.
func referencesComponent(known, directories map[string]bool, file, content, componentPath string, symbols []string) bool {
	for _, resolved := range resolveImports(known, directories, file, parseImports(file, content)) {
		if resolved == componentPath {
			return true
		}
	}
	for _, symbol := range symbols {
		if impactContainsWord(content, symbol) {
			return true
		}
	}
	return false
}

func impactContainsWord(content, word string) bool {
	for index := 0; index < len(content); {
		found := strings.Index(content[index:], word)
		if found < 0 {
			return false
		}
		start := index + found
		end := start + len(word)
		beforeOK := start == 0 || !isImpactWordChar(content[start-1])
		afterOK := end == len(content) || !isImpactWordChar(content[end])
		if beforeOK && afterOK {
			return true
		}
		index = end
	}
	return false
}

func isImpactWordChar(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func parseImports(filePath, content string) []string {
	switch path.Ext(filePath) {
	case ".go":
		return parseGoImports(content)
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return parseScriptImports(content)
	default:
		return nil
	}
}

func resolveImports(known, directories map[string]bool, filePath string, imports []string) []string {
	var resolved []string
	for _, imp := range imports {
		switch path.Ext(filePath) {
		case ".go":
			resolved = append(resolved, resolveGoImport(known, directories, filePath, imp)...)
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
			resolved = append(resolved, resolveScriptImport(known, filePath, imp)...)
		}
	}
	return resolved
}

var goImportStringPattern = regexp.MustCompile(`"([^"]+)"`)

// parseGoImports extracts import paths from single-line and block imports.
// Aliased, blank, and dot imports are all read the same way: only the quoted
// path matters for mapping.
func parseGoImports(content string) []string {
	var imports []string
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if index := strings.Index(trimmed, "//"); index >= 0 {
			trimmed = strings.TrimSpace(trimmed[:index])
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				continue
			}
			if match := goImportStringPattern.FindStringSubmatch(trimmed); match != nil {
				imports = append(imports, match[1])
			}
			continue
		}
		if strings.HasPrefix(trimmed, "import (") {
			inBlock = true
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "import ("))
			if rest != "" && rest != ")" {
				if match := goImportStringPattern.FindStringSubmatch(rest); match != nil {
					imports = append(imports, match[1])
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "import ") {
			if match := goImportStringPattern.FindStringSubmatch(trimmed); match != nil {
				imports = append(imports, match[1])
			}
		}
	}
	return imports
}

var (
	scriptFromPattern    = regexp.MustCompile(`from\s+['"]([^'"]+)['"]`)
	scriptRequirePattern = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)
	scriptSidePattern    = regexp.MustCompile(`(?m)^\s*import\s+['"]([^'"]+)['"]`)
	scriptDynamicPattern = regexp.MustCompile(`import\(\s*['"]([^'"]+)['"]\s*\)`)
)

// parseScriptImports extracts module specifiers from from-imports,
// side-effect imports, dynamic imports, and require calls.
func parseScriptImports(content string) []string {
	var imports []string
	for _, pattern := range []*regexp.Regexp{scriptFromPattern, scriptRequirePattern, scriptSidePattern, scriptDynamicPattern} {
		for _, match := range pattern.FindAllStringSubmatch(content, -1) {
			imports = append(imports, match[1])
		}
	}
	return imports
}

// resolveGoImport maps a Go import path to the non-test Go files of the
// matching workspace directory. A relative import resolves against the
// importing file's directory; otherwise the longest workspace directory that
// equals the import path or forms its tail wins, so a module-prefixed import
// such as morde/server/internal/store still maps onto internal/store.
// Import paths with no matching directory, such as the standard library, are
// external and resolve to nothing.
func resolveGoImport(known, directories map[string]bool, filePath, imp string) []string {
	if strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../") {
		return goFilesInDir(known, path.Clean(path.Join(path.Dir(filePath), imp)))
	}
	best := ""
	for dir := range directories {
		if imp == dir || strings.HasSuffix(imp, "/"+dir) {
			if len(dir) > len(best) {
				best = dir
			}
		}
	}
	if best == "" {
		return nil
	}
	return goFilesInDir(known, best)
}

func goFilesInDir(known map[string]bool, dir string) []string {
	var files []string
	for file := range known {
		if path.Dir(file) == dir && strings.HasSuffix(file, ".go") && !strings.HasSuffix(file, "_test.go") {
			files = append(files, file)
		}
	}
	return files
}

// resolveScriptImport maps a relative or workspace-rooted specifier to the
// file it names, trying the usual extension and index fallbacks. Bare
// specifiers are package imports, not workspace files, and resolve to
// nothing.
func resolveScriptImport(known map[string]bool, filePath, imp string) []string {
	var base string
	switch {
	case strings.HasPrefix(imp, "/"):
		base = strings.TrimPrefix(imp, "/")
	case strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../"):
		base = path.Clean(path.Join(path.Dir(filePath), imp))
	default:
		return nil
	}
	if known[base] {
		return []string{base}
	}
	if path.Ext(base) != "" {
		return nil
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		if known[base+ext] {
			return []string{base + ext}
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx"} {
		if known[base+"/index"+ext] {
			return []string{base + "/index" + ext}
		}
	}
	return nil
}

func exportedSymbols(filePath, content string) []string {
	switch path.Ext(filePath) {
	case ".go":
		return exportedGoSymbols(content)
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return exportedScriptSymbols(content)
	default:
		return nil
	}
}

var (
	goFuncDeclPattern   = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Z][A-Za-z0-9_]*)\s*(?:\[[^\]]*\]\s*)?\(`)
	goSingleDeclPattern = regexp.MustCompile(`^(type|var|const)\s+([A-Z][A-Za-z0-9_]*)\b`)
	goBlockDeclPattern  = regexp.MustCompile(`^(type|var|const)\s*\(\s*$`)
	goBlockNamePattern  = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*)\b`)
)

// exportedGoSymbols lists the exported top-level names of a Go file:
// functions and methods, single type/var/const declarations, and the names
// inside type/var/const blocks.
func exportedGoSymbols(content string) []string {
	var symbols []string
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			symbols = append(symbols, name)
		}
	}
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				continue
			}
			if match := goBlockNamePattern.FindStringSubmatch(line); match != nil {
				add(match[1])
			}
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if match := goFuncDeclPattern.FindStringSubmatch(trimmed); match != nil {
			add(match[1])
			continue
		}
		if match := goBlockDeclPattern.FindStringSubmatch(trimmed); match != nil {
			inBlock = true
			continue
		}
		if match := goSingleDeclPattern.FindStringSubmatch(trimmed); match != nil {
			add(match[2])
		}
	}
	return symbols
}

var (
	scriptExportDeclPattern = regexp.MustCompile(`(?m)export\s+(?:default\s+)?(?:async\s+function|function|class|abstract\s+class|const|let|var|interface|type|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	scriptExportListPattern = regexp.MustCompile(`(?m)export\s*\{([^}]*)\}`)
)

// exportedScriptSymbols lists the names a TS/JS module exports, including
// the exported name of aliased list entries (export { Foo as Bar } exports
// Bar).
func exportedScriptSymbols(content string) []string {
	var symbols []string
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" && name != "default" && !seen[name] {
			seen[name] = true
			symbols = append(symbols, name)
		}
	}
	for _, match := range scriptExportDeclPattern.FindAllStringSubmatch(content, -1) {
		add(match[1])
	}
	for _, match := range scriptExportListPattern.FindAllStringSubmatch(content, -1) {
		for _, item := range strings.Split(match[1], ",") {
			item = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), "type "))
			if index := strings.Index(item, " as "); index >= 0 {
				item = strings.TrimSpace(item[index+len(" as "):])
			}
			add(item)
		}
	}
	return symbols
}
