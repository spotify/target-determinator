package pkg

import (
	"fmt"
	"sort"
	"strings"
)

// QuoteQueryWord quotes one target pattern or label for the Bazel query lexer.
// Backslashes do not escape quotes in this grammar. Words containing both quote
// delimiters cannot be represented, so callers must use their safe fallback.
func QuoteQueryWord(word string) (string, error) {
	if !strings.Contains(word, `"`) {
		return `"` + word + `"`, nil
	}
	if !strings.Contains(word, "'") {
		return "'" + word + "'", nil
	}
	return "", fmt.Errorf("cannot quote Bazel query word containing both quote delimiters: %q", word)
}

const scopedUniverseVariable = "target_determinator_scoped_universe"

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
//     change, removed packages are never carried, and macro (.bzl) changes
//     trigger a full-rehash fallback.
//
// The returned expression is parenthesized so it can be substituted into a
// larger query expression. An unquotable label or package returns an error so
// the caller can fall back to a full computation.
func BuildScopedUniverse(dirtyPackages []string, carriedLabels []string) (string, error) {
	terms := make([]string, 0, len(dirtyPackages)+1)
	for _, pkg := range dirtyPackages {
		pattern, err := QuoteQueryWord(pkg + ":all")
		if err != nil {
			return "", err
		}
		terms = append(terms, pattern)
	}
	if len(carriedLabels) > 0 {
		sorted := append([]string(nil), carriedLabels...)
		sort.Strings(sorted)
		for i, label := range sorted {
			word, err := QuoteQueryWord(label)
			if err != nil {
				return "", err
			}
			sorted[i] = word
		}
		terms = append(terms, "set("+strings.Join(sorted, " ")+")")
	}
	if len(terms) == 0 {
		return "set()", nil
	}
	return "(" + strings.Join(terms, " + ") + ")", nil
}

// ScopeTargetsPattern narrows a targets pattern built over //... to the given
// universe expression. The universe is bound once with a Bazel query `let`
// expression, then every occurrence of //... is replaced with a reference to
// that binding. This preserves the semantics of filters expressed over the
// full repo — e.g. `//... - attr(tags, "manual", //...)` becomes
// `let U = (...) in ($U - attr(tags, "manual", $U))` — without duplicating a
// potentially very large universe for each occurrence.
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
	var result strings.Builder
	replacements := 0
	for start := 0; start < len(originalPattern); {
		idx := strings.Index(originalPattern[start:], "//...")
		if idx < 0 {
			result.WriteString(originalPattern[start:])
			break
		}
		idx += start
		result.WriteString(originalPattern[start:idx])
		// Do not rewrite repository-qualified wildcards such as @repo//....
		if idx > 0 && isLabelCharacter(originalPattern[idx-1]) {
			result.WriteString("//...")
		} else {
			result.WriteByte('$')
			result.WriteString(scopedUniverseVariable)
			replacements++
		}
		start = idx + len("//...")
	}
	if replacements == 0 {
		return "", fmt.Errorf("targets pattern %q does not contain //... and cannot be scoped", originalPattern)
	}
	return fmt.Sprintf("let %s = %s in (%s)", scopedUniverseVariable, universe, result.String()), nil
}

func isLabelCharacter(c byte) bool {
	return c == '@' || c == '_' || c == '-' || c == '.' || c == '+' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
