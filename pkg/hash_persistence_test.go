package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ss "github.com/bazel-contrib/target-determinator/common/sorted_set"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/analysis"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	gazelle_label "github.com/bazelbuild/bazel-gazelle/label"
	"google.golang.org/protobuf/proto"
)

func TestSeedableFormatSizeDelta(t *testing.T) {
	const numTargets = 1000
	const avgDeps = 5

	targetHashes := make(map[string]map[string]string, numTargets)
	targetEdges := make(map[string][]string, numTargets)

	for i := 0; i < numTargets; i++ {
		label := fmt.Sprintf("//pkg%d:target%d", i/10, i%10)
		targetHashes[label] = map[string]string{
			"": fmt.Sprintf("%064x", i),
		}

		deps := make([]string, 0, avgDeps)
		for j := 0; j < avgDeps && i+j+1 < numTargets; j++ {
			depLabel := fmt.Sprintf("//pkg%d:target%d", (i+j+1)/10, (i+j+1)%10)
			deps = append(deps, depLabel)
		}
		if len(deps) > 0 {
			targetEdges[label] = deps
		}
	}

	v8Data := PersistedHashData{
		FormatVersion: 0,
		GitCommitSha:  "abc123",
		Timestamp:     time.Now(),
		BazelRelease:  "release 8.0.0",
		TargetHashes:  targetHashes,
		Metadata: HashMetadata{
			TargetsPattern: "//...",
			TotalTargets:   numTargets,
		},
	}

	v9Data := PersistedHashData{
		FormatVersion:                CurrentPersistedHashFormatVersion,
		SeedCompatibilityFingerprint: "compatibility",
		GitCommitSha:                 "abc123",
		Timestamp:                    v8Data.Timestamp,
		BazelRelease:                 "release 8.0.0",
		TargetHashes:                 targetHashes,
		TargetEdges:                  targetEdges,
		Metadata: HashMetadata{
			TargetsPattern: "//...",
			TotalTargets:   numTargets,
		},
	}

	// Legacy PersistHashes output is indented; seedable output is compact to
	// offset some of the additional dependency-graph cost.
	v8JSON, err := json.MarshalIndent(v8Data, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal v8: %v", err)
	}
	v9JSON, err := json.Marshal(v9Data)
	if err != nil {
		t.Fatalf("failed to marshal v9: %v", err)
	}

	v8Size := len(v8JSON)
	v9Size := len(v9JSON)
	deltaBytes := v9Size - v8Size
	deltaPercent := float64(deltaBytes) / float64(v8Size) * 100.0

	t.Logf("v9 format size delta (%d targets, ~%d deps each):", numTargets, avgDeps)
	t.Logf("  v8 size: %d bytes (%.1f KB)", v8Size, float64(v8Size)/1024)
	t.Logf("  v9 size: %d bytes (%.1f KB)", v9Size, float64(v9Size)/1024)
	t.Logf("  delta:   +%d bytes (+%.1f%%)", deltaBytes, deltaPercent)
	t.Logf("  extrapolated for 442K targets: v8 ~%.1f MB, v9 ~%.1f MB",
		float64(v8Size)*442000.0/float64(numTargets)/1024/1024,
		float64(v9Size)*442000.0/float64(numTargets)/1024/1024)
}

func TestLoadPersistedHashesReadsV8(t *testing.T) {
	dir := t.TempDir()
	v8File := filepath.Join(dir, "v8.json")

	v8Data := PersistedHashData{
		GitCommitSha: "abc123",
		Timestamp:    time.Now(),
		BazelRelease: "release 8.0.0",
		TargetHashes: map[string]map[string]string{
			"//pkg:target": {"": "deadbeef"},
		},
		Metadata: HashMetadata{
			TargetsPattern: "//...",
			TotalTargets:   1,
		},
	}

	data, err := json.MarshalIndent(v8Data, "", "  ")
	if err != nil {
		t.Fatalf("marshal v8: %v", err)
	}
	if err := os.WriteFile(v8File, data, 0o644); err != nil {
		t.Fatalf("write v8: %v", err)
	}

	loaded, err := LoadPersistedHashes(v8File)
	if err != nil {
		t.Fatalf("LoadPersistedHashes v8: %v", err)
	}

	if loaded.FormatVersion != 0 {
		t.Errorf("expected FormatVersion 0 for v8 file, got %d", loaded.FormatVersion)
	}
	if loaded.TargetEdges != nil {
		t.Errorf("expected nil TargetEdges for v8 file, got %v", loaded.TargetEdges)
	}
	if loaded.TargetHashes["//pkg:target"][""] != "deadbeef" {
		t.Errorf("hash mismatch: got %v", loaded.TargetHashes)
	}
}

