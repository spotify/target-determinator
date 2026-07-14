package pkg

import (
	"fmt"
	"sort"
	"strings"
)

// BuildScopedUniverse returns a bazel query expression covering the scoped
// universe of a seeded run:
//
//   - each dirty package contributes a `//pkg:all` wildcard, so that targets
//     added to or removed from the package since the seed are picked up
//     (explicit labels from the seed would miss new targets and error on
//     deleted ones);
//   - carried labels — dirty* targets in unchanged packages (i.e. reverse
//     deps of the dirty packages) — are named explicitly via set(). These
//     labels are guaranteed to still exist because their BUILD files did not
//     change and macro (.bzl) changes trigger a full-rehash fallback.
//
// The returned expression is parenthesized so it can be substituted into a
// larger query expression.
func BuildScopedUniverse(dirtyPackages []string, carriedLabels []string) string {
	terms := make([]string, 0, len(dirtyPackages)+1)
	for _, pkg := range dirtyPackages {
		terms = append(terms, pkg+":all")
	}
	if len(carriedLabels) > 0 {
		sorted := append([]string(nil), carriedLabels...)
		sort.Strings(sorted)
		terms = append(terms, "set("+strings.Join(sorted, " ")+")")
	}
	if len(terms) == 0 {
		return "set()"
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// ScopeTargetsPattern narrows a targets pattern built over //... to the given
// universe expression by substituting every occurrence of //... with the
// universe. This preserves the semantics of filters expressed over the full
// repo — e.g. `//... - attr(tags, "manual", //...)` becomes
// `(U) - attr(tags, "manual", (U))` — while restricting evaluation to the
// scoped universe, so manual-tag filtering behaves identically to a full run.
//
// Patterns that do not contain //... cannot be scoped this way; callers
// should fall back to a full computation in that case.
//
// Known limitations of scoped mode versus a full //... query:
//   - --filter-incompatible-targets is a cquery-only concern; the seeded path
//     is only supported with --query-backend=query, which never filters
//     incompatible targets in full mode either.
//   - Targets in unchanged packages whose rule-generation depends on state
//     outside the package (e.g. macros) are not re-listed; any .bzl change
//     triggers a full-rehash fallback before reaching this point.
func ScopeTargetsPattern(originalPattern string, universe string) (string, error) {
	if !strings.Contains(originalPattern, "//...") {
		return "", fmt.Errorf("targets pattern %q does not contain //... and cannot be scoped", originalPattern)
	}
	return strings.ReplaceAll(originalPattern, "//...", universe), nil
}
