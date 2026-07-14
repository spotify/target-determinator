package pkg

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// BuildScopedPattern returns a bazel query expression that covers only the
// given labels. It writes the labels to a query-file when there are many, or
// uses an inline set() for small sets.
//
// Known limitations of scoped mode versus a full //... query:
//   - --filter-incompatible-targets is not applied separately; the universe
//     is already narrowed so unaffected incompatible targets are excluded.
//   - Manual-tagged targets: set() names targets explicitly, so manual targets
//     in the dirty set are included (matching deps() behavior).
//   - Newly created targets in unchanged packages that happen to depend on a
//     changed target are NOT discovered. This is correct: new targets require
//     a BUILD file change, which marks the package dirty.
func BuildScopedPattern(labels []string) string {
	if len(labels) == 0 {
		return "set()"
	}
	sort.Strings(labels)
	return "set(" + strings.Join(labels, " ") + ")"
}

// WriteScopedQueryFile writes labels to a temporary file suitable for passing
// to bazel query via --query_file. Returns the file path; the caller is
// responsible for cleanup.
func WriteScopedQueryFile(labels []string) (string, error) {
	if len(labels) == 0 {
		return "", fmt.Errorf("no labels for scoped query")
	}

	sort.Strings(labels)
	pattern := "deps(set(" + strings.Join(labels, " ") + "))"

	f, err := os.CreateTemp("", "td-scoped-query-*.bzl")
	if err != nil {
		return "", fmt.Errorf("failed to create query file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(pattern); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("failed to write query file: %w", err)
	}

	return f.Name(), nil
}

// LoadIncompleteMetadataScoped loads metadata for only the specified dirty*
// labels and their transitive deps. This replaces the full //... query with
// a targeted set() expression, drastically reducing query and parse time for
// typical source-only/BUILD-only changes.
func LoadIncompleteMetadataScoped(context *Context, rev LabelledGitRev, dirtyStarLabels []string) (*QueryResults, func(), error) {
	scopedTargets, err := ParseTargetsList(BuildScopedPattern(dirtyStarLabels))
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to build scoped targets: %w", err)
	}
	return LoadIncompleteMetadata(context, rev, scopedTargets)
}
