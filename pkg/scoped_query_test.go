package pkg

import (
	"strings"
	"testing"
)

func TestBuildScopedUniverseEmpty(t *testing.T) {
	got := BuildScopedUniverse(nil, nil)
	if got != "set()" {
		t.Errorf("empty universe: want %q, got %q", "set()", got)
	}
}

func TestBuildScopedUniversePackagesOnly(t *testing.T) {
	got := BuildScopedUniverse([]string{"//a", "//b/c"}, nil)
	want := "(//a:all + //b/c:all)"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildScopedUniverseRootPackage(t *testing.T) {
	got := BuildScopedUniverse([]string{"//"}, nil)
	if got != "(//:all)" {
		t.Fatalf("BuildScopedUniverse(root) = %q, want %q", got, "(//:all)")
	}
}

func TestBuildScopedUniverseLabelsOnly(t *testing.T) {
	got := BuildScopedUniverse(nil, []string{"//z:z", "//a:a"})
	want := "(set(//a:a //z:z))"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildScopedUniversePackagesAndLabels(t *testing.T) {
	got := BuildScopedUniverse([]string{"//pkg"}, []string{"//app:binary"})
	want := "(//pkg:all + set(//app:binary))"
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
	got, err := ScopeTargetsPattern(original, "(//pkg:all + set(//app:binary))")
	if err != nil {
		t.Fatalf("ScopeTargetsPattern: %v", err)
	}
	want := `let target_determinator_scoped_universe = (//pkg:all + set(//app:binary)) in ($target_determinator_scoped_universe - attr(tags, "manual", $target_determinator_scoped_universe))`
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
