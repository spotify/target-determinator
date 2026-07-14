package pkg

import (
	"path/filepath"
	"sort"
	"strings"
)

// DirtySetResult contains the computed dirty set and metadata.
type DirtySetResult struct {
	// DirtyLabels is the set of target labels directly affected by changes.
	DirtyLabels map[string]bool
	// DirtyStarLabels is DirtyLabels plus all transitive reverse deps.
	DirtyStarLabels map[string]bool
	// DirtyPackages is the sorted list of packages containing changed files
	// (source or BUILD). Targets in these packages must be re-listed with a
	// package wildcard (e.g. //pkg:all) rather than by explicit label, so
	// that targets added to or removed from the package are handled.
	DirtyPackages []string
	// NeedsFallback is true when a change requires a full rehash.
	NeedsFallback bool
	// FallbackReason describes why a fallback was triggered.
	FallbackReason string
}

// ComputeDirtySet determines which targets need rehashing based on changed
// files and the persisted edge map.
//
// changedFiles maps file paths (relative to workspace root) to their git
// diff status code (M, A, D, R, etc.).
// edges maps target labels to their direct dependency labels (from the seed
// file's TargetEdges).
// allLabels is the complete set of labels known to the seed file (union of
// TargetEdges keys and values, plus TargetHashes keys). Used both to find
// labels in dirty packages and to derive the set of known packages.
// ruleClassFingerprintFiles is the set of workspace-relative paths used for
// rule-class fingerprints; a change to any of these triggers a fallback.
//
// A changed source file is attributed to its owning package by walking up
// the directory tree to the nearest package known to the seed. This mirrors
// how Bazel assigns files to packages: globs cannot cross package
// boundaries, and a label //pkg:path/file requires pkg to be a package, so
// the nearest enclosing package is the only one whose targets can reference
// the file. Files with no enclosing known package cannot be inputs to any
// seeded target and are ignored.
//
// Fallback (full rehash) is triggered by: .bzl files, MODULE.bazel and
// *.MODULE.bazel, .bazelrc, .bazelversion, .bazelignore, rule-class
// fingerprint files, deleted or renamed BUILD files (deleted/renamed
// packages), and BUILD files for packages unknown to the seed (new
// packages, which may also re-partition the parent package's files).
func ComputeDirtySet(
	changedFiles map[string]string,
	edges map[string][]string,
	allLabels map[string]bool,
	ruleClassFingerprintFiles map[string]bool,
) *DirtySetResult {
	result := &DirtySetResult{
		DirtyLabels:     make(map[string]bool),
		DirtyStarLabels: make(map[string]bool),
	}

	knownPackages := make(map[string]bool)
	for label := range allLabels {
		knownPackages[labelToPackage(label)] = true
	}

	// First pass: fallback triggers.
	for filePath, status := range changedFiles {
		base := filepath.Base(filePath)

		if ruleClassFingerprintFiles[filePath] {
			result.NeedsFallback = true
			result.FallbackReason = "rule-class fingerprint file changed: " + filePath
			return result
		}

		if isFallbackTrigger(base) {
			result.NeedsFallback = true
			result.FallbackReason = "fallback trigger file changed: " + filePath
			return result
		}

		if base == "BUILD" || base == "BUILD.bazel" {
			if status == "D" || strings.HasPrefix(status, "R") {
				result.NeedsFallback = true
				result.FallbackReason = "package deleted or renamed: " + filePath
				return result
			}
			if !knownPackages[fileToPackage(filePath)] {
				result.NeedsFallback = true
				result.FallbackReason = "BUILD file for package unknown to seed: " + filePath
				return result
			}
		}
	}

	// Second pass: map changed files to dirty packages and labels.
	dirtyPackages := make(map[string]bool)
	for filePath := range changedFiles {
		base := filepath.Base(filePath)

		var pkg string
		if base == "BUILD" || base == "BUILD.bazel" {
			// A BUILD file change dirties its own package (verified known above).
			pkg = fileToPackage(filePath)
		} else {
			// A source file belongs to the nearest enclosing known package.
			owner, ok := owningPackage(filePath, knownPackages)
			if !ok {
				// No enclosing package: the file cannot be an input to any
				// seeded target (see function comment).
				continue
			}
			pkg = owner
		}

		if !dirtyPackages[pkg] {
			dirtyPackages[pkg] = true
			for label := range allLabels {
				if labelInPackage(label, pkg) {
					result.DirtyLabels[label] = true
				}
			}
		}
	}

	for pkg := range dirtyPackages {
		result.DirtyPackages = append(result.DirtyPackages, pkg)
	}
	sort.Strings(result.DirtyPackages)

	// Propagate dirtiness through reverse deps.
	rdeps := BuildRdeps(edges)

	queue := make([]string, 0, len(result.DirtyLabels))
	for label := range result.DirtyLabels {
		result.DirtyStarLabels[label] = true
		queue = append(queue, label)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, rdep := range rdeps[current] {
			if !result.DirtyStarLabels[rdep] {
				result.DirtyStarLabels[rdep] = true
				queue = append(queue, rdep)
			}
		}
	}

	return result
}

func isFallbackTrigger(basename string) bool {
	if strings.HasSuffix(basename, ".bzl") {
		return true
	}
	switch basename {
	case "MODULE.bazel", ".bazelrc", ".bazelversion", ".bazelignore":
		return true
	}
	if strings.HasSuffix(basename, ".MODULE.bazel") {
		return true
	}
	return false
}

func fileToPackage(filePath string) string {
	dir := filepath.Dir(filePath)
	if dir == "." {
		return "//"
	}
	return "//" + filepath.ToSlash(dir)
}

// owningPackage walks up the directory tree from filePath and returns the
// nearest enclosing package present in knownPackages.
func owningPackage(filePath string, knownPackages map[string]bool) (string, bool) {
	dir := filepath.ToSlash(filepath.Dir(filePath))
	for {
		var pkg string
		if dir == "." || dir == "/" || dir == "" {
			pkg = "//"
		} else {
			pkg = "//" + dir
		}
		if knownPackages[pkg] {
			return pkg, true
		}
		if pkg == "//" {
			return "", false
		}
		parent := filepath.ToSlash(filepath.Dir(dir))
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func labelInPackage(label string, pkg string) bool {
	return labelToPackage(label) == pkg
}

// LabelPackage returns the package portion of a label, e.g. "//foo/bar" for
// "//foo/bar:baz". Repository prefixes are stripped.
func LabelPackage(label string) string {
	return labelToPackage(label)
}

func labelToPackage(label string) string {
	if idx := strings.Index(label, "//"); idx >= 0 {
		label = label[idx:]
	}
	if idx := strings.IndexByte(label, ':'); idx >= 0 {
		return label[:idx]
	}
	return label
}

// BuildRdeps constructs a reverse-dependency map from an edge map.
func BuildRdeps(edges map[string][]string) map[string][]string {
	rdeps := make(map[string][]string)
	for label, deps := range edges {
		for _, dep := range deps {
			rdeps[dep] = append(rdeps[dep], label)
		}
	}
	for dep := range rdeps {
		sort.Strings(rdeps[dep])
	}
	return rdeps
}

// CollectAllLabels builds the complete set of labels from the edge map
// (both keys and values) and the hash map keys.
func CollectAllLabels(edges map[string][]string, hashLabels map[string]map[string]string) map[string]bool {
	all := make(map[string]bool)
	for lbl := range edges {
		all[lbl] = true
		for _, dep := range edges[lbl] {
			all[dep] = true
		}
	}
	for lbl := range hashLabels {
		all[lbl] = true
	}
	return all
}