func TestLoadPersistedHashesReadsV9(t *testing.T) {
	dir := t.TempDir()
	v9File := filepath.Join(dir, "v9.json")

	v9Data := PersistedHashData{
		FormatVersion:                CurrentPersistedHashFormatVersion,
		SeedCompatibilityFingerprint: "compatibility",
		GitCommitSha:                 "abc123",
		Timestamp:                    time.Now(),
		BazelRelease:                 "release 8.0.0",
		TargetHashes: map[string]map[string]string{
			"//pkg:a": {"": "aaa"},
			"//pkg:b": {"": "bbb"},
		},
		TargetEdges: map[string][]string{
			"//pkg:a": {"//pkg:b"},
		},
		Metadata: HashMetadata{
			TargetsPattern: "//...",
			TotalTargets:   2,
		},
	}

	data, err := json.MarshalIndent(v9Data, "", "  ")
	if err != nil {
		t.Fatalf("marshal v9: %v", err)
	}
	if err := os.WriteFile(v9File, data, 0o644); err != nil {
		t.Fatalf("write v9: %v", err)
	}

	loaded, err := LoadPersistedHashes(v9File)
	if err != nil {
		t.Fatalf("LoadPersistedHashes v9: %v", err)
	}

	if loaded.FormatVersion != CurrentPersistedHashFormatVersion {
		t.Errorf("expected FormatVersion 9, got %d", loaded.FormatVersion)
	}
	if loaded.SeedCompatibilityFingerprint != "compatibility" {
		t.Errorf("fingerprint mismatch: got %q", loaded.SeedCompatibilityFingerprint)
	}
	if len(loaded.TargetEdges) != 1 {
		t.Errorf("expected 1 edge entry, got %d", len(loaded.TargetEdges))
	}
	if len(loaded.TargetEdges["//pkg:a"]) != 1 || loaded.TargetEdges["//pkg:a"][0] != "//pkg:b" {
		t.Errorf("edge mismatch: got %v", loaded.TargetEdges)
	}
}

func TestCompareHashFilesV8AndV9(t *testing.T) {
	dir := t.TempDir()
	v8File := filepath.Join(dir, "v8.json")
	v9File := filepath.Join(dir, "v9.json")

	ts := time.Now()

	v8Data := PersistedHashData{
		GitCommitSha: "before",
		Timestamp:    ts,
		BazelRelease: "release 8.0.0",
		TargetHashes: map[string]map[string]string{
			"//pkg:a": {"": "aaa"},
			"//pkg:b": {"": "bbb"},
		},
		Metadata: HashMetadata{TotalTargets: 2},
	}
	v9Data := PersistedHashData{
		FormatVersion:                CurrentPersistedHashFormatVersion,
		SeedCompatibilityFingerprint: "compatibility",
		GitCommitSha:                 "after",
		Timestamp:                    ts,
		BazelRelease:                 "release 8.0.0",
		TargetHashes: map[string]map[string]string{
			"//pkg:a": {"": "aaa"},
			"//pkg:b": {"": "changed"},
		},
		TargetEdges: map[string][]string{
			"//pkg:a": {"//pkg:b"},
		},
		Metadata: HashMetadata{TotalTargets: 2},
	}

	writeJSON := func(path string, data interface{}) {
		b, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	writeJSON(v8File, v8Data)
	writeJSON(v9File, v9Data)

	result, err := CompareHashFiles(v8File, v9File)
	if err != nil {
		t.Fatalf("CompareHashFiles v8 vs v9: %v", err)
	}

	if result.Summary.TotalChanged != 1 {
		t.Errorf("expected 1 changed, got %d", result.Summary.TotalChanged)
	}
	if len(result.Summary.AffectedTargets) != 1 || result.Summary.AffectedTargets[0] != "//pkg:b" {
		t.Errorf("expected //pkg:b affected, got %v", result.Summary.AffectedTargets)
	}
}

func TestSeedCompatibilityFingerprintChangesWithHashingInputs(t *testing.T) {
	base := &Context{
		BazelCmd:                  fakeBazelCmd{hashKey: "command-a"},
		QueryBackend:              "query",
		FilterIncompatibleTargets: false,
	}
	fingerprint, err := ComputeSeedCompatibilityFingerprint(base, "//...", "release 8.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Context) string{
		"targets":       func(*Context) string { return "//services/..." },
		"backend":       func(ctx *Context) string { ctx.QueryBackend = "cquery"; return "//..." },
		"bazel command": func(ctx *Context) string { ctx.BazelCmd = fakeBazelCmd{hashKey: "command-b"}; return "//..." },
		"fingerprints": func(ctx *Context) string {
			ctx.RuleClassFingerprints = []RuleClassFingerprint{{RuleClassGlobs: []string{"java_*"}, Files: []string{"versions.txt"}}}
			return "//..."
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := *base
			pattern := mutate(&changed)
			got, err := ComputeSeedCompatibilityFingerprint(&changed, pattern, "release 8.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if got == fingerprint {
				t.Fatalf("fingerprint did not change for %s", name)
			}
		})
	}
	differentRelease, err := ComputeSeedCompatibilityFingerprint(base, "//...", "release 9.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if differentRelease == fingerprint {
		t.Fatal("fingerprint did not change with Bazel release")
	}
}

