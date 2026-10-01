package pkg

import (
	"errors"
	"sort"
	"testing"
)

func TestComputeDirtySetSourceFileChange(t *testing.T) {
	edges := map[string][]string{
		"//pkg:rule_a": {"//pkg:src.java"},
		"//app:binary": {"//pkg:rule_a"},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"pkg/src.java": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if result.NeedsFallback {
		t.Fatal("unexpected fallback")
	}
	if !result.DirtyLabels["//pkg:rule_a"] {
		t.Error("expected //pkg:rule_a in DirtyLabels")
	}
	if !result.DirtyLabels["//pkg:src.java"] {
		t.Error("expected //pkg:src.java in DirtyLabels")
	}
	if !result.DirtyStarLabels["//app:binary"] {
		t.Error("expected //app:binary in DirtyStarLabels (rdep of //pkg:rule_a)")
	}
}

func TestComputeDirtySetBUILDFileChange(t *testing.T) {
	edges := map[string][]string{
		"//pkg:rule_a": {"//pkg:src.java"},
		"//pkg:rule_b": {"//other:dep"},
		"//other:dep":  {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"pkg/BUILD.bazel": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if result.NeedsFallback {
		t.Fatal("unexpected fallback")
	}
	if !result.DirtyLabels["//pkg:rule_a"] {
		t.Error("expected //pkg:rule_a dirty")
	}
	if !result.DirtyLabels["//pkg:rule_b"] {
		t.Error("expected //pkg:rule_b dirty")
	}
	if result.DirtyLabels["//other:dep"] {
		t.Error("//other:dep should not be dirty")
	}
}

func TestComputeDirtySetMainRootDoesNotDirtyExternalRootLabels(t *testing.T) {
	edges := map[string][]string{
		"//:main": {"//:root.txt"},
		"@@bazel_tools+remote_coverage_tools_extension+remote_coverage_tools//:coverage_report_generator": {},
		"@bazel_tools//tools/test:coverage_report_generator": {
			"@@bazel_tools+remote_coverage_tools_extension+remote_coverage_tools//:coverage_report_generator",
		},
		"//.agents/skills/migrate-mma-dashboards:migrate-mma-test": {
			"@bazel_tools//tools/test:coverage_report_generator",
		},
	}
	allLabels := CollectAllLabels(edges, nil)

	result := ComputeDirtySet(
		map[string]string{"root.txt": "M"}, edges, allLabels, nil, nil, nil,
	)

	if !result.DirtyLabels["//:main"] {
		t.Error("expected main-workspace root target to be dirty")
	}
	for _, label := range []string{
		"@@bazel_tools+remote_coverage_tools_extension+remote_coverage_tools//:coverage_report_generator",
		"@bazel_tools//tools/test:coverage_report_generator",
		"//.agents/skills/migrate-mma-dashboards:migrate-mma-test",
	} {
		if result.DirtyStarLabels[label] {
			t.Errorf("unrelated label %s must not become dirty", label)
		}
	}
}

func TestComputeDirtySetMainPackageDoesNotDirtyExternalPackageLabels(t *testing.T) {
	edges := map[string][]string{
		"//shared:main":                  {"//shared:source.txt"},
		"@@canonical_repo//shared:other": {},
		"@apparent_repo//shared:other":   {},
	}
	allLabels := CollectAllLabels(edges, nil)

	result := ComputeDirtySet(
		map[string]string{"shared/source.txt": "M"}, edges, allLabels, nil, nil, nil,
	)

	if !result.DirtyLabels["//shared:main"] {
		t.Error("expected main-workspace target to be dirty")
	}
	for _, label := range []string{
		"@@canonical_repo//shared:other",
		"@apparent_repo//shared:other",
	} {
		if result.DirtyLabels[label] {
			t.Errorf("external label %s must not be directly dirty", label)
		}
	}
}

func TestComputeDirtySetPropagatesThroughExternalLabels(t *testing.T) {
	edges := map[string][]string{
		"//pkg:changed":           {"//pkg:source.txt"},
		"@@repo//bridge:external": {"//pkg:changed"},
		"//consumer:transitive":   {"@@repo//bridge:external"},
		"//consumer:direct":       {"//pkg:changed"},
	}
	allLabels := CollectAllLabels(edges, nil)

	result := ComputeDirtySet(
		map[string]string{"pkg/source.txt": "M"}, edges, allLabels, nil, nil, nil,
	)

	for _, label := range []string{
		"//pkg:changed",
		"@@repo//bridge:external",
		"//consumer:transitive",
		"//consumer:direct",
	} {
		if !result.DirtyStarLabels[label] {
			t.Errorf("expected %s in DirtyStarLabels", label)
		}
	}
	if result.DirtyLabels["@@repo//bridge:external"] {
		t.Error("external label should be reached by propagation, not directly dirtied")
	}
}

func TestComputeDirtySetBzlFallback(t *testing.T) {
	edges := map[string][]string{
		"//pkg:rule_a": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"tools/defs.bzl": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bzl change")
	}
	if result.FallbackReason == "" {
		t.Error("expected non-empty FallbackReason")
	}
	if result.FallbackCode != "unsafe_file_change" {
		t.Errorf("FallbackCode = %q, want unsafe_file_change", result.FallbackCode)
	}
}

func TestComputeDirtySetModuleBazelFallback(t *testing.T) {
	changedFiles := map[string]string{
		"MODULE.bazel": "M",
	}

	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for MODULE.bazel change")
	}
}

func TestComputeDirtySetSubModuleBazelFallback(t *testing.T) {
	changedFiles := map[string]string{
		"tools/modules/rules_java.MODULE.bazel": "M",
	}

	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for *.MODULE.bazel change")
	}
}

// boundarySeedEdges is a seed in which //parent:lib globs over parent/child,
// before child becomes a package of its own.
func boundarySeedEdges() map[string][]string {
	return map[string][]string{
		"//parent:lib":  {"//parent:a.java", "//parent:child/b.java"},
		"//app:bin":     {"//parent:lib"},
		"//gone:target": {"//other:x"},
		"//app:user":    {"//gone:target"},
		"//other:x":     {},
	}
}

// packageLookup is a stub packagesExistAtTarget that records its calls.
type packageLookup struct {
	existing map[string]bool
	err      error
	calls    [][]string
}

func (l *packageLookup) lookup(pkgs []string) (map[string]bool, error) {
	l.calls = append(l.calls, pkgs)
	return l.existing, l.err
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestComputeDirtySetNewPackageDirtiesEnclosingPackage(t *testing.T) {
	edges := boundarySeedEdges()
	result := ComputeDirtySet(
		map[string]string{"parent/child/BUILD.bazel": "A", "parent/child/c.java": "A"},
		edges, CollectAllLabels(edges, nil), nil, nil, nil,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//parent", "//parent/child"})
	assertStrings(t, "RemovedPackages", result.RemovedPackages, nil)
	for _, label := range []string{"//parent:lib", "//parent:child/b.java"} {
		if !result.DirtyLabels[label] {
			t.Errorf("expected %s dirty", label)
		}
	}
	if !result.DirtyStarLabels["//app:bin"] {
		t.Error("expected rdep //app:bin of the enclosing package dirty*")
	}
	if result.DirtyStarLabels["//app:user"] {
		t.Error("//app:user is unrelated and should not be dirty*")
	}
}

func TestComputeDirtySetNewPackageWithoutKnownAncestor(t *testing.T) {
	edges := boundarySeedEdges()
	result := ComputeDirtySet(
		map[string]string{"brand/new/BUILD.bazel": "A"},
		edges, CollectAllLabels(edges, nil), nil, nil, nil,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//brand/new"})
	if len(result.DirtyLabels) != 0 {
		t.Errorf("DirtyLabels = %v, want none", result.DirtyLabels)
	}
}

func TestComputeDirtySetNewRootPackage(t *testing.T) {
	edges := boundarySeedEdges()
	result := ComputeDirtySet(
		map[string]string{"BUILD.bazel": "A"},
		edges, CollectAllLabels(edges, nil), nil, nil, nil,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//"})
}

func TestComputeDirtySetNestedNewPackages(t *testing.T) {
	edges := boundarySeedEdges()
	result := ComputeDirtySet(
		map[string]string{
			"parent/child/BUILD.bazel":      "A",
			"parent/child/deep/BUILD.bazel": "A",
		},
		edges, CollectAllLabels(edges, nil), nil, nil, nil,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "DirtyPackages", result.DirtyPackages,
		[]string{"//parent", "//parent/child", "//parent/child/deep"})
}

func TestComputeDirtySetDeletedPackage(t *testing.T) {
	edges := boundarySeedEdges()
	edges["//gone:target"] = []string{"//other:x", "//gone:src.java"}
	lookup := &packageLookup{existing: map[string]bool{}}
	result := ComputeDirtySet(
		map[string]string{"gone/BUILD.bazel": "D"},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "RemovedPackages", result.RemovedPackages, []string{"//gone"})
	assertStrings(t, "DirtyPackages", result.DirtyPackages, nil)
	for _, label := range []string{"//gone:target", "//gone:src.java"} {
		if !result.DirtyLabels[label] {
			t.Errorf("expected removed label %s dirty", label)
		}
	}
	if !result.DirtyStarLabels["//app:user"] {
		t.Error("expected rdep //app:user of the removed package dirty*")
	}
	if len(lookup.calls) != 1 {
		t.Fatalf("lookup called %d times, want 1", len(lookup.calls))
	}
	assertStrings(t, "lookup pkgs", lookup.calls[0], []string{"//gone"})
}

func TestComputeDirtySetDeletedPackageDirtiesNewOwner(t *testing.T) {
	edges := boundarySeedEdges()
	edges["//parent/child:lib"] = []string{"//parent/child:b.java"}
	lookup := &packageLookup{existing: map[string]bool{}}
	result := ComputeDirtySet(
		map[string]string{"parent/child/BUILD.bazel": "D"},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "RemovedPackages", result.RemovedPackages, []string{"//parent/child"})
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//parent"})
	if !result.DirtyLabels["//parent:lib"] {
		t.Error("expected the absorbing package's //parent:lib dirty")
	}
}

func TestComputeDirtySetDeletedPackageAndParent(t *testing.T) {
	// Seed owner of //parent/child is //parent, but //parent is removed too,
	// so at the target its files belong to the root package.
	edges := boundarySeedEdges()
	edges["//parent/child:lib"] = []string{}
	edges["//:root"] = []string{}
	lookup := &packageLookup{existing: map[string]bool{}}
	result := ComputeDirtySet(
		map[string]string{
			"parent/BUILD.bazel":       "D",
			"parent/child/BUILD.bazel": "D",
		},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "RemovedPackages", result.RemovedPackages, []string{"//parent", "//parent/child"})
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//"})
	if len(lookup.calls) != 1 {
		t.Fatalf("lookup called %d times, want 1 batched call", len(lookup.calls))
	}
	assertStrings(t, "lookup pkgs", lookup.calls[0], []string{"//parent", "//parent/child"})
}

func TestComputeDirtySetDeletedBUILDWithSurvivingOtherBUILD(t *testing.T) {
	edges := boundarySeedEdges()
	lookup := &packageLookup{existing: map[string]bool{"//gone": true}}
	result := ComputeDirtySet(
		map[string]string{"gone/BUILD.bazel": "D"},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "RemovedPackages", result.RemovedPackages, nil)
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//gone"})
}

func TestComputeDirtySetDeletionResolvedFromDiff(t *testing.T) {
	edges := boundarySeedEdges()
	cases := map[string]struct {
		changed     map[string]string
		wantRemoved []string
		wantDirty   []string
	}{
		"both BUILD files deleted": {
			changed:     map[string]string{"gone/BUILD": "D", "gone/BUILD.bazel": "D"},
			wantRemoved: []string{"//gone"},
		},
		"other BUILD file added": {
			changed:   map[string]string{"gone/BUILD.bazel": "D", "gone/BUILD": "A"},
			wantDirty: []string{"//gone"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lookup := &packageLookup{}
			result := ComputeDirtySet(tc.changed, edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup)

			if result.NeedsFallback {
				t.Fatalf("unexpected fallback: %s", result.FallbackReason)
			}
			if len(lookup.calls) != 0 {
				t.Errorf("lookup called %d times, want 0", len(lookup.calls))
			}
			assertStrings(t, "RemovedPackages", result.RemovedPackages, tc.wantRemoved)
			assertStrings(t, "DirtyPackages", result.DirtyPackages, tc.wantDirty)
		})
	}
}

func TestComputeDirtySetNoLookupWithoutDeletions(t *testing.T) {
	edges := boundarySeedEdges()
	lookup := &packageLookup{}
	ComputeDirtySet(
		map[string]string{"parent/BUILD.bazel": "M", "brand/new/BUILD.bazel": "A"},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)
	if len(lookup.calls) != 0 {
		t.Errorf("lookup called %d times, want 0", len(lookup.calls))
	}
}

func TestComputeDirtySetDeletionLookupFailureFallsBack(t *testing.T) {
	edges := boundarySeedEdges()
	for name, lookup := range map[string]func([]string) (map[string]bool, error){
		"nil lookup":    nil,
		"lookup errors": (&packageLookup{err: errors.New("boom")}).lookup,
	} {
		t.Run(name, func(t *testing.T) {
			result := ComputeDirtySet(
				map[string]string{"gone/BUILD.bazel": "D"},
				edges, CollectAllLabels(edges, nil), nil, nil, lookup,
			)
			if !result.NeedsFallback {
				t.Fatal("expected fallback when deleted package existence is unknown")
			}
			if result.FallbackCode != "package_lookup_error" {
				t.Errorf("FallbackCode = %q, want package_lookup_error", result.FallbackCode)
			}
		})
	}
}

func TestComputeDirtySetPackageMove(t *testing.T) {
	edges := boundarySeedEdges()
	lookup := &packageLookup{existing: map[string]bool{}}
	result := ComputeDirtySet(
		map[string]string{
			"gone/BUILD.bazel":         "D",
			"gone/src.java":            "D",
			"parent/moved/BUILD.bazel": "A",
			"parent/moved/src.java":    "A",
		},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	assertStrings(t, "RemovedPackages", result.RemovedPackages, []string{"//gone"})
	assertStrings(t, "DirtyPackages", result.DirtyPackages, []string{"//parent", "//parent/moved"})
	if !result.DirtyStarLabels["//app:user"] {
		t.Error("expected rdep of the removed package dirty*")
	}
}

func TestComputeDirtySetBoundaryChangeWithBzlStillFallsBack(t *testing.T) {
	edges := boundarySeedEdges()
	result := ComputeDirtySet(
		map[string]string{"parent/child/BUILD.bazel": "A", "tools/defs.bzl": "M"},
		edges, CollectAllLabels(edges, nil), nil, nil, nil,
	)
	if !result.NeedsFallback || result.FallbackCode != "unsafe_file_change" {
		t.Errorf("got fallback=%v code=%q, want unsafe_file_change", result.NeedsFallback, result.FallbackCode)
	}
}

func TestPruneDirtySetPreservesRemovedPackages(t *testing.T) {
	edges := boundarySeedEdges()
	lookup := &packageLookup{existing: map[string]bool{}}
	original := ComputeDirtySet(
		map[string]string{"gone/BUILD.bazel": "D"},
		edges, CollectAllLabels(edges, nil), nil, nil, lookup.lookup,
	)
	pruned := PruneDirtySet(original, &PersistedHashData{TargetEdges: edges}, ProbeResult{}, nil)
	assertStrings(t, "RemovedPackages", pruned.RemovedPackages, []string{"//gone"})
}

func TestComputeDirtySetOwningPackageWalkUp(t *testing.T) {
	// The file lives several directories below the package's BUILD file, as
	// is typical for Maven-layout Java packages.
	edges := map[string][]string{
		"//svc:lib":    {"//svc:src/main/java/App.java"},
		"//app:binary": {"//svc:lib"},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"svc/src/main/java/App.java": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	if !result.DirtyLabels["//svc:lib"] {
		t.Error("expected //svc:lib dirty via owning-package walk-up")
	}
	if !result.DirtyStarLabels["//app:binary"] {
		t.Error("expected //app:binary in DirtyStarLabels")
	}
	if len(result.DirtyPackages) != 1 || result.DirtyPackages[0] != "//svc" {
		t.Errorf("expected DirtyPackages=[//svc], got %v", result.DirtyPackages)
	}
}

func TestComputeDirtySetUnownedFileIgnored(t *testing.T) {
	edges := map[string][]string{
		"//pkg:target": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"docs/README.md": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if result.NeedsFallback {
		t.Fatalf("unexpected fallback: %s", result.FallbackReason)
	}
	if len(result.DirtyLabels) != 0 {
		t.Errorf("expected no dirty labels for unowned file, got %v", result.DirtyLabels)
	}
	if len(result.DirtyPackages) != 0 {
		t.Errorf("expected no dirty packages for unowned file, got %v", result.DirtyPackages)
	}
}

func TestComputeDirtySetDiamondRdeps(t *testing.T) {
	// A→B, A→C, B→D, C→D. Change D → all four dirty*.
	edges := map[string][]string{
		"//pkg:A": {"//pkg:B", "//pkg:C"},
		"//pkg:B": {"//pkg:D"},
		"//pkg:C": {"//pkg:D"},
		"//pkg:D": {"//pkg:d.java"},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"pkg/d.java": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	for _, label := range []string{"//pkg:A", "//pkg:B", "//pkg:C", "//pkg:D", "//pkg:d.java"} {
		if !result.DirtyStarLabels[label] {
			t.Errorf("expected %s in DirtyStarLabels", label)
		}
	}
}

func TestComputeDirtySetFingerprintFileFallback(t *testing.T) {
	fingerprints := map[string]bool{
		"tools/modules/rules_java.MODULE.bazel": true,
	}

	changedFiles := map[string]string{
		"tools/modules/rules_java.MODULE.bazel": "M",
	}

	result := ComputeDirtySet(changedFiles, nil, nil, fingerprints, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for fingerprint file change")
	}
}

func TestComputeDirtySetBazelrcFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelrc": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bazelrc change")
	}
}

func TestComputeDirtySetBazelVersionFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelversion": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bazelversion change")
	}
}

func TestComputeDirtySetBazelIgnoreFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelignore": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bazelignore change")
	}
}

