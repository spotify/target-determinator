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
	// NeedsFallback is true when a change requires full rehash.
	NeedsFallback bool
	// FallbackReason describes why a fallback was triggered.
	FallbackReason string
	// DeletedPackages lists packages whose BUILD files were deleted.
	DeletedPackages []string
}

// ComputeDirtySet determines which targets need rehashing based on changed
// files and the persisted edge map.
//
// changedFiles maps file paths (relative to workspace root) to their git
// diff status code (M, A, D, R, etc.).
// edges maps target labels to their direct dependency labels (from the seed
// file's TargetEdges).
// allLabels is the complete set of labels known to the seed file (union of
// TargetEdges keys and values, plus TargetHashes keys). Used to find source
// file labels in packages with changed files.
// ruleClassFingerprintFiles is the set of workspace-relative paths used for
// rule-class fingerprints; a change to any of these triggers a fallback.
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

	for filePath := range changedFiles {
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
	}

	for filePath, status := range changedFiles {
		base := filepath.Base(filePath)

		if (base == "BUILD" || base == "BUILD.bazel") && (status == "D" || status == "R") {
			pkg := fileToPackage(filePath)
			result.DeletedPackages = append(result.DeletedPackages, pkg)
		}

		pkg := fileToPackage(filePath)
		for label := range allLabels {
			if labelInPackage(label, pkg) {
				result.DirtyLabels[label] = true
			}
		}
	}

	sort.Strings(result.DeletedPackages)

	rdeps := BuildRdeps(edges)

	for label := range result.DirtyLabels {
		result.DirtyStarLabels[label] = true
	}

	queue := make([]string, 0, len(result.DirtyLabels))
	for label := range result.DirtyLabels {
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

	for _, pkg := range result.DeletedPackages {
		for label := range allLabels {
			if labelInPackage(label, pkg) {
				result.DirtyStarLabels[label] = true
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

func labelInPackage(label string, pkg string) bool {
	return labelToPackage(label) == pkg
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
