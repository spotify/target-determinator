package pkg

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/analysis"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	"google.golang.org/protobuf/proto"
)

const seedTestBazelVersion = "release 5.1.1"

func TestSeedHashesChain(t *testing.T) {
	// A→B→C (chain). Seed B and C. Compute A from seed.
	dir, cqueryResult := layoutProject(t)
	_ = dir

	// Full computation
	fullTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	hwLC := LabelAndConfiguration{
		Label:         mustParseLabel("//HelloWorld:HelloWorld"),
		Configuration: NormalizeConfiguration(configurationChecksum),
	}
	glLC := LabelAndConfiguration{
		Label:         mustParseLabel("//HelloWorld:GreetingLib"),
		Configuration: NormalizeConfiguration(configurationChecksum),
	}

	fullHashHW, err := fullTHC.Hash(hwLC)
	if err != nil {
		t.Fatalf("full hash HelloWorld: %v", err)
	}
	fullHashGL, err := fullTHC.Hash(glLC)
	if err != nil {
		t.Fatalf("full hash GreetingLib: %v", err)
	}

	// Now create a new THC, seed GreetingLib + source files, compute HelloWorld
	allHashes := fullTHC.ExtractHashes()

	// Build seed set: everything except HelloWorld rule
	seedHashes := make(map[string][]byte)
	hwKey := hwLC.Label.String() + "\x00" + hwLC.Configuration.String()
	for k, v := range allHashes {
		if k != hwKey {
			seedHashes[k] = v
		}
	}

	seededTHC2 := parseResult(t, cqueryResult, seedTestBazelVersion)
	if err := seededTHC2.SeedHashes(seedHashes); err != nil {
		t.Fatalf("SeedHashes: %v", err)
	}

	seededHashHW, err := seededTHC2.Hash(hwLC)
	if err != nil {
		t.Fatalf("seeded hash HelloWorld: %v", err)
	}
	seededHashGL, err := seededTHC2.Hash(glLC)
	if err != nil {
		t.Fatalf("seeded hash GreetingLib: %v", err)
	}

	if !bytes.Equal(fullHashHW, seededHashHW) {
		t.Errorf("HelloWorld hash mismatch: full=%s seeded=%s",
			hex.EncodeToString(fullHashHW), hex.EncodeToString(seededHashHW))
	}
	if !bytes.Equal(fullHashGL, seededHashGL) {
		t.Errorf("GreetingLib hash mismatch: full=%s seeded=%s",
			hex.EncodeToString(fullHashGL), hex.EncodeToString(seededHashGL))
	}
}

