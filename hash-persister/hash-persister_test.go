package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/target-determinator/pkg"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestValidateSeed(t *testing.T) {
	valid := &pkg.PersistedHashData{
		FormatVersion:                pkg.CurrentPersistedHashFormatVersion,
		SeedCompatibilityFingerprint: "fingerprint",
		GitCommitSha:                 "seed-sha",
		TargetEdges:                  map[string][]string{},
		TargetHashes: map[string]map[string]string{
			"//pkg:target": {"": strings.Repeat("01", 32)},
		},
	}
	if reason := validateSeed(valid, "seed-sha", "fingerprint"); reason != nil {
		t.Fatalf("valid seed rejected: %s", reason.Detail)
	}

	tests := map[string]struct {
		code   string
		mutate func(*pkg.PersistedHashData)
	}{
		"format":      {"seed_format_incompatible", func(data *pkg.PersistedHashData) { data.FormatVersion-- }},
		"commit":      {"seed_commit_mismatch", func(data *pkg.PersistedHashData) { data.GitCommitSha = "other" }},
		"fingerprint": {"seed_compatibility_mismatch", func(data *pkg.PersistedHashData) { data.SeedCompatibilityFingerprint = "other" }},
		"edges":       {"seed_missing_edges", func(data *pkg.PersistedHashData) { data.TargetEdges = nil }},
		"hash encoding": {"seed_hash_invalid", func(data *pkg.PersistedHashData) {
			data.TargetHashes = map[string]map[string]string{"//pkg:target": {"": "not-hex"}}
		}},
		"hash length": {"seed_hash_invalid", func(data *pkg.PersistedHashData) {
			data.TargetHashes = map[string]map[string]string{"//pkg:target": {"": "01"}}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			changed := *valid
			test.mutate(&changed)
			reason := validateSeed(&changed, "seed-sha", "fingerprint")
			if reason == nil {
				t.Fatal("incompatible seed was accepted")
			}
			if reason.Code != test.code {
				t.Fatalf("fallback code = %q, want %q", reason.Code, test.code)
			}
		})
	}
}

func TestWriteExecutionReport(t *testing.T) {
	path := t.TempDir() + "/report.json"
	want := &executionReport{
		SchemaVersion:         1,
		RequestedMode:         "incremental",
		EffectiveMode:         "full",
		Status:                "success",
		FallbackCode:          "unsafe_file_change",
		RecomputedTargetCount: 42,
	}
	if err := writeExecutionReport(path, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got executionReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != *want {
		t.Fatalf("execution report = %#v, want %#v", got, *want)
	}
}

func TestShouldFallbackForRecomputation(t *testing.T) {
	tests := map[string]struct {
		recomputed int
		total      int
		threshold  int
		want       bool
	}{
		"below threshold":    {recomputed: 69, total: 100, threshold: 70, want: false},
		"at threshold":       {recomputed: 70, total: 100, threshold: 70, want: true},
		"above threshold":    {recomputed: 71, total: 100, threshold: 70, want: true},
		"custom threshold":   {recomputed: 50, total: 100, threshold: 50, want: true},
		"fractional below":   {recomputed: 6, total: 9, threshold: 70, want: false},
		"fractional at":      {recomputed: 7, total: 10, threshold: 70, want: true},
		"empty seed":         {recomputed: 0, total: 0, threshold: 70, want: false},
		"no dirty targets":   {recomputed: 0, total: 100, threshold: 70, want: false},
		"dirty exceeds seed": {recomputed: 2, total: 1, threshold: 70, want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := shouldFallbackForRecomputation(test.recomputed, test.total, test.threshold); got != test.want {
				t.Fatalf("shouldFallbackForRecomputation(%d, %d, %d) = %v, want %v",
					test.recomputed, test.total, test.threshold, got, test.want)
			}
		})
	}
}

func TestValidateRecomputationFallbackPercent(t *testing.T) {
	for _, percent := range []int{1, 70, 100} {
		if err := validateRecomputationFallbackPercent(percent); err != nil {
			t.Errorf("valid percentage %d rejected: %v", percent, err)
		}
	}
	for _, percent := range []int{-1, 0, 101} {
		if err := validateRecomputationFallbackPercent(percent); err == nil {
			t.Errorf("invalid percentage %d accepted", percent)
		}
	}
}

