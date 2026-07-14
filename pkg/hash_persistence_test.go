package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV9FormatSizeDelta(t *testing.T) {
	const numTargets = 1000
	const avgDeps = 5

	targetHashes := make(map[string]map[string]string, numTargets)
	targetEdges := make(map[string][]string, numTargets)
	targetConfigs := make(map[string]string, numTargets)

	for i := 0; i < numTargets; i++ {
		label := fmt.Sprintf("//pkg%d:target%d", i/10, i%10)
		targetHashes[label] = map[string]string{
			"": fmt.Sprintf("%064x", i),
		}
		targetConfigs[label] = ""

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
		FormatVersion:        9,
		GitCommitSha:         "abc123",
		Timestamp:            v8Data.Timestamp,
		BazelRelease:         "release 8.0.0",
		TargetHashes:         targetHashes,
		TargetEdges:          targetEdges,
		TargetConfigurations: targetConfigs,
		Metadata: HashMetadata{
			TargetsPattern: "//...",
			TotalTargets:   numTargets,
		},
	}

	v8JSON, err := json.Marshal(v8Data)
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
		FormatVersion: 9,
		GitCommitSha:  "abc123",
		Timestamp:     time.Now(),
		BazelRelease:  "release 8.0.0",
		TargetHashes: map[string]map[string]string{
			"//pkg:a": {"": "aaa"},
			"//pkg:b": {"": "bbb"},
		},
		TargetEdges: map[string][]string{
			"//pkg:a": {"//pkg:b"},
		},
		TargetConfigurations: map[string]string{
			"//pkg:a": "",
			"//pkg:b": "",
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

	if loaded.FormatVersion != 9 {
		t.Errorf("expected FormatVersion 9, got %d", loaded.FormatVersion)
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
		FormatVersion: 9,
		GitCommitSha:  "after",
		Timestamp:     ts,
		BazelRelease:  "release 8.0.0",
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