func TestSeedHashesDiamond(t *testing.T) {
	// Diamond: A→B, A→C, B→D, C→D
	dir := t.TempDir()

	// Create source files
	for _, name := range []string{"a.java", "b.java", "c.java", "d.java"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content-"+name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	configuration := &analysis.Configuration{Checksum: configurationChecksum}
	cfg := NormalizeConfiguration(configurationChecksum)

	cqueryResult := &analysis.CqueryResult{
		Results: []*analysis.ConfiguredTarget{
			{
				Target: &build.Target{
					Type: build.Target_RULE.Enum(),
					Rule: &build.Rule{
						Name:      proto.String("//diamond:A"),
						RuleClass: proto.String("java_binary"),
						Location:  proto.String(fmt.Sprintf("%s/BUILD.bazel:1:1", dir)),
						RuleInput: []string{"//diamond:B", "//diamond:C"},
					},
				},
				Configuration: configuration,
			},
			{
				Target: &build.Target{
					Type: build.Target_RULE.Enum(),
					Rule: &build.Rule{
						Name:      proto.String("//diamond:B"),
						RuleClass: proto.String("java_library"),
						Location:  proto.String(fmt.Sprintf("%s/BUILD.bazel:2:1", dir)),
						RuleInput: []string{"//diamond:D", "//diamond:b.java"},
					},
				},
				Configuration: configuration,
			},
			{
				Target: &build.Target{
					Type: build.Target_RULE.Enum(),
					Rule: &build.Rule{
						Name:      proto.String("//diamond:C"),
						RuleClass: proto.String("java_library"),
						Location:  proto.String(fmt.Sprintf("%s/BUILD.bazel:3:1", dir)),
						RuleInput: []string{"//diamond:D", "//diamond:c.java"},
					},
				},
				Configuration: configuration,
			},
			{
				Target: &build.Target{
					Type: build.Target_RULE.Enum(),
					Rule: &build.Rule{
						Name:      proto.String("//diamond:D"),
						RuleClass: proto.String("java_library"),
						Location:  proto.String(fmt.Sprintf("%s/BUILD.bazel:4:1", dir)),
						RuleInput: []string{"//diamond:d.java"},
					},
				},
				Configuration: configuration,
			},
			sourceFileTarget(dir, "//diamond:a.java"),
			sourceFileTarget(dir, "//diamond:b.java"),
			sourceFileTarget(dir, "//diamond:c.java"),
			sourceFileTarget(dir, "//diamond:d.java"),
		},
	}

	fullTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	aLC := LabelAndConfiguration{Label: mustParseLabel("//diamond:A"), Configuration: cfg}

	fullHashA, err := fullTHC.Hash(aLC)
	if err != nil {
		t.Fatalf("full hash A: %v", err)
	}

	// Seed everything except A, recompute A
	allHashes := fullTHC.ExtractHashes()
	seedHashes := make(map[string][]byte)
	aKey := aLC.Label.String() + "\x00" + aLC.Configuration.String()
	for k, v := range allHashes {
		if k != aKey {
			seedHashes[k] = v
		}
	}

	seededTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	if err := seededTHC.SeedHashes(seedHashes); err != nil {
		t.Fatalf("SeedHashes: %v", err)
	}

	seededHashA, err := seededTHC.Hash(aLC)
	if err != nil {
		t.Fatalf("seeded hash A: %v", err)
	}

	if !bytes.Equal(fullHashA, seededHashA) {
		t.Errorf("diamond A hash mismatch: full=%s seeded=%s",
			hex.EncodeToString(fullHashA), hex.EncodeToString(seededHashA))
	}
}

func TestSeedHashesChangedSource(t *testing.T) {
	// Seed all hashes. Change a source file. Verify the source file's
	// target and its rdeps get different hashes.
	dir, cqueryResult := layoutProject(t)
	cfg := NormalizeConfiguration(configurationChecksum)

	fullTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	hwLC := LabelAndConfiguration{Label: mustParseLabel("//HelloWorld:HelloWorld"), Configuration: cfg}
	glLC := LabelAndConfiguration{Label: mustParseLabel("//HelloWorld:GreetingLib"), Configuration: cfg}

	fullHashHW, _ := fullTHC.Hash(hwLC)
	fullHashGL, _ := fullTHC.Hash(glLC)

	// Change Greeting.java (transitive source)
	if err := os.WriteFile(filepath.Join(dir, "Greeting.java"), []byte("changed!"), 0o644); err != nil {
		t.Fatalf("write Greeting.java: %v", err)
	}

	// New THC reads the changed file
	changedTHC := parseResult(t, cqueryResult, seedTestBazelVersion)

	// Seed only HelloWorld.java's hash (unchanged source), not Greeting.java or rules
	hwJavaLC := LabelAndConfiguration{Label: mustParseLabel("//HelloWorld:HelloWorld.java"), Configuration: NormalizeConfiguration("")}
	hwJavaHash, _ := fullTHC.Hash(hwJavaLC)
	hwJavaKey := hwJavaLC.Label.String() + "\x00" + hwJavaLC.Configuration.String()
	seedHashes := map[string][]byte{hwJavaKey: hwJavaHash}

	if err := changedTHC.SeedHashes(seedHashes); err != nil {
		t.Fatalf("SeedHashes: %v", err)
	}

	changedHashGL, _ := changedTHC.Hash(glLC)
	changedHashHW, _ := changedTHC.Hash(hwLC)

	if bytes.Equal(fullHashGL, changedHashGL) {
		t.Error("expected GreetingLib hash to change when Greeting.java changes")
	}
	if bytes.Equal(fullHashHW, changedHashHW) {
		t.Error("expected HelloWorld hash to change when transitive source changes")
	}
}

func TestSeedHashesRecursionTerminates(t *testing.T) {
	// Deep chain: T0→T1→T2→...→T9→source. Seed T1..T9+source. Compute T0.
	dir := t.TempDir()
	const depth = 10
	cfg := NormalizeConfiguration(configurationChecksum)
	configuration := &analysis.Configuration{Checksum: configurationChecksum}

	if err := os.WriteFile(filepath.Join(dir, "leaf.java"), []byte("leaf"), 0o644); err != nil {
		t.Fatalf("write leaf.java: %v", err)
	}

	var results []*analysis.ConfiguredTarget
	for i := 0; i < depth; i++ {
		var ruleInput []string
		if i < depth-1 {
			ruleInput = []string{fmt.Sprintf("//chain:T%d", i+1)}
		} else {
			ruleInput = []string{"//chain:leaf.java"}
		}
		results = append(results, &analysis.ConfiguredTarget{
			Target: &build.Target{
				Type: build.Target_RULE.Enum(),
				Rule: &build.Rule{
					Name:      proto.String(fmt.Sprintf("//chain:T%d", i)),
					RuleClass: proto.String("java_library"),
					Location:  proto.String(fmt.Sprintf("%s/BUILD.bazel:%d:1", dir, i+1)),
					RuleInput: ruleInput,
				},
			},
			Configuration: configuration,
		})
	}
	results = append(results, sourceFileTarget(dir, "//chain:leaf.java"))

	cqueryResult := &analysis.CqueryResult{Results: results}

	fullTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	t0LC := LabelAndConfiguration{Label: mustParseLabel("//chain:T0"), Configuration: cfg}

	fullHashT0, err := fullTHC.Hash(t0LC)
	if err != nil {
		t.Fatalf("full hash T0: %v", err)
	}

	// Seed all except T0
	allHashes := fullTHC.ExtractHashes()
	seedHashes := make(map[string][]byte)
	t0Key := t0LC.Label.String() + "\x00" + t0LC.Configuration.String()
	for k, v := range allHashes {
		if k != t0Key {
			seedHashes[k] = v
		}
	}

	seededTHC := parseResult(t, cqueryResult, seedTestBazelVersion)
	if err := seededTHC.SeedHashes(seedHashes); err != nil {
		t.Fatalf("SeedHashes: %v", err)
	}

	seededHashT0, err := seededTHC.Hash(t0LC)
	if err != nil {
		t.Fatalf("seeded hash T0: %v", err)
	}

	if !bytes.Equal(fullHashT0, seededHashT0) {
		t.Errorf("chain T0 hash mismatch: full=%s seeded=%s",
			hex.EncodeToString(fullHashT0), hex.EncodeToString(seededHashT0))
	}
}

func TestSeedHashesNotFrozen(t *testing.T) {
	// Verify SeedHashes does NOT freeze the cache (unlike RestoreHashes).
	_, cqueryResult := layoutProject(t)
	thc := parseResult(t, cqueryResult, seedTestBazelVersion)

	if err := thc.SeedHashes(map[string][]byte{}); err != nil {
		t.Fatalf("SeedHashes: %v", err)
	}

	// Should be able to compute a new hash (cache not frozen)
	hwLC := LabelAndConfiguration{
		Label:         mustParseLabel("//HelloWorld:HelloWorld"),
		Configuration: NormalizeConfiguration(configurationChecksum),
	}
	_, err := thc.Hash(hwLC)
	if err != nil {
		t.Fatalf("Hash after SeedHashes should succeed (not frozen): %v", err)
	}
}

func TestRestoreHashesIsFrozen(t *testing.T) {
	// Contrast: RestoreHashes DOES freeze the cache.
	_, cqueryResult := layoutProject(t)
	thc := parseResult(t, cqueryResult, seedTestBazelVersion)

	if err := thc.RestoreHashes(map[string][]byte{}); err != nil {
		t.Fatalf("RestoreHashes: %v", err)
	}

	hwLC := LabelAndConfiguration{
		Label:         mustParseLabel("//HelloWorld:HelloWorld"),
		Configuration: NormalizeConfiguration(configurationChecksum),
	}
	_, err := thc.Hash(hwLC)
	if err == nil {
		t.Fatal("Hash after RestoreHashes should fail (frozen), but succeeded")
	}
}

func sourceFileTarget(dir string, labelStr string) *analysis.ConfiguredTarget {
	name := labelStr[len("//"):]
	if idx := len(name) - 1; idx >= 0 {
		for i, c := range name {
			if c == ':' {
				name = name[i+1:]
				break
			}
		}
	}
	return &analysis.ConfiguredTarget{
		Target: &build.Target{
			Type: build.Target_SOURCE_FILE.Enum(),
			SourceFile: &build.SourceFile{
				Name:     proto.String(labelStr),
				Location: proto.String(fmt.Sprintf("%s/%s:1:1", dir, name)),
			},
		},
	}
}