func TestHighRecomputationFallbackIsReported(t *testing.T) {
	if highRecomputationFallbackCode != "high_recomputation_ratio" {
		t.Fatalf("fallback code = %q, want stable metric value", highRecomputationFallbackCode)
	}
	report := &executionReport{RequestedMode: "incremental", EffectiveMode: "incremental"}
	applySeededOutcome(report, seededOutcome{
		FallbackCode:          highRecomputationFallbackCode,
		FallbackDetail:        "dirty set includes 70 of 100 seeded targets",
		RecomputedTargetCount: 100,
		TotalTargetCount:      100,
	})
	if report.EffectiveMode != "full" || report.FallbackCode != highRecomputationFallbackCode {
		t.Fatalf("fallback report = %#v", report)
	}
}

func TestCountDirtySeedTargetsIgnoresNonPersistedLabels(t *testing.T) {
	targetHashes := map[string]map[string]string{
		"//app:binary":  nil,
		"//lib:library": nil,
	}
	dirtyLabels := map[string]bool{
		"//app:binary":    true,
		"//app:source.go": true,
	}
	if got := countDirtySeedTargets(targetHashes, dirtyLabels); got != 1 {
		t.Fatalf("countDirtySeedTargets() = %d, want 1", got)
	}
}

func TestParseGitNameStatusPreservesWhitespace(t *testing.T) {
	got, err := parseGitNameStatus([]byte("M\x00pkg/file name.txt\x00A\x00nested dir/new file.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if got["pkg/file name.txt"] != "M" || got["nested dir/new file.txt"] != "A" {
		t.Fatalf("unexpected parsed statuses: %#v", got)
	}
}

func TestParseGitNameStatusRejectsMalformedOutput(t *testing.T) {
	if _, err := parseGitNameStatus([]byte("M\x00path\x00A")); err == nil {
		t.Fatal("expected malformed output error")
	}
}

func TestValidateIncrementalBackend(t *testing.T) {
	tests := map[string]struct {
		seedableOutput bool
		seededInput    bool
		queryBackend   string
		wantError      bool
	}{
		"full compact cquery":        {queryBackend: "cquery"},
		"full seedable cquery":       {seedableOutput: true, queryBackend: "cquery", wantError: true},
		"incremental compact cquery": {seededInput: true, queryBackend: "cquery", wantError: true},
		"incremental seedable cquery": {
			seedableOutput: true,
			seededInput:    true,
			queryBackend:   "cquery",
			wantError:      true,
		},
		"full compact query":        {queryBackend: "query"},
		"full seedable query":       {seedableOutput: true, queryBackend: "query"},
		"incremental compact query": {seededInput: true, queryBackend: "query"},
		"incremental seedable query": {
			seedableOutput: true,
			seededInput:    true,
			queryBackend:   "query",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateIncrementalBackend(test.seedableOutput, test.seededInput, test.queryBackend)
			if (err != nil) != test.wantError {
				t.Fatalf("validateIncrementalBackend() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestNewPersistedOutputIncludesSeedMetadataOnlyWhenRequested(t *testing.T) {
	hashes := map[string]map[string]string{"//pkg:target": {"": "hash"}}
	edges := map[string][]string{"//pkg:target": {"//pkg:source"}}

	for _, test := range []struct {
		name             string
		seededInput      bool
		seedableOutput   bool
		wantSeedMetadata bool
	}{
		{name: "full compact"},
		{name: "full seedable", seedableOutput: true, wantSeedMetadata: true},
		{name: "incremental compact", seededInput: true},
		{name: "incremental seedable", seededInput: true, seedableOutput: true, wantSeedMetadata: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config{
				Context:        &pkg.Context{WorkspacePath: "/workspace"},
				CommitSha:      "new-sha",
				Targets:        pkg.TargetsList{},
				SeedableOutput: test.seedableOutput,
			}
			if test.seededInput {
				cfg.SeedFile = "seed.json"
			}
			got := newPersistedOutput(cfg, "release 8", hashes, edges, "fingerprint")
			hasSeedMetadata := got.FormatVersion != 0 || got.TargetEdges != nil || got.SeedCompatibilityFingerprint != ""
			if hasSeedMetadata != test.wantSeedMetadata {
				t.Fatalf("seed metadata present = %v, want %v: %#v", hasSeedMetadata, test.wantSeedMetadata, got)
			}
		})
	}
}

func TestMergePersistedEntriesReplacesDirtyState(t *testing.T) {
	dirty := map[string]bool{
		"//pkg:changed":      true,
		"//pkg:deleted":      true,
		"//pkg:removed_edge": true,
	}

	seedHashes := map[string]map[string]string{
		"//pkg:clean":   {"": "clean-old"},
		"//pkg:changed": {"": "changed-old"},
		"//pkg:deleted": {"": "deleted-old"},
	}
	freshHashes := map[string]map[string]string{
		"//pkg:changed": {"": "changed-new"},
		"//pkg:added":   {"": "added-new"},
	}
	wantHashes := map[string]map[string]string{
		"//pkg:clean":   {"": "clean-old"},
		"//pkg:changed": {"": "changed-new"},
		"//pkg:added":   {"": "added-new"},
	}
	if got := mergePersistedEntries(seedHashes, dirty, freshHashes); !reflect.DeepEqual(got, wantHashes) {
		t.Fatalf("merged hashes = %#v, want %#v", got, wantHashes)
	}

	seedEdges := map[string][]string{
		"//pkg:clean":        {"//dep:clean"},
		"//pkg:changed":      {"//dep:old"},
		"//pkg:deleted":      {"//dep:deleted"},
		"//pkg:removed_edge": {"//dep:removed"},
	}
	freshEdges := map[string][]string{
		"//pkg:changed": {"//dep:new"},
		"//pkg:added":   {"//dep:added"},
	}
	wantEdges := map[string][]string{
		"//pkg:clean":   {"//dep:clean"},
		"//pkg:changed": {"//dep:new"},
		"//pkg:added":   {"//dep:added"},
	}
	if got := mergePersistedEntries(seedEdges, dirty, freshEdges); !reflect.DeepEqual(got, wantEdges) {
		t.Fatalf("merged edges = %#v, want %#v", got, wantEdges)
	}
}

func TestBuildProbePattern(t *testing.T) {
	// ":*" rather than ":all" so source files are covered, and no targets
	// pattern wrapper so manual-tagged targets are not filtered out.
	for _, tc := range []struct {
		name     string
		packages []string
		want     string
	}{
		{"empty", nil, "set()"},
		{"root", []string{"//"}, `("//:*")`},
		{"punctuation", []string{"//pkg+other=one"}, `("//pkg+other=one:*")`},
		{"quote", []string{`//say"hi`}, `('//say"hi:*')`},
		{"several", []string{"//", "//ci", "//tools/binaries"}, `("//:*" + "//ci:*" + "//tools/binaries:*")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildProbePattern(tc.packages)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("buildProbePattern(%v) = %q, want %q", tc.packages, got, tc.want)
			}
		})
	}
}

func TestCarriedScopedLabelsSkipsDirtyAndRemovedPackages(t *testing.T) {
	dirty := &pkg.DirtySetResult{
		DirtyStarLabels: map[string]bool{
			"//dirty:a":   true,
			"//gone:b":    true,
			"//carried:c": true,
			"//carried:d": true,
		},
		DirtyPackages:   []string{"//dirty"},
		RemovedPackages: []string{"//gone"},
	}
	seedTargetHashes := map[string]map[string]string{
		"//dirty:a":   {"": "00"},
		"//gone:b":    {"": "00"},
		"//carried:c": {"": "00"},
	}

	got := carriedScopedLabels(dirty, seedTargetHashes)

	if !reflect.DeepEqual(got, []string{"//carried:c"}) {
		t.Errorf("carriedScopedLabels = %v, want [//carried:c]", got)
	}
}

func TestGitPackagesExist(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, f := range []string{"BUILD.bazel", "a/b/BUILD.bazel", "c/BUILD", "d/src.java"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "init")
	sha := git("rev-parse", "HEAD")

	got, err := gitPackagesExist(dir, sha, []string{"//", "//a/b", "//c", "//d", "//missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"//": true, "//a/b": true, "//c": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gitPackagesExist = %v, want %v", got, want)
	}

	if _, err := gitPackagesExist(dir, "0000000000000000000000000000000000000000", []string{"//a/b"}); err == nil {
		t.Error("expected an error for an unknown commit")
	}
}

func TestBuildProbePatternUnquotableWord(t *testing.T) {
	got, err := buildProbePattern([]string{`//both'"`})
	if err == nil || got != "" {
		t.Fatalf("unquotable word returned pattern %q, error %v", got, err)
	}
}

func TestValidateSeedHashes(t *testing.T) {
	for _, dependency := range []bool{false, true} {
		for _, hash := range []string{strings.Repeat("0a", 32), strings.Repeat("AF", 32), "", "0", "00", strings.Repeat("00", 33), strings.Repeat("gg", 32)} {
			t.Run(fmt.Sprintf("dependency_%t/hash_%s", dependency, hash), func(t *testing.T) {
				data := &pkg.PersistedHashData{TargetHashes: map[string]map[string]string{}}
				hashes := map[string]map[string]string{"//pkg:target": {"cfg1": strings.Repeat("01", 32), "cfg2": hash}}
				if dependency {
					data.DependencyHashes = hashes
				} else {
					data.TargetHashes = hashes
				}
				wantValid := hash == strings.Repeat("0a", 32) || hash == strings.Repeat("AF", 32)
				if err := validateSeedHashes(data); (err == nil) != wantValid {
					t.Fatalf("validation error = %v, want valid = %t", err, wantValid)
				}
				if _, err := reusableSeedHashes(data, nil); (err == nil) != wantValid {
					t.Fatalf("seeding error = %v, want valid = %t", err, wantValid)
				}
			})
		}
	}
	if err := validateSeedHashes(&pkg.PersistedHashData{}); err != nil {
		t.Fatal(err)
	}
}

func TestReusableSeedHashesCopiesHashesAndSkipsDirtyLabels(t *testing.T) {
	data := &pkg.PersistedHashData{
		TargetHashes: map[string]map[string]string{
			"//pkg:clean": {"cfg1": strings.Repeat("01", 32), "cfg2": strings.Repeat("02", 32)},
			"//pkg:dirty": {"cfg": "invalid"},
		},
		DependencyHashes: map[string]map[string]string{
			"//dep:clean": {"cfg": strings.Repeat("03", 32)},
			"//dep:dirty": {"cfg": "invalid"},
		},
	}
	hashes, err := reusableSeedHashes(data, map[string]bool{"//pkg:dirty": true, "//dep:dirty": true})
	require.NoError(t, err)
	want := map[string][]byte{
		"//pkg:clean\x00cfg1": []byte(strings.Repeat("\x01", 32)),
		"//pkg:clean\x00cfg2": []byte(strings.Repeat("\x02", 32)),
		"//dep:clean\x00cfg":  []byte(strings.Repeat("\x03", 32)),
	}
	require.Equal(t, want, hashes)
	hashes["//pkg:clean\x00cfg1"][0] = 0xff
	assert.Equal(t, want["//pkg:clean\x00cfg2"], hashes["//pkg:clean\x00cfg2"])
	assert.Equal(t, want["//dep:clean\x00cfg"], hashes["//dep:clean\x00cfg"])
}

func TestSeedSkipLabelsKeepsProvenUnchangedDependencyHashes(t *testing.T) {
	// A dependency-only label in a dirty package that probe pruning proved
	// unchanged must still seed the scoped cache, or the persisted output
	// loses its dependency hash and the next seeded run treats it as new.
	data := &pkg.PersistedHashData{
		TargetHashes: map[string]map[string]string{
			"//pkg:lib": {"": strings.Repeat("01", 32)},
		},
		DependencyHashes: map[string]map[string]string{
			"//pkg:platform": {"": strings.Repeat("02", 32)},
		},
	}
	dirtyResult := &pkg.DirtySetResult{
		DirtyLabels:     map[string]bool{"//pkg:lib": true, "//pkg:platform": true},
		DirtyStarLabels: map[string]bool{"//pkg:lib": true, "//pkg:platform": true, "//app:bin": true},
		ProvenUnchanged: map[string]bool{"//pkg:platform": true},
	}

	hashes, err := reusableSeedHashes(data, seedSkipLabels(dirtyResult))
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{
		"//pkg:platform\x00": []byte(strings.Repeat("\x02", 32)),
	}, hashes)
}

func TestSeedSkipLabelsWithoutPruningSkipsAllDirtyStar(t *testing.T) {
	dirtyStar := map[string]bool{"//pkg:lib": true, "//app:bin": true}
	assert.Equal(t, dirtyStar, seedSkipLabels(&pkg.DirtySetResult{DirtyStarLabels: dirtyStar}))
}

// twoRunWorkspace models a workspace where //app:consumer depends on the
// manual-tagged //:manual_tool, which reads //inputs:tool.txt. Only
// //app:consumer and //:lib match the targets pattern.
type twoRunWorkspace struct {
	dir string
}

const twoRunBazelRelease = "release 9.0.0"

func newTwoRunWorkspace(t *testing.T) *twoRunWorkspace {
	t.Helper()
	w := &twoRunWorkspace{dir: t.TempDir()}
	w.write(t, "inputs/tool.txt", "tool v1")
	w.write(t, "lib.txt", "lib")
	w.write(t, "README.md", "readme v1")
	return w
}

func (w *twoRunWorkspace) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(w.dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func (w *twoRunWorkspace) rule(name, class string, line int, inputs ...string) *build.Target {
	pkgPath := strings.TrimPrefix(strings.SplitN(name, ":", 2)[0], "//")
	return &build.Target{
		Type: build.Target_RULE.Enum(),
		Rule: &build.Rule{
			Name:      proto.String(name),
			RuleClass: proto.String(class),
			Location:  proto.String(fmt.Sprintf("%s:%d:1", filepath.Join(w.dir, pkgPath, "BUILD.bazel"), line)),
			RuleInput: inputs,
		},
	}
}

func (w *twoRunWorkspace) source(name string) *build.Target {
	parts := strings.SplitN(strings.TrimPrefix(name, "//"), ":", 2)
	return &build.Target{
		Type: build.Target_SOURCE_FILE.Enum(),
		SourceFile: &build.SourceFile{
			Name:     proto.String(name),
			Location: proto.String(filepath.Join(w.dir, parts[0], parts[1]) + ":1:1"),
		},
	}
}

func (w *twoRunWorkspace) targets() map[string]*build.Target {
	return map[string]*build.Target{
		"//app:consumer":    w.rule("//app:consumer", "java_binary", 1, "//:manual_tool"),
		"//:manual_tool":    w.rule("//:manual_tool", "genrule", 1, "//inputs:tool.txt"),
		"//:lib":            w.rule("//:lib", "filegroup", 2, "//:lib.txt"),
		"//inputs:tool.txt": w.source("//inputs:tool.txt"),
		"//:lib.txt":        w.source("//:lib.txt"),
	}
}

func (w *twoRunWorkspace) queryResults(t *testing.T, labels []string, matching []string) *pkg.QueryResults {
	t.Helper()
	all := w.targets()
	targets := make([]*build.Target, 0, len(labels))
	for _, l := range labels {
		targets = append(targets, all[l])
	}
	results, err := pkg.NewQueryModeResults(targets, matching, twoRunBazelRelease)
	require.NoError(t, err)
	return results
}

// fullRun hashes the whole workspace as a non-seeded run would.
func (w *twoRunWorkspace) fullRun(t *testing.T, cfg *config) *pkg.PersistedHashData {
	t.Helper()
	labels := make([]string, 0)
	for l := range w.targets() {
		labels = append(labels, l)
	}
	results := w.queryResults(t, labels, []string{"//app:consumer", "//:lib"})
	require.NoError(t, results.PrefillCache())
	data, err := mergePersistedData(&pkg.PersistedHashData{}, nil, nil, results, cfg, "fingerprint")
	require.NoError(t, err)
	return data
}

// seededRun mirrors runSeeded with the Bazel queries replaced by the probe
// and scoped graphs Bazel would return at the target revision.
func (w *twoRunWorkspace) seededRun(
	t *testing.T,
	cfg *config,
	seed *pkg.PersistedHashData,
	changedFiles map[string]string,
	probeLabels []string,
	scopedLabels []string,
	scopedMatching []string,
) *pkg.PersistedHashData {
	t.Helper()
	dirty := pkg.ComputeDirtySet(changedFiles, seed.TargetEdges,
		pkg.CollectAllLabels(seed.TargetEdges, seed.TargetHashes), nil, nil, nil)
	require.False(t, dirty.NeedsFallback, dirty.FallbackReason)

	probe := w.queryResults(t, probeLabels, probeLabels)
	probeSeed, err := buildExternalSeedHashes(seed, dirty.DirtyPackages)
	require.NoError(t, err)
	require.NoError(t, probe.TargetHashCache.SeedHashes(probeSeed))
	require.NoError(t, probe.PrefillCache())
	dirty = pkg.PruneDirtySet(dirty, seed, pkg.ProbeResult{
		Hashes:      probe.TargetHashCache.ExtractHexHashes(),
		SourceFiles: probe.TargetHashCache.SourceFileLabels(),
	}, changedFiles)

	scoped := w.queryResults(t, scopedLabels, scopedMatching)
	skip := seedSkipLabels(dirty)
	seedHashes, err := reusableSeedHashes(seed, skip)
	require.NoError(t, err)
	require.NoError(t, scoped.TargetHashCache.SeedHashes(seedHashes))
	require.NoError(t, scoped.PrefillCache())
	data, err := mergePersistedData(seed, dirty.DirtyStarLabels, skip, scoped, cfg, "fingerprint")
	require.NoError(t, err)
	return data
}

func TestSeededRunsKeepEdgesOfUnchangedDependencyOnlyLabels(t *testing.T) {
	w := newTwoRunWorkspace(t)
	targets, err := pkg.ParseTargetsList("//...")
	require.NoError(t, err)
	cfg := &config{
		Context:        &pkg.Context{WorkspacePath: w.dir},
		Targets:        targets,
		SeedableOutput: true,
	}
	seed := w.fullRun(t, cfg)
	require.Contains(t, seed.DependencyHashes, "//:manual_tool")
	require.Equal(t, []string{"//inputs:tool.txt"}, seed.TargetEdges["//:manual_tool"])

	// Run 1: a Markdown edit dirties the root package without changing any
	// target. The scoped query lists //:all, which skips the manual target.
	w.write(t, "README.md", "readme v2")
	first := w.seededRun(t, cfg, seed,
		map[string]string{"README.md": "M"},
		[]string{"//:manual_tool", "//:lib", "//:lib.txt", "//inputs:tool.txt"},
		[]string{"//:lib", "//:lib.txt"},
		[]string{"//:lib"},
	)
	assert.Equal(t, seed.DependencyHashes["//:manual_tool"], first.DependencyHashes["//:manual_tool"])
	assert.Equal(t, []string{"//inputs:tool.txt"}, first.TargetEdges["//:manual_tool"])

	// Run 2, seeded from run 1: editing the tool's input in another package
	// must reach //app:consumer through the carried edge.
	w.write(t, "inputs/tool.txt", "tool v2")
	second := w.seededRun(t, cfg, first,
		map[string]string{"inputs/tool.txt": "M"},
		[]string{"//inputs:tool.txt"},
		[]string{"//app:consumer", "//:manual_tool", "//inputs:tool.txt"},
		[]string{"//app:consumer"},
	)
	full := w.fullRun(t, cfg)
	assert.NotEqual(t, seed.TargetHashes["//app:consumer"], second.TargetHashes["//app:consumer"])
	assert.Equal(t, full.TargetHashes["//app:consumer"], second.TargetHashes["//app:consumer"])
}

func TestMergePersistedEdgesPrefersVisitedFreshGraph(t *testing.T) {
	seed := map[string][]string{
		"//pkg:unchanged_skipped": {"//dep:a"},
		"//pkg:visited_now_leaf":  {"//dep:b"},
		"//pkg:dirty":             {"//dep:c"},
		"//other:untouched":       {"//dep:d"},
	}
	skip := map[string]bool{"//pkg:dirty": true}
	fresh := map[string][]string{"//pkg:dirty": {"//dep:e"}}
	visited := map[string]bool{"//pkg:visited_now_leaf": true, "//pkg:dirty": true}

	assert.Equal(t, map[string][]string{
		"//pkg:unchanged_skipped": {"//dep:a"},
		"//pkg:dirty":             {"//dep:e"},
		"//other:untouched":       {"//dep:d"},
	}, mergePersistedEdges(seed, skip, fresh, visited))
}

func BenchmarkValidateSeed(b *testing.B) {
	for _, count := range []int{100000, 500000} {
		b.Run(fmt.Sprintf("hashes_%d", count), func(b *testing.B) {
			data := &pkg.PersistedHashData{FormatVersion: pkg.CurrentPersistedHashFormatVersion, GitCommitSha: "seed", SeedCompatibilityFingerprint: "fingerprint", TargetEdges: map[string][]string{}, TargetHashes: make(map[string]map[string]string, count)}
			for i := 0; i < count; i++ {
				data.TargetHashes[fmt.Sprintf("//representative/package_%06d:target", i)] = map[string]string{"": strings.Repeat("01", 32)}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if reason := validateSeed(data, "seed", "fingerprint"); reason != nil {
					b.Fatal(reason)
				}
			}
		})
	}
}
