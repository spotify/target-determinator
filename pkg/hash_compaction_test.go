package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/analysis"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"
)

// metadataBazel serves a small real protobuf graph through the query and cquery
// protocols, exercising both public loaders without starting a Bazel server.
type metadataBazel struct {
	targets []*build.Target
}

func (b metadataBazel) HashKey() string { return "metadata-loader-test" }

func (b metadataBazel) Execute(cfg BazelCmdConfig, _ []string, command string, args ...string) (int, error) {
	switch command {
	case "info":
		values := map[string]string{"release": "release 9.2.0", "starlark-semantics": "enable_bzlmod=true"}
		if len(args) == 1 {
			if value, ok := values[args[0]]; ok {
				_, err := fmt.Fprintln(cfg.Stdout, value)
				return 0, err
			}
		}
	case "mod":
		return 0, json.NewEncoder(cfg.Stdout).Encode(map[string]string{"short": "canonical"})
	case "config":
		_, err := fmt.Fprint(cfg.Stdout, "[]")
		return 0, err
	case "query":
		for _, arg := range args {
			if strings.HasPrefix(arg, "--output_file=") {
				file, err := os.Create(strings.TrimPrefix(arg, "--output_file="))
				if err != nil {
					return 1, err
				}
				defer file.Close()
				for _, target := range b.targets {
					if _, err := protodelim.MarshalTo(file, target); err != nil {
						return 1, err
					}
				}
				return 0, nil
			}
			if arg == "--output=label" {
				_, err := fmt.Fprintln(cfg.Stdout, "//pkg:root")
				return 0, err
			}
		}
	}
	return 1, fmt.Errorf("unexpected command %s %v", command, args)
}

func (b metadataBazel) Cquery(_ string, cfg BazelCmdConfig, _ []string, _ ...string) (int, error) {
	result := &analysis.CqueryResult{}
	for _, target := range b.targets {
		result.Results = append(result.Results, &analysis.ConfiguredTarget{
			Target: target, Configuration: &analysis.Configuration{},
		})
	}
	_, err := protodelim.MarshalTo(cfg.Stdout, result)
	return 0, err
}

func compactionFixture(t *testing.T) (string, metadataBazel) {
	t.Helper()
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "dep"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "dep/input.txt"), []byte("source contents"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "java.rules"), []byte("toolchain fingerprint"), 0600))
	return workspace, metadataBazel{targets: []*build.Target{
		{Type: build.Target_SOURCE_FILE.Enum(), SourceFile: &build.SourceFile{
			Name: proto.String("//dep:input.txt"), Location: proto.String(filepath.Join(workspace, "dep/input.txt") + ":1:1"),
		}},
		{Type: build.Target_RULE.Enum(), Rule: &build.Rule{
			Name: proto.String("@@canonical//dep:dep"), RuleClass: proto.String("filegroup"),
			RuleInput: []string{"//dep:input.txt"},
			Attribute: []*build.Attribute{{Name: proto.String("tags"), Type: build.Attribute_STRING_LIST.Enum(), StringListValue: []string{"a", "b"}}},
		}},
		{Type: build.Target_GENERATED_FILE.Enum(), GeneratedFile: &build.GeneratedFile{
			Name: proto.String("//dep:generated.txt"), GeneratingRule: proto.String("@short//dep:dep"),
		}},
		{Type: build.Target_RULE.Enum(), Rule: &build.Rule{
			Name: proto.String("//pkg:root"), RuleClass: proto.String("java_library"),
			SkylarkEnvironmentHashCode: proto.String("environment"),
			RuleInput:                  []string{"@short//dep:dep", "//dep:generated.txt", "//dep:input.txt"},
			Attribute: []*build.Attribute{
				{Name: proto.String("deps"), Type: build.Attribute_LABEL_LIST.Enum(), StringListValue: []string{"@short//dep:dep"}, ExplicitlySpecified: proto.Bool(true)},
				{Name: proto.String("generator_location"), Type: build.Attribute_STRING.Enum(), StringValue: proto.String("/absolute/location")},
				{Name: proto.String("visibility"), Type: build.Attribute_STRING_LIST.Enum(), StringListValue: []string{"@//pkg:pkg"}, Nodep: proto.Bool(true)},
				{Name: proto.String("label_map"), Type: build.Attribute_LABEL_DICT_UNARY.Enum(), LabelDictUnaryValue: []*build.LabelDictUnaryEntry{{Key: proto.String("key"), Value: proto.String("@short//dep:dep")}}},
				{Name: proto.String("label_list_map"), Type: build.Attribute_LABEL_LIST_DICT.Enum(), LabelListDictValue: []*build.LabelListDictEntry{{Key: proto.String("key"), Value: []string{"@short//dep:dep"}}}},
				{Name: proto.String("label_keys"), Type: build.Attribute_LABEL_KEYED_STRING_DICT.Enum(), LabelKeyedStringDictValue: []*build.LabelKeyedStringDictEntry{{Key: proto.String("@short//dep:dep"), Value: proto.String("value")}}},
			},
		}},
	}}
}

