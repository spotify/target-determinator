package pkg

import (
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
	if got != "(//pkg:all)" {
		t.Errorf("want %q, got %q", "(//pkg:all)", got)
	}
}

func TestScopeTargetsPatternManualFilter(t *testing.T) {
	original := `//... - attr(tags, "manual", //...)`
	got, err := ScopeTargetsPattern(original, "(//pkg:all + set(//app:binary))")
	if err != nil {
		t.Fatalf("ScopeTargetsPattern: %v", err)
	}
	want := `(//pkg:all + set(//app:binary)) - attr(tags, "manual", (//pkg:all + set(//app:binary)))`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestScopeTargetsPatternNoWildcard(t *testing.T) {
	if _, err := ScopeTargetsPattern("//app:binary", "(//pkg:all)"); err == nil {
		t.Fatal("expected error for pattern without //...")
	}
}

func TestScopeTargetsPatternDoesNotRewriteExternalWildcard(t *testing.T) {
	got, err := ScopeTargetsPattern("@repo//... + //...", "(//pkg:all)")
	if err != nil {
		t.Fatal(err)
	}
	want := "@repo//... + (//pkg:all)"
	if got != want {
		t.Fatalf("ScopeTargetsPattern = %q, want %q", got, want)
	}
}
