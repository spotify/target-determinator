package main

import (
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
	if reason := validateSeed(valid, "seed-sha", "fingerprint"); reason != "" {
		t.Fatalf("valid seed rejected: %s", reason)
	}

	tests := map[string]func(*pkg.PersistedHashData){
		"format":      func(data *pkg.PersistedHashData) { data.FormatVersion-- },
		"commit":      func(data *pkg.PersistedHashData) { data.GitCommitSha = "other" },
		"fingerprint": func(data *pkg.PersistedHashData) { data.SeedCompatibilityFingerprint = "other" },
		"edges":       func(data *pkg.PersistedHashData) { data.TargetEdges = nil },
		"hash encoding": func(data *pkg.PersistedHashData) {
			data.TargetHashes = map[string]map[string]string{"//pkg:target": {"": "not-hex"}}
		},
		"hash length": func(data *pkg.PersistedHashData) {
			data.TargetHashes = map[string]map[string]string{"//pkg:target": {"": "01"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := *valid
			mutate(&changed)
			if reason := validateSeed(&changed, "seed-sha", "fingerprint"); reason == "" {
				t.Fatal("incompatible seed was accepted")
			}
		})
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

func TestValidateSeedableBackend(t *testing.T) {
	if err := validateSeedableBackend(true, "cquery"); err == nil {
		t.Fatal("expected cquery seedable output to be rejected")
	}
	if err := validateSeedableBackend(true, "query"); err != nil {
		t.Fatalf("query seedable output rejected: %v", err)
	}
	if err := validateSeedableBackend(false, "cquery"); err != nil {
		t.Fatalf("full cquery mode rejected: %v", err)
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