func TestComputeDirtySetRepositoryMetadataFallbacks(t *testing.T) {
	for _, path := range []string{
		"WORKSPACE",
		"WORKSPACE.bazel",
		"WORKSPACE.bzlmod",
		"MODULE.bazel.lock",
		"REPO.bazel",
		"VENDOR.bazel",
		"third_party/maven_install.json",
		"third_party/Cargo.lock",
		"web/package-lock.json",
		"python/requirements_lock.txt",
		".gitmodules",
		"config/ci.bazelrc",
		"config/.bazelrc.ci",
		"config/common.rc",
	} {
		t.Run(path, func(t *testing.T) {
			result := ComputeDirtySet(map[string]string{path: "M"}, nil, nil, nil, nil, nil)
			if !result.NeedsFallback {
				t.Fatalf("expected fallback for %s", path)
			}
		})
	}
}

func TestComputeDirtySetFallbackTriggerPattern(t *testing.T) {
	result := ComputeDirtySet(
		map[string]string{"tools/savvy/etna.yaml": "M"},
		nil, nil, nil,
		[]string{"tools/savvy/etna.yaml"},
		nil,
	)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for file matching fallback trigger pattern")
	}
	if result.FallbackCode != "unsafe_file_change" {
		t.Errorf("FallbackCode = %q, want unsafe_file_change", result.FallbackCode)
	}
}

