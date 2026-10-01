package pkg

import (
	"errors"
	"path"
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
	// DirtyPackages is the sorted list of packages, existing at the target
	// revision, whose targets must be re-listed with a package wildcard
	// (e.g. //pkg:all) rather than by explicit label, so that targets added
	// to or removed from the package are handled.
	DirtyPackages []string
	// RemovedPackages is the sorted list of packages that no longer exist at
	// the target revision. Their seed labels are in DirtyLabels, but they are
	// never in DirtyPackages: there is nothing left to list.
	RemovedPackages []string
	// NeedsFallback is true when a change requires a full rehash.
	NeedsFallback bool
	// FallbackReason describes why a fallback was triggered.
	FallbackReason string
	// FallbackCode is a stable, bounded identifier suitable for reporting and
	// metrics. FallbackReason remains the human-readable detail.
	FallbackCode string
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
// Adding or removing a package changes the hashes of only its own targets,
// the targets of its nearest enclosing package (whose globs and
// subpackages() results gain or lose the directory's files), and their
// reverse deps. Packages further up cannot see past the enclosing package.
// An added package is re-listed with a wildcard; a removed package's labels
// are dropped. In both cases the nearest enclosing package at the seed and
// at the target revision is dirtied as if its BUILD file had changed.
//
// packagesExistAtTarget reports which of the given packages still have a
// BUILD or BUILD.bazel file at the target revision. A package survives the
// deletion of one of those files if the other one remains. When the other
// file also appears in the diff, its status answers that directly; only
// the remaining deletions are looked up, all in a single call. If
// packagesExistAtTarget is nil or fails, those deletions trigger a fallback.
//
// Fallback (full rehash) is triggered by files that can change loading or
// repository resolution without appearing in target edges: Starlark,
// module/workspace/repository metadata, Bazel configuration, and rule-class
// fingerprint files.
func ComputeDirtySet(
	changedFiles map[string]string,
	edges map[string][]string,
	allLabels map[string]bool,
	ruleClassFingerprintFiles map[string]bool,
	fallbackTriggerPatterns []string,
	packagesExistAtTarget func(pkgs []string) (map[string]bool, error),
) *DirtySetResult {
	if code, reason, ok := findFallbackTrigger(changedFiles, ruleClassFingerprintFiles, fallbackTriggerPatterns); ok {
		return fallbackResult(code, reason)
	}

	knownPackages, labelsByPackage := indexSeedPackages(allLabels)
	boundaries, err := findBoundaryChanges(changedFiles, knownPackages, packagesExistAtTarget)
	if err != nil {
		return fallbackResult("package_lookup_error",
			"cannot determine whether deleted packages still exist: "+err.Error())
	}

	result := &DirtySetResult{DirtyLabels: make(map[string]bool)}
	for pkg := range findDirtyPackages(changedFiles, knownPackages, boundaries) {
		for _, label := range labelsByPackage[pkg] {
			result.DirtyLabels[label] = true
		}
		if !boundaries.removed[pkg] {
			result.DirtyPackages = append(result.DirtyPackages, pkg)
		}
	}
	sort.Strings(result.DirtyPackages)
	result.RemovedPackages = sortedKeys(boundaries.removed)
	result.DirtyStarLabels = propagateFrom(result.DirtyLabels, result.DirtyLabels, edges)
	return result
}

func fallbackResult(code, reason string) *DirtySetResult {
	return &DirtySetResult{
		DirtyLabels:     make(map[string]bool),
		DirtyStarLabels: make(map[string]bool),
		NeedsFallback:   true,
		FallbackCode:    code,
		FallbackReason:  reason,
	}
}

// findFallbackTrigger reports the first changed file that forces a full
// rehash, with its fallback code and reason.
func findFallbackTrigger(
	changedFiles map[string]string,
	ruleClassFingerprintFiles map[string]bool,
	fallbackTriggerPatterns []string,
) (code, reason string, found bool) {
	for filePath := range changedFiles {
		if ruleClassFingerprintFiles[filePath] {
			return "rule_fingerprint_change", "rule-class fingerprint file changed: " + filePath, true
		}
		for _, pattern := range fallbackTriggerPatterns {
			if matched, _ := path.Match(pattern, filePath); matched {
				return "unsafe_file_change", "file matched fallback trigger pattern " + pattern + ": " + filePath, true
			}
		}
		if isFallbackTrigger(filepath.Base(filePath)) {
			return "unsafe_file_change", "fallback trigger file changed: " + filePath, true
		}
	}
	return "", "", false
}

// indexSeedPackages returns the main-workspace packages known to the seed and
// their labels.
func indexSeedPackages(allLabels map[string]bool) (map[string]bool, map[string][]string) {
	knownPackages := make(map[string]bool)
	labelsByPackage := make(map[string][]string)
	for label := range allLabels {
		// Changed files are always in the main workspace. External labels can
		// share the same package path (including the root package) after
		// labelToPackage strips their repository prefix, but they must not be
		// selected directly by a workspace package change. Keep them in edges
		// so reverse-dependency propagation can still traverse them.
		if !strings.HasPrefix(label, "//") {
			continue
		}
		pkg := labelToPackage(label)
		knownPackages[pkg] = true
		labelsByPackage[pkg] = append(labelsByPackage[pkg], label)
	}
	return knownPackages, labelsByPackage
}

// boundaryChanges are the packages that appear or disappear between the seed
// and the target revision.
type boundaryChanges struct {
	added   map[string]bool
	removed map[string]bool
}

// findBoundaryChanges classifies changed BUILD files into added and removed
// packages. It returns an error only when packagesExistAtTarget is needed and
// is nil or fails.
func findBoundaryChanges(
	changedFiles map[string]string,
	knownPackages map[string]bool,
	packagesExistAtTarget func(pkgs []string) (map[string]bool, error),
) (boundaryChanges, error) {
	boundaries := boundaryChanges{added: make(map[string]bool), removed: make(map[string]bool)}
	// Packages whose BUILD file was deleted but whose survival the diff
	// alone cannot tell; resolved below with packagesExistAtTarget.
	deletionsNeedingLookup := make(map[string]bool)

	for filePath, status := range changedFiles {
		if !isBuildFile(filepath.Base(filePath)) {
			continue
		}
		pkg := fileToPackage(filePath)
		if status != "D" {
			if !knownPackages[pkg] {
				boundaries.added[pkg] = true
			}
			continue
		}
		// A directory is a package while it has a BUILD or a BUILD.bazel
		// file, so deleting one only removes the package if the other is
		// gone too. A directory with both practically never happens (Bazel
		// would read BUILD.bazel and ignore BUILD); this exists so that
		// repositories using either name are handled correctly, and the
		// typical case is simply "the package's only BUILD file was
		// deleted". If the other file is in the diff, its status decides:
		// deleted means the package is removed; added or modified means it
		// survives and is handled like any edited BUILD file. If it is not
		// in the diff, it either still exists unchanged or never existed,
		// and only the target revision can tell which.
		otherStatus, otherInDiff := changedFiles[otherBuildFile(filePath)]
		switch {
		case !otherInDiff:
			deletionsNeedingLookup[pkg] = true
		case otherStatus == "D":
			boundaries.removed[pkg] = true
		}
	}

	if len(deletionsNeedingLookup) == 0 {
		return boundaries, nil
	}
	if packagesExistAtTarget == nil {
		return boundaryChanges{}, errNoPackageLookup
	}
	pkgs := sortedKeys(deletionsNeedingLookup)
	existing, err := packagesExistAtTarget(pkgs)
	if err != nil {
		return boundaryChanges{}, err
	}
	for _, pkg := range pkgs {
		if !existing[pkg] {
			boundaries.removed[pkg] = true
		}
	}
	return boundaries, nil
}

// findDirtyPackages returns every package whose targets must be rehashed:
// packages of changed files, the added and removed packages themselves, and
// the nearest enclosing package of each. The result includes removed
// packages, whose seed labels must be invalidated.
func findDirtyPackages(
	changedFiles map[string]string,
	knownPackages map[string]bool,
	boundaries boundaryChanges,
) map[string]bool {
	targetPackages := make(map[string]bool, len(knownPackages)+len(boundaries.added))
	for pkg := range knownPackages {
		if !boundaries.removed[pkg] {
			targetPackages[pkg] = true
		}
	}
	for pkg := range boundaries.added {
		targetPackages[pkg] = true
	}

	dirty := make(map[string]bool)

	// A package boundary change dirties the nearest enclosing package on both
	// sides of the change. They differ only when several boundaries change
	// in one diff, e.g. a package and its parent are both removed.
	for _, changed := range []map[string]bool{boundaries.added, boundaries.removed} {
		for pkg := range changed {
			dirty[pkg] = true
			if pkg == "//" {
				continue
			}
			dir := strings.TrimPrefix(pkg, "//")
			if owner, ok := owningPackage(dir, knownPackages); ok {
				dirty[owner] = true
			}
			if owner, ok := owningPackage(dir, targetPackages); ok {
				dirty[owner] = true
			}
		}
	}

	for filePath := range changedFiles {
		if isBuildFile(filepath.Base(filePath)) {
			// A BUILD file change dirties its own package.
			dirty[fileToPackage(filePath)] = true
			continue
		}
		// A source file belongs to the nearest enclosing known package. With
		// none, it cannot be an input to any seeded target (see
		// ComputeDirtySet).
		if owner, ok := owningPackage(filePath, knownPackages); ok {
			dirty[owner] = true
		}
	}
	return dirty
}

// ProbeResult is what a probe of the dirty packages learned about them.
type ProbeResult struct {
	// Hashes maps "label\x00configuration" to the hex hash computed at the
	// destination revision.
	Hashes map[string]string
	// SourceFiles is the set of probed labels that are source files. Their
	// dirtiness comes from the git diff rather than from Hashes.
	SourceFiles map[string]bool
}

// PruneDirtySet narrows a DirtySetResult by propagating reverse
// dependencies only from targets that actually changed, rather than from
// every target in a dirty package. A BUILD.bazel edit marks its whole
// package dirty, so in a high-fanout package the unpruned rdeps closure can
// cover most of the repository even when only one target really changed.
//
// A source file is unchanged exactly when the git diff does not mention it.
// Every other label is unchanged when its probe hash matches the seed;
// labels the probe or the seed omits are conservatively treated as changed.
//
// DirtyLabels, DirtyPackages and RemovedPackages are preserved — those
// packages still need re-listing via wildcards or dropping — and only
// DirtyStarLabels is recomputed.
func PruneDirtySet(
	original *DirtySetResult,
	seed *PersistedHashData,
	probe ProbeResult,
	changedFiles map[string]string,
) *DirtySetResult {
	actuallyChanged := findChangedTargets(original.DirtyLabels, seed, probe, changedFiles)
	newDirtyStar := propagateFrom(original.DirtyLabels, actuallyChanged, seed.TargetEdges)

	return &DirtySetResult{
		DirtyLabels:     original.DirtyLabels,
		DirtyStarLabels: newDirtyStar,
		DirtyPackages:   original.DirtyPackages,
		RemovedPackages: original.RemovedPackages,
	}
}

// findChangedTargets returns the subset of dirtyLabels that changed between
// the seed and the destination revision.
func findChangedTargets(
	dirtyLabels map[string]bool,
	seed *PersistedHashData,
	probe ProbeResult,
	changedFiles map[string]string,
) map[string]bool {
	changed := make(map[string]bool)
	for label := range dirtyLabels {
		if probe.SourceFiles[label] {
			if _, ok := changedFiles[labelToPath(label)]; ok {
				changed[label] = true
			}
			continue
		}
		if targetHashChanged(label, seed.SeedHashes(label), probe.Hashes) {
			changed[label] = true
		}
	}
	return changed
}

// labelToPath returns the workspace-relative path a main-repo label refers
// to, e.g. "//pkg:sub/f.java" becomes "pkg/sub/f.java".
func labelToPath(label string) string {
	if idx := strings.Index(label, "//"); idx >= 0 {
		label = label[idx+len("//"):]
	}
	pkg, name, found := strings.Cut(label, ":")
	switch {
	case !found:
		return pkg
	case pkg == "":
		return name
	default:
		return pkg + "/" + name
	}
}

// targetHashChanged reports whether a single target's probe hash differs
// from its seed hash. Returns true for new targets (nil seedConfigs) and
// targets missing from probe results.
func targetHashChanged(label string, seedConfigs map[string]string, probeHashes map[string]string) bool {
	if seedConfigs == nil {
		return true
	}
	for config, seedHex := range seedConfigs {
		probeHex, ok := probeHashes[label+"\x00"+config]
		if !ok || probeHex != seedHex {
			return true
		}
	}
	return false
}

// propagateFrom builds DirtyStarLabels by including all directly dirty
// labels and BFS-propagating rdeps only from the actuallyChanged subset.
func propagateFrom(dirtyLabels, actuallyChanged map[string]bool, edges map[string][]string) map[string]bool {
	rdeps := BuildRdeps(edges)

	// Every directly dirty label is dirty* regardless of whether it
	// changed, but membership of the result must not be mistaken for
	// having been traversed: an unchanged dirty label still has to be
	// walked through to reach what lies behind it. Hence a separate
	// visited set from the result set.
	result := make(map[string]bool, len(dirtyLabels))
	for label := range dirtyLabels {
		result[label] = true
	}

	visited := make(map[string]bool, len(actuallyChanged))
	queue := make([]string, 0, len(actuallyChanged))
	for label := range actuallyChanged {
		if !visited[label] {
			visited[label] = true
			queue = append(queue, label)
		}
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, rdep := range rdeps[current] {
			result[rdep] = true
			if !visited[rdep] {
				visited[rdep] = true
				queue = append(queue, rdep)
			}
		}
	}
	return result
}

// nonLockfileExtensions are extensions a dependency lockfile never uses.
// Lockfile detection matches on name fragments, so without this a script,
// document or migration merely named after a lock forces a full rehash.
//
// Keep this list to executable code, images and prose. Anything that could
// hold structured dependency data must stay off it, however unlikely it
// looks: pip writes requirements_lock.txt, so ".txt" belongs to lockfiles,
// and ".xml" is omitted for the same reason. Wrongly listing an extension
// here silently under-reports impacted targets, which is far worse than the
// redundant rehash a missing entry causes.
var nonLockfileExtensions = map[string]bool{
	".md": true, ".rst": true, ".html": true,
	".png": true, ".jpg": true, ".jpeg": true, ".svg": true,
	".sh": true, ".bash": true,
	".go": true, ".java": true, ".py": true, ".ts": true, ".js": true,
	".sql": true,
}

// looksLikeLockfile reports whether lowerBasename, already lowercased, names a
// dependency lockfile. Three shapes occur: a ".lock" extension (Cargo.lock,
// buf.lock), ".lock" as an inner component (multitool.lock.json,
// .terraform.lock.hcl), and a "-lock" or "_lock" name stem (pnpm-lock.yaml,
// package-lock.json). Only the first is unambiguous; the other two also match
// ordinary files that happen to be named after a lock, so they additionally
// require an extension a lockfile could plausibly use.
func looksLikeLockfile(lowerBasename string) bool {
	if strings.HasSuffix(lowerBasename, ".lock") {
		return true
	}
	if !strings.Contains(lowerBasename, ".lock.") &&
		!strings.Contains(lowerBasename, "-lock.") &&
		!strings.Contains(lowerBasename, "_lock.") {
		return false
	}
	return !nonLockfileExtensions[filepath.Ext(lowerBasename)]
}

func isFallbackTrigger(basename string) bool {
	if strings.HasSuffix(basename, ".bzl") {
		return true
	}
	lowerBasename := strings.ToLower(basename)
	if looksLikeLockfile(lowerBasename) {
		return true
	}
	if basename == ".bazelrc" || strings.HasPrefix(basename, ".bazelrc.") ||
		strings.HasSuffix(basename, ".bazelrc") || strings.HasSuffix(basename, ".rc") {
		return true
	}
	switch basename {
	case "MODULE.bazel", "MODULE.bazel.lock",
		"WORKSPACE", "WORKSPACE.bazel", "WORKSPACE.bzlmod",
		"REPO.bazel", "VENDOR.bazel",
		"maven_install.json",
		".bazelversion", ".bazelignore", ".gitmodules":
		return true
	}
	if strings.HasSuffix(basename, ".MODULE.bazel") {
		return true
	}
	return false
}

var errNoPackageLookup = errors.New("no package lookup available")

func isBuildFile(basename string) bool {
	return basename == "BUILD" || basename == "BUILD.bazel"
}

// otherBuildFile returns the path of the other BUILD file name in the same
// directory: "pkg/BUILD.bazel" for "pkg/BUILD", and vice versa. Bazel treats
// either name as the package's BUILD file.
func otherBuildFile(filePath string) string {
	other := "BUILD"
	if filepath.Base(filePath) == "BUILD" {
		other = "BUILD.bazel"
	}
	return filepath.Join(filepath.Dir(filePath), other)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
