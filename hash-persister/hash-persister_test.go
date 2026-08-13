package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/target-determinator/pkg"
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
		want       bool
	}{
		"below threshold":    {recomputed: 69, total: 100, want: false},
		"at threshold":       {recomputed: 70, total: 100, want: true},
		"above threshold":    {recomputed: 71, total: 100, want: true},
		"fractional below":   {recomputed: 6, total: 9, want: false},
		"fractional at":      {recomputed: 7, total: 10, want: true},
		"empty seed":         {recomputed: 0, total: 0, want: false},
		"no dirty targets":   {recomputed: 0, total: 100, want: false},
		"dirty exceeds seed": {recomputed: 2, total: 1, want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := shouldFallbackForRecomputation(test.recomputed, test.total); got != test.want {
				t.Fatalf("shouldFallbackForRecomputation(%d, %d) = %v, want %v",
					test.recomputed, test.total, got, test.want)
			}
		})
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