func TestComputeDirtySetFallbackTriggerPatternGlob(t *testing.T) {
	result := ComputeDirtySet(
		map[string]string{"tools/savvy/etna.yaml": "M"},
		nil, nil, nil,
		[]string{"tools/savvy/*.yaml"},
		nil,
	)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for file matching glob pattern")
	}
}

func TestComputeDirtySetFallbackTriggerPatternNoMatch(t *testing.T) {
	edges := map[string][]string{"//pkg:rule_a": {}}
	allLabels := CollectAllLabels(edges, nil)
	result := ComputeDirtySet(
		map[string]string{"pkg/src.java": "M"},
		edges, allLabels, nil,
		[]string{"tools/savvy/*.yaml"},
		nil,
	)
	if result.NeedsFallback {
		t.Error("unexpected fallback for file not matching pattern")
	}
}

func TestBuildRdeps(t *testing.T) {
	edges := map[string][]string{
		"//a": {"//b", "//c"},
		"//b": {"//c"},
	}

	rdeps := BuildRdeps(edges)

	bRdeps := rdeps["//b"]
	sort.Strings(bRdeps)
	if len(bRdeps) != 1 || bRdeps[0] != "//a" {
		t.Errorf("rdeps of //b: want [//a], got %v", bRdeps)
	}

	cRdeps := rdeps["//c"]
	sort.Strings(cRdeps)
	if len(cRdeps) != 2 || cRdeps[0] != "//a" || cRdeps[1] != "//b" {
		t.Errorf("rdeps of //c: want [//a //b], got %v", cRdeps)
	}
}

