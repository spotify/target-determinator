package pkg

import (
	"os"
	"strings"
	"testing"
)

func TestBuildScopedPatternEmpty(t *testing.T) {
	got := BuildScopedPattern(nil)
	if got != "set()" {
		t.Errorf("empty labels: want %q, got %q", "set()", got)
	}
}

func TestBuildScopedPatternSorted(t *testing.T) {
	got := BuildScopedPattern([]string{"//z:z", "//a:a", "//m:m"})
	want := "set(//a:a //m:m //z:z)"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildScopedPatternSingle(t *testing.T) {
	got := BuildScopedPattern([]string{"//foo:bar"})
	want := "set(//foo:bar)"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestWriteScopedQueryFile(t *testing.T) {
	path, err := WriteScopedQueryFile([]string{"//b:b", "//a:a"})
	if err != nil {
		t.Fatalf("WriteScopedQueryFile: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read query file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "deps(set(//a:a //b:b))") {
		t.Errorf("expected sorted deps(set(...)), got %q", content)
	}
}

func TestWriteScopedQueryFileEmpty(t *testing.T) {
	_, err := WriteScopedQueryFile(nil)
	if err == nil {
		t.Fatal("expected error for empty labels")
	}
}
