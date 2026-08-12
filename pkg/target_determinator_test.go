package pkg

import (
	"os"
	"strings"
	"testing"

	"github.com/bazel-contrib/target-determinator/common"
)

func TestQueryPatternArgsInline(t *testing.T) {
	args, cleanup, err := queryPatternArgs("//...")
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args[0] != "//..." {
		t.Fatalf("inline query args = %v, want [//...]", args)
	}
}

func TestQueryPatternArgsUsesAndCleansUpFile(t *testing.T) {
	pattern := strings.Repeat("x", maxInlineQueryPatternLength+1)
	args, cleanup, err := queryPatternArgs(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || !strings.HasPrefix(args[0], "--query_file=") {
		cleanup()
		t.Fatalf("query file args = %v", args)
	}
	path := strings.TrimPrefix(args[0], "--query_file=")
	data, err := os.ReadFile(path)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if string(data) != pattern {
		cleanup()
		t.Fatal("query file did not contain the complete pattern")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("query file still exists after cleanup: %v", err)
	}
}

func Test_stringSliceContainsStartingWith(t *testing.T) {
	type args struct {
		slice   []common.RelPath
		element common.RelPath
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			"containsExact",
			args{
				[]common.RelPath{common.NewRelPath("foo")},
				common.NewRelPath("foo"),
			},
			true,
		},
		{
			"containsDirWithoutTrailingSlash",
			args{
				[]common.RelPath{common.NewRelPath("foo"), common.NewRelPath("bar/baz")},
				common.NewRelPath("foo/"),
			},
			true,
		},
		{
			"containsDirWithTrailingSlashButIsFile",
			args{
				[]common.RelPath{common.NewRelPath("foo/")},
				common.NewRelPath("foo"),
			},
			false,
		},
		{
			"containsPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo")},
				common.NewRelPath("foo/bar"),
			},
			true,
		},
		{
			"otherIsPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo/bar")},
				common.NewRelPath("foo"),
			},
			false,
		},
		{
			"doesNotContain",
			args{
				[]common.RelPath{common.NewRelPath("foo"), common.NewRelPath("bar/baz")},
				common.NewRelPath("frob"),
			},
			false,
		},
		{
			"stringPrefixButNotPathPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo/b")},
				common.NewRelPath("foo/bar"),
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringSliceContainsStartingWith(tt.args.slice, tt.args.element); got != tt.want {
				t.Errorf("stringSliceContainsStartingWith() with (slice = %v, element = %v) returns %v, want %v",
					tt.args.slice, tt.args.element.String(), got, tt.want)
			}
		})
	}
}

func Test_ParseCanonicalLabel(t *testing.T) {
	n := Normalizer{}
	for _, tt := range []string{
		"@//label",
		"@//label:package",
		"//label:package",
		":package",
		"@rules_python~0.21.0~pip~pip_boto3//:pkg",
	} {
		_, err := n.ParseCanonicalLabel(tt)
		if err != nil {
			t.Errorf("ParseCanonicalLabel() with (label=%s) produces error %s", tt, err)
		}
	}
}