func TestHashOnlyMetadataPreservesHashesAndRetainsFullMetadataWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name, backend      string
		includeDifferences bool
		compact            bool
	}{
		{name: "query", backend: "query", compact: true},
		{name: "query with explanations", backend: "query", includeDifferences: true},
		{name: "cquery", backend: "cquery"},
	} {
		for _, opaque := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/opaque_%t", tc.name, opaque), func(t *testing.T) {
				workspace, bazel := compactionFixture(t)
				ctx := &Context{
					WorkspacePath: workspace, BazelCmd: bazel, QueryBackend: tc.backend,
					IncludeDifferences: tc.includeDifferences, AnalysisCacheClearStrategy: "skip",
					RuleClassFingerprints: []RuleClassFingerprint{{RuleClassGlobs: []string{"java_*"}, Files: []string{"java.rules"}}},
				}
				if opaque {
					ctx.OpaqueInputRepositories = []string{"short"}
				}
				targets, err := ParseTargetsList("//pkg:root")
				require.NoError(t, err)
				full, cleanup, err := LoadIncompleteMetadata(ctx, NoLabelledGitRev, targets)
				defer cleanup()
				require.NoError(t, err)
				small, cleanup, err := LoadMetadataForHashing(ctx, NoLabelledGitRev, targets)
				defer cleanup()
				require.NoError(t, err)

				root := label.New("", "pkg", "root")
				configuration := NormalizeConfiguration("")
				require.NotEmpty(t, full.TransitiveConfiguredTargets[root][configuration].GetTarget().GetRule().Attribute)
				attributes := small.TransitiveConfiguredTargets[root][configuration].GetTarget().GetRule().Attribute
				if tc.compact {
					require.Nil(t, attributes)
				} else {
					require.NotEmpty(t, attributes)
				}
				require.NoError(t, full.PrefillCache())
				require.NoError(t, small.PrefillCache())
				for target, configurations := range full.TransitiveConfiguredTargets {
					for config := range configurations {
						key := LabelAndConfiguration{Label: target, Configuration: config}
						want, err := full.TargetHashCache.Hash(key)
						require.NoError(t, err)
						got, err := small.TargetHashCache.Hash(key)
						require.NoError(t, err)
						require.Equal(t, want, got, "hash changed for %s", target)
					}
				}
				want, err := BuildPersistedHashData("commit", full, ctx, targets.String(), tc.backend == "query")
				require.NoError(t, err)
				got, err := BuildPersistedHashData("commit", small, ctx, targets.String(), tc.backend == "query")
				require.NoError(t, err)
				got.Timestamp = want.Timestamp
				require.Equal(t, want, got)
			})
		}
	}
}

func TestPrehashOpaqueRuleDoesNotReadItsAttributes(t *testing.T) {
	n := &Normalizer{Mapping: map[string]string{"short": "canonical"}}
	rule := &build.Rule{Name: proto.String("@short//dep:dep"), Attribute: []*build.Attribute{{}}}
	cache := NewTargetHashCache(nil, n, "release 9.2.0", true, nil, "short")
	require.NoError(t, cache.prehashRuleAttributes(rule))
	require.Nil(t, rule.Attribute)
}

func TestHashOnlyMetadataDoesNotHashDependencyFilesEarly(t *testing.T) {
	workspace, bazel := compactionFixture(t)
	ctx := &Context{WorkspacePath: workspace, BazelCmd: bazel, QueryBackend: "query"}
	targets, err := ParseTargetsList("//pkg:root")
	require.NoError(t, err)
	key := LabelAndConfiguration{Label: label.New("", "pkg", "root"), Configuration: NormalizeConfiguration("")}
	before, cleanup, err := LoadIncompleteMetadata(ctx, NoLabelledGitRev, targets)
	defer cleanup()
	require.NoError(t, err)
	beforeHash, err := before.TargetHashCache.Hash(key)
	require.NoError(t, err)
	prepared, cleanup, err := LoadMetadataForHashing(ctx, NoLabelledGitRev, targets)
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(workspace, "dep/input.txt"), []byte("changed after metadata loading"), 0600))
	after, cleanup, err := LoadIncompleteMetadata(ctx, NoLabelledGitRev, targets)
	defer cleanup()
	require.NoError(t, err)
	afterHash, err := after.TargetHashCache.Hash(key)
	require.NoError(t, err)
	preparedHash, err := prepared.TargetHashCache.Hash(key)
	require.NoError(t, err)
	require.NotEqual(t, beforeHash, afterHash)
	require.Equal(t, afterHash, preparedHash)
}
