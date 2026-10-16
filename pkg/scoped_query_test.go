package pkg

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildScopedUniverseEmpty(t *testing.T) {
	got, err := BuildScopedUniverse(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "set()" {
		t.Errorf("empty universe: want %q, got %q", "set()", got)
	}
}

func TestBuildScopedUniversePackagesOnly(t *testing.T) {
	got, err := BuildScopedUniverse([]string{"//a", "//b/c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `("//a:all" + "//b/c:all")`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildScopedUniverseRootPackage(t *testing.T) {
	got, err := BuildScopedUniverse([]string{"//"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != `("//:all")` {
		t.Fatalf("BuildScopedUniverse(root) = %q, want %q", got, `("//:all")`)
	}
}

func TestBuildScopedUniverseLabelsOnly(t *testing.T) {
	got, err := BuildScopedUniverse(nil, []string{"//z:z", "//a:a"})
	if err != nil {
		t.Fatal(err)
	}
	want := `(set("//a:a" "//z:z"))`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildScopedUniversePackagesAndLabels(t *testing.T) {
	got, err := BuildScopedUniverse([]string{"//pkg"}, []string{"//app:binary"})
	if err != nil {
		t.Fatal(err)
	}
	want := `("//pkg:all" + set("//app:binary"))`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestScopeTargetsPatternPlain(t *testing.T) {
	got, err := ScopeTargetsPattern("//...", "(//pkg:all)")
	if err != nil {
		t.Fatalf("ScopeTargetsPattern: %v", err)
	}
	want := "let target_determinator_scoped_universe = (//pkg:all) in ($target_determinator_scoped_universe)"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestScopeTargetsPatternManualFilter(t *testing.T) {
	original := `//... - attr(tags, "manual", //...)`
	got, err := ScopeTargetsPattern(original, `("//pkg:all" + set("//app:binary"))`)
	if err != nil {
		t.Fatalf("ScopeTargetsPattern: %v", err)
	}
	want := `let target_determinator_scoped_universe = ("//pkg:all" + set("//app:binary")) in ($target_determinator_scoped_universe - attr(tags, "manual", $target_determinator_scoped_universe))`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
	if strings.Count(got, "//app:binary") != 1 {
		t.Fatalf("scoped universe was repeated in query: %q", got)
	}
}

func TestScopeTargetsPatternNoWildcard(t *testing.T) {
	if _, err := ScopeTargetsPattern("//app:binary", "(//pkg:all)"); err == nil {
		t.Fatal("expected error for pattern without //...")
	}
	if _, err := ScopeTargetsPattern("@repo//...", "(//pkg:all)"); err == nil {
		t.Fatal("expected error for pattern containing only a repository-qualified wildcard")
	}
}

func TestScopeTargetsPatternDoesNotRewriteExternalWildcard(t *testing.T) {
	for _, repositoryWildcard := range []string{"@repo//...", "@@canonical_repo//..."} {
		t.Run(repositoryWildcard, func(t *testing.T) {
			got, err := ScopeTargetsPattern(repositoryWildcard+" + //...", "(//pkg:all)")
			if err != nil {
				t.Fatal(err)
			}
			want := "let target_determinator_scoped_universe = (//pkg:all) in (" + repositoryWildcard + " + $target_determinator_scoped_universe)"
			if got != want {
				t.Fatalf("ScopeTargetsPattern = %q, want %q", got, want)
			}
		})
	}
}

func TestQuoteQueryWord(t *testing.T) {
	for _, tc := range []struct{ name, word, want string }{
		{"ordinary", "//pkg:target", `"//pkg:target"`},
		{"plus_equals", "//pkg+other=one:target", `"//pkg+other=one:target"`},
		{"single_quote", "//pkg:it's", `"//pkg:it's"`},
		{"double_quote", `//pkg:say"hi`, `'//pkg:say"hi'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := QuoteQueryWord(tc.word)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	_, err := QuoteQueryWord(`//pkg:both'"`)
	require.Error(t, err)
}

func TestBuildScopedUniverseGeneratedLabels(t *testing.T) {
	labels := []string{
		"//:.aspect_rules_js/node_modules/@work-graph+core@0.0.0/ref",
		"//:.aspect_rules_js/node_modules/@work-graph+core@0.0.0/pkg",
	}
	original := append([]string(nil), labels...)
	got, err := BuildScopedUniverse([]string{"//pkg+other=one"}, labels)
	require.NoError(t, err)
	want := `("//pkg+other=one:all" + set("//:.aspect_rules_js/node_modules/@work-graph+core@0.0.0/pkg" "//:.aspect_rules_js/node_modules/@work-graph+core@0.0.0/ref"))`
	require.Equal(t, want, got)
	require.Equal(t, original, labels, "must not change caller's label order")
}

func TestBuildScopedUniverseUnquotableWord(t *testing.T) {
	for _, tc := range []struct{ packages, labels []string }{
		{packages: []string{`//both'"`}},
		{labels: []string{`//pkg:both'"`}},
	} {
		got, err := BuildScopedUniverse(tc.packages, tc.labels)
		require.Error(t, err)
		require.Empty(t, got)
	}
}
