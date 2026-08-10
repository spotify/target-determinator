package pkg

import (
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

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

func TestComputeDirtySetBzlFallback(t *testing.T) {
	edges := map[string][]string{
		"//pkg:rule_a": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"tools/defs.bzl": "M",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

	result := ComputeDirtySet(changedFiles, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for MODULE.bazel change")
	}
}

func TestComputeDirtySetSubModuleBazelFallback(t *testing.T) {
	changedFiles := map[string]string{
		"tools/modules/rules_java.MODULE.bazel": "M",
	}

	result := ComputeDirtySet(changedFiles, nil, nil, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for *.MODULE.bazel change")
	}
}

func TestComputeDirtySetDeletedBUILDFallsBack(t *testing.T) {
	edges := map[string][]string{
		"//deleted_pkg:target": {"//lib:dep"},
		"//app:binary":         {"//deleted_pkg:target"},
		"//lib:dep":            {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"deleted_pkg/BUILD.bazel": "D",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for deleted BUILD file")
	}
	if result.FallbackCode != "package_boundary_change" {
		t.Errorf("FallbackCode = %q, want package_boundary_change", result.FallbackCode)
	}
}

func TestComputeDirtySetRenamedBUILDFallsBack(t *testing.T) {
	edges := map[string][]string{
		"//pkg:target": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"pkg/BUILD.bazel": "R100",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for renamed BUILD file")
	}
}

func TestComputeDirtySetNewPackageFallsBack(t *testing.T) {
	edges := map[string][]string{
		"//pkg:target": {},
	}
	allLabels := CollectAllLabels(edges, nil)

	changedFiles := map[string]string{
		"newpkg/BUILD.bazel": "A",
	}

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for BUILD file of package unknown to seed")
	}
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

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

	result := ComputeDirtySet(changedFiles, edges, allLabels, nil)

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

	result := ComputeDirtySet(changedFiles, nil, nil, fingerprints)

	if !result.NeedsFallback {
		t.Fatal("expected fallback for fingerprint file change")
	}
}

func TestComputeDirtySetBazelrcFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelrc": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bazelrc change")
	}
}

func TestComputeDirtySetBazelVersionFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelversion": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil)
	if !result.NeedsFallback {
		t.Fatal("expected fallback for .bazelversion change")
	}
}

func TestComputeDirtySetBazelIgnoreFallback(t *testing.T) {
	changedFiles := map[string]string{".bazelignore": "M"}
	result := ComputeDirtySet(changedFiles, nil, nil, nil)
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
			result := ComputeDirtySet(map[string]string{path: "M"}, nil, nil, nil)
			if !result.NeedsFallback {
				t.Fatalf("expected fallback for %s", path)
			}
		})
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