func TestPersistenceModesPreserveHashesAndGeneratedFileEdges(t *testing.T) {
	configuration := NormalizeConfiguration("")
	generatorLabel := mustParseLabel("//gen:generator")
	outputLabel := mustParseLabel("//gen:output")
	consumerLabel := mustParseLabel("//app:consumer")
	transitive := map[gazelle_label.Label]map[Configuration]*analysis.ConfiguredTarget{
		generatorLabel: {configuration: {Target: &build.Target{
			Type: build.Target_RULE.Enum(),
			Rule: &build.Rule{Name: proto.String(generatorLabel.String()), RuleClass: proto.String("genrule")},
		}}},
		outputLabel: {configuration: {Target: &build.Target{
			Type:          build.Target_GENERATED_FILE.Enum(),
			GeneratedFile: &build.GeneratedFile{Name: proto.String(outputLabel.String()), GeneratingRule: proto.String(generatorLabel.String())},
		}}},
		consumerLabel: {configuration: {Target: &build.Target{
			Type: build.Target_RULE.Enum(),
			Rule: &build.Rule{Name: proto.String(consumerLabel.String()), RuleClass: proto.String("java_library"), RuleInput: []string{outputLabel.String()}},
		}}},
	}
	queryResults := &QueryResults{
		MatchingTargets: &MatchingTargets{
			labels: ss.NewSortedSetFn([]gazelle_label.Label{generatorLabel, consumerLabel}, CompareLabels),
			labelsToConfigurations: map[gazelle_label.Label]*ss.SortedSet[Configuration]{
				generatorLabel: ss.NewSortedSetFn([]Configuration{configuration}, ConfigurationLess),
				consumerLabel:  ss.NewSortedSetFn([]Configuration{configuration}, ConfigurationLess),
			},
		},
		TransitiveConfiguredTargets: transitive,
		TargetHashCache:             NewTargetHashCache(transitive, &Normalizer{}, "release 8.0.0", true, nil),
	}
	edges, err := ExtractEdges(queryResults)
	if err != nil {
		t.Fatal(err)
	}
	if got := edges[outputLabel.String()]; len(got) != 1 || got[0] != generatorLabel.String() {
		t.Fatalf("generated-file edge = %v, want [%s]", got, generatorLabel)
	}

	context := &Context{
		WorkspacePath: t.TempDir(),
		BazelCmd:      fakeBazelCmd{hashKey: "command"},
		QueryBackend:  "query",
	}
	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := PersistHashes(legacyPath, "abc123", queryResults, context, "//..."); err != nil {
		t.Fatal(err)
	}
	legacyRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyRaw), "\n  \"git_commit_sha\"") {
		t.Fatal("legacy output is no longer pretty-printed")
	}
	legacy, err := LoadPersistedHashes(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.FormatVersion != 0 || legacy.TargetEdges != nil || legacy.SeedCompatibilityFingerprint != "" {
		t.Fatalf("legacy output unexpectedly contains incremental metadata: %#v", legacy)
	}

	seedablePath := filepath.Join(t.TempDir(), "seedable.json")
	if err := PersistSeedableHashes(seedablePath, "abc123", queryResults, context, "//..."); err != nil {
		t.Fatal(err)
	}
	seedable, err := LoadPersistedHashes(seedablePath)
	if err != nil {
		t.Fatal(err)
	}
	if seedable.FormatVersion != CurrentPersistedHashFormatVersion {
		t.Fatalf("seedable format version = %d, want %d", seedable.FormatVersion, CurrentPersistedHashFormatVersion)
	}
	if seedable.SeedCompatibilityFingerprint == "" {
		t.Fatal("seedable output has no compatibility fingerprint")
	}
	if got := seedable.TargetEdges[outputLabel.String()]; len(got) != 1 || got[0] != generatorLabel.String() {
		t.Fatalf("seedable generated-file edge = %v, want [%s]", got, generatorLabel)
	}
	if !reflect.DeepEqual(seedable.TargetHashes, legacy.TargetHashes) {
		t.Fatalf("seedable hashes differ from legacy hashes:\nseedable: %v\nlegacy: %v", seedable.TargetHashes, legacy.TargetHashes)
	}
}