func TestCollectAllLabels(t *testing.T) {
	edges := map[string][]string{
		"//a": {"//b"},
	}
	hashes := map[string]map[string]string{
		"//c": {"": "hash"},
	}

	all := CollectAllLabels(edges, hashes)

	for _, lbl := range []string{"//a", "//b", "//c"} {
		if !all[lbl] {
			t.Errorf("expected %s in allLabels", lbl)
		}
	}
}

func TestFileToPackage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"pkg/Foo.java", "//pkg"},
		{"a/b/c/BUILD.bazel", "//a/b/c"},
		{"BUILD.bazel", "//"},
		{"Foo.java", "//"},
	}
	for _, tt := range tests {
		got := fileToPackage(tt.path)
		if got != tt.want {
			t.Errorf("fileToPackage(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestLabelToPackage(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"//pkg:target", "//pkg"},
		{"@repo//pkg:target", "//pkg"},
		{"@@canonical_repo//pkg:target", "//pkg"},
		{"//a/b/c:d", "//a/b/c"},
		{"//pkg", "//pkg"},
	}
	for _, tt := range tests {
		got := labelToPackage(tt.label)
		if got != tt.want {
			t.Errorf("labelToPackage(%q) = %q, want %q", tt.label, got, tt.want)
		}
	}
}

func TestPruneDirtySetEliminatesUnchangedRdeps(t *testing.T) {
	// Setup: package //tools/binaries has two aliases. //app:binary depends
	// on //tools/binaries:existing_tool. A BUILD.bazel change makes both
	// aliases directly dirty, propagating to //app:binary.
	edges := map[string][]string{
		"//tools/binaries:existing_tool": {},
		"//tools/binaries:new_tool":      {},
		"//app:binary":                   {"//tools/binaries:existing_tool"},
		"//other:lib":                    {"//app:binary"},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"tools/binaries/BUILD.bazel": "M",
	}

	original := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	// Verify the unpruned dirty set cascades broadly.
	if !original.DirtyStarLabels["//app:binary"] {
		t.Fatal("expected //app:binary in unpruned DirtyStarLabels")
	}
	if !original.DirtyStarLabels["//other:lib"] {
		t.Fatal("expected //other:lib in unpruned DirtyStarLabels")
	}

	// Seed hashes: existing_tool has hash "aaaa...", new_tool is absent (new target).
	seedHashes := map[string]map[string]string{
		"//tools/binaries:existing_tool": {"": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"//app:binary":                   {"": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		"//other:lib":                    {"": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
	}

	// Probe hashes: existing_tool is UNCHANGED, new_tool is new.
	probeHashes := map[string]string{
		"//tools/binaries:existing_tool\x00": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"//tools/binaries:new_tool\x00":      "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}

	pruned := PruneDirtySet(original, &PersistedHashData{TargetHashes: seedHashes, TargetEdges: edges}, ProbeResult{Hashes: probeHashes}, nil)

	// Directly dirty labels should be preserved.
	if !pruned.DirtyStarLabels["//tools/binaries:existing_tool"] {
		t.Error("expected //tools/binaries:existing_tool in pruned DirtyStarLabels (still directly dirty)")
	}
	if !pruned.DirtyStarLabels["//tools/binaries:new_tool"] {
		t.Error("expected //tools/binaries:new_tool in pruned DirtyStarLabels (new target, actually changed)")
	}

	// The key assertion: //app:binary and //other:lib should NOT be in the
	// pruned dirty set because existing_tool's hash didn't change.
	// new_tool has no rdeps, so its change doesn't propagate.
	if pruned.DirtyStarLabels["//app:binary"] {
		t.Error("//app:binary should have been pruned — its dep //tools/binaries:existing_tool is unchanged")
	}
	if pruned.DirtyStarLabels["//other:lib"] {
		t.Error("//other:lib should have been pruned — transitive dep unchanged")
	}

	// DirtyPackages should be preserved.
	if len(pruned.DirtyPackages) != 1 || pruned.DirtyPackages[0] != "//tools/binaries" {
		t.Errorf("expected DirtyPackages=[//tools/binaries], got %v", pruned.DirtyPackages)
	}
}

func TestPruneDirtySetPreservesChangedRdeps(t *testing.T) {
	// When a target's hash actually changes, its rdeps must remain dirty.
	edges := map[string][]string{
		"//lib:changed":  {"//lib:src.java"},
		"//lib:same":     {},
		"//app:consumer": {"//lib:changed"},
	}
	allLabels := CollectAllLabels(edges, nil)

	original := ComputeDirtySet(
		map[string]string{"lib/BUILD.bazel": "M"}, edges, allLabels, nil, nil, nil,
	)

	seedHashes := map[string]map[string]string{
		"//lib:changed":  {"": "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"},
		"//lib:same":     {"": "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000"},
		"//lib:src.java": {"": "cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000"},
		"//app:consumer": {"": "dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000"},
	}

	// Probe: //lib:changed has a DIFFERENT hash, //lib:same is unchanged.
	probeHashes := map[string]string{
		"//lib:changed\x00":  "ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000",
		"//lib:same\x00":     "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000",
		"//lib:src.java\x00": "cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000",
	}

	pruned := PruneDirtySet(original, &PersistedHashData{TargetHashes: seedHashes, TargetEdges: edges}, ProbeResult{Hashes: probeHashes}, nil)

	if !pruned.DirtyStarLabels["//app:consumer"] {
		t.Error("//app:consumer must remain dirty — its dep //lib:changed has a different hash")
	}
}

func TestPruneDirtySetNoOpWhenAllChanged(t *testing.T) {
	edges := map[string][]string{
		"//pkg:a":   {},
		"//app:dep": {"//pkg:a"},
	}
	allLabels := CollectAllLabels(edges, nil)

	original := ComputeDirtySet(
		map[string]string{"pkg/BUILD.bazel": "M"}, edges, allLabels, nil, nil, nil,
	)

	seedHashes := map[string]map[string]string{
		"//pkg:a":   {"": "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"},
		"//app:dep": {"": "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000"},
	}

	// Probe: //pkg:a hash changed.
	probeHashes := map[string]string{
		"//pkg:a\x00": "ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000",
	}

	pruned := PruneDirtySet(original, &PersistedHashData{TargetHashes: seedHashes, TargetEdges: edges}, ProbeResult{Hashes: probeHashes}, nil)

	// Everything should remain dirty — same as unpruned.
	if !pruned.DirtyStarLabels["//app:dep"] {
		t.Error("//app:dep must remain dirty when //pkg:a changed")
	}
}

func TestPruneDirtySetJudgesSourceFilesByGitNotHash(t *testing.T) {
	// A source file carries no seed hash; the git diff decides. //pkg:kept.java
	// is untouched so its consumer must not propagate, while //pkg:edited.java
	// is in the diff so its consumer must.
	edges := map[string][]string{
		"//pkg:uses_kept":   {"//pkg:kept.java"},
		"//pkg:uses_edited": {"//pkg:edited.java"},
		"//app:via_kept":    {"//pkg:uses_kept"},
		"//app:via_edited":  {"//pkg:uses_edited"},
	}
	allLabels := CollectAllLabels(edges, nil)
	original := ComputeDirtySet(
		map[string]string{"pkg/edited.java": "M"}, edges, allLabels, nil, nil, nil,
	)

	// Both source files are directly dirty before pruning, and both
	// consumers are reachable from them.
	for _, label := range []string{"//app:via_kept", "//app:via_edited"} {
		if !original.DirtyStarLabels[label] {
			t.Fatalf("expected %s in unpruned DirtyStarLabels", label)
		}
	}

	// A consumer of a changed source file hashes differently, as it would
	// in reality; the consumer of the untouched one does not.
	unchanged := "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"
	differs := "ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000"
	seedHashes := map[string]map[string]string{
		"//pkg:uses_kept":   {"": unchanged},
		"//pkg:uses_edited": {"": unchanged},
	}
	probe := ProbeResult{
		Hashes: map[string]string{
			"//pkg:uses_kept\x00":   unchanged,
			"//pkg:uses_edited\x00": differs,
		},
		SourceFiles: map[string]bool{
			"//pkg:kept.java":   true,
			"//pkg:edited.java": true,
		},
	}

	pruned := PruneDirtySet(original,
		&PersistedHashData{TargetHashes: seedHashes, TargetEdges: edges},
		probe, map[string]string{"pkg/edited.java": "M"})

	if pruned.DirtyStarLabels["//app:via_kept"] {
		t.Error("//app:via_kept should be pruned: pkg/kept.java is not in the git diff")
	}
	if !pruned.DirtyStarLabels["//app:via_edited"] {
		t.Error("//app:via_edited must stay dirty: pkg/edited.java is in the git diff")
	}
}

func TestLabelToPath(t *testing.T) {
	for _, tt := range []struct{ label, want string }{
		{"//pkg:file.java", "pkg/file.java"},
		{"//pkg:src/main/java/App.java", "pkg/src/main/java/App.java"},
		{"//:root.txt", "root.txt"},
		{"//a/b/c:d.txt", "a/b/c/d.txt"},
		{"//pkg", "pkg"},
	} {
		if got := labelToPath(tt.label); got != tt.want {
			t.Errorf("labelToPath(%q) = %q, want %q", tt.label, got, tt.want)
		}
	}
}

func TestPropagateTraversesThroughUnchangedDirtyLabels(t *testing.T) {
	// //pkg:middle is dirty (its package changed) but its own hash did not
	// change. It still has to be walked through, or //app:behind is lost.
	// Membership of the result set must not double as "already traversed".
	edges := map[string][]string{
		"//pkg:changed": {"//pkg:src.java"},
		"//pkg:middle":  {"//pkg:changed"},
		"//app:behind":  {"//pkg:middle"},
	}
	allLabels := CollectAllLabels(edges, nil)
	original := ComputeDirtySet(
		map[string]string{"pkg/src.java": "M"}, edges, allLabels, nil, nil, nil,
	)

	unchanged := "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000"
	seedHashes := map[string]map[string]string{
		"//pkg:changed": {"": "cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000cccc0000"},
		"//pkg:middle":  {"": unchanged},
	}
	probe := ProbeResult{
		Hashes: map[string]string{
			"//pkg:changed\x00": "dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000dddd0000",
			"//pkg:middle\x00":  unchanged,
		},
		SourceFiles: map[string]bool{"//pkg:src.java": true},
	}

	pruned := PruneDirtySet(original,
		&PersistedHashData{TargetHashes: seedHashes, TargetEdges: edges},
		probe, map[string]string{"pkg/src.java": "M"})

	if !pruned.DirtyStarLabels["//app:behind"] {
		t.Error("//app:behind must be reached by traversing through the unchanged dirty label //pkg:middle")
	}
}

func TestPruneDirtySetReadsDependencyHashes(t *testing.T) {
	// A manual-tagged dep lives in DependencyHashes, not TargetHashes, yet
	// must still be comparable or its reverse dependencies all propagate.
	edges := map[string][]string{
		"//pkg:tool":     {},
		"//app:consumer": {"//pkg:tool"},
	}
	allLabels := CollectAllLabels(edges, nil)
	original := ComputeDirtySet(
		map[string]string{"pkg/BUILD.bazel": "M"}, edges, allLabels, nil, nil, nil,
	)
	if !original.DirtyStarLabels["//app:consumer"] {
		t.Fatal("expected //app:consumer in unpruned DirtyStarLabels")
	}

	unchanged := "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"
	seed := &PersistedHashData{
		TargetHashes:     map[string]map[string]string{},
		DependencyHashes: map[string]map[string]string{"//pkg:tool": {"": unchanged}},
		TargetEdges:      edges,
	}
	probe := ProbeResult{Hashes: map[string]string{"//pkg:tool\x00": unchanged}}

	pruned := PruneDirtySet(original, seed, probe, nil)

	if pruned.DirtyStarLabels["//app:consumer"] {
		t.Error("//app:consumer should be pruned: //pkg:tool is unchanged per DependencyHashes")
	}
}

func TestIsFallbackTriggerLockfiles(t *testing.T) {
	tests := []struct {
		basename string
		want     bool
	}{
		// Plain ".lock" extension.
		{"Cargo.lock", true},
		{"yarn.lock", true},
		{"poetry.lock", true},
		{"uv.lock", true},
		{"buf.lock", true},
		{"MODULE.bazel.lock", true},

		// ".lock" as an inner component.
		{"multitool.lock.json", true},
		{".terraform.lock.hcl", true},

		// "-lock" or "_lock" name stem.
		{"pnpm-lock.yaml", true},
		{"package-lock.json", true},
		{"requirements_lock.txt", true},

		// Extensions that could hold dependency data stay conservative, as
		// does an unrecognised one.
		{"deps-lock.xml", true},
		{"deps-lock.xyz", true},

		// Ordinary files merely named after a lock must not trigger.
		{"merge-pnpm-lock.sh", false},
		{"regen-pnpm-lock.sh", false},
		{"repin-with-lock.sh", false},
		{"2x-lock.png", false},
		{"02_add_and_lock.md", false},
		{"v3__workflow_lock.sql", false},

		// Nothing lock-like at all.
		{"README.md", false},
		{"Main.java", false},
	}

	for _, tt := range tests {
		if got := isFallbackTrigger(tt.basename); got != tt.want {
			t.Errorf("isFallbackTrigger(%q) = %v, want %v", tt.basename, got, tt.want)
		}
	}
}

func TestComputeDirtySetNestedLockExtensionFallsBack(t *testing.T) {
	changedFiles := map[string]string{
		"tools/multitool.lock.json": "M",
	}

	result := ComputeDirtySet(changedFiles, nil, nil, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for multitool.lock.json change")
	}
	if result.FallbackCode != "unsafe_file_change" {
		t.Errorf("FallbackCode = %q, want unsafe_file_change", result.FallbackCode)
	}
}

func TestComputeDirtySetLockNamedScriptDoesNotFallBack(t *testing.T) {
	edges := map[string][]string{
		"//pkg:rule_a": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"tools/git/merge-pnpm-lock.sh": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil, nil, nil)

	if result.NeedsFallback {
		t.Errorf("unexpected fallback for a shell script: %s", result.FallbackReason)
	}
}
