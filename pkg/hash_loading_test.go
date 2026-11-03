package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadPersistedHashesMatchesJSONDecoder(t *testing.T) {
	cases := []string{
		`{}`,
		`null`,
		`{"target_hashes":null,"target_edges":{},"dependency_hashes":null}`,
		`{"target_hashes":{"//pkg:a":{"cfg1":"01"},"//pkg:b":null},"target_edges":{"//pkg:a":[],"//pkg:b":null}}`,
		`{"target_hashes":{"//pkg:a":{"cfg1":"01"}},"target_hashes":{"//pkg:b":{"cfg2":"02"}}}`,
		`{"target_hashes":{"//pkg:a":{"cfg1":"01"},"//pkg:a":{"cfg2":"02"}}}`,
		`{"target_hashes":{"//pkg:a":{"":"01"}},"target_hashes":null,"target_hashes":{"//pkg:b":{"":"02"}}}`,
		`{"TARGET_HASHES":{"//pkg:a":{"":"01"}},"TARGET_EDGES":{"//pkg:a":["//pkg:b"]},"Git_Commit_Sha":"seed"}`,
		`{"target_haſhes":{"//pkg:a":{"":"01"}}}`,
		`{"future":{"nested":[1e1000,null,true,{"quoted":"\\\""}]},"target_hashes":{"//pkg:a":{"":"01"}}}`,
		`{"timestamp":"2026-11-03T12:00:00Z","metadata":{"total_targets":1},"metadata":{"workspace_path":"/ws"}}`,
		`{"target_hashes":{}} ignored trailing content`,
		``, `[]`, `123`, `"string"`,
		`{"target_hashes":[]}`, `{"target_hashes":{"//pkg:a":[]}}`,
		`{"target_edges":{"//pkg:a":123}}`, `{"format_version":"nine"}`,
		`{"timestamp":"bad"}`, `{"future":[1,}`, `{"target_hashes":{`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			var want PersistedHashData
			wantErr := json.NewDecoder(strings.NewReader(input)).Decode(&want)
			path := filepath.Join(t.TempDir(), "seed.json")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			got, gotErr := LoadPersistedHashes(path)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("decoder behavior changed: standard=%v streamed=%v", wantErr, gotErr)
			}
			if gotErr == nil && !reflect.DeepEqual(got, &want) {
				t.Fatalf("decoded data changed: got=%#v want=%#v", got, &want)
			}
		})
	}
}

func BenchmarkLoadPersistedHashes(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("targets_%d", count), func(b *testing.B) {
			data := &PersistedHashData{TargetHashes: make(map[string]map[string]string, count), TargetEdges: make(map[string][]string, count)}
			for i := 0; i < count; i++ {
				label := fmt.Sprintf("//representative/repository/package_%06d:target", i)
				data.TargetHashes[label] = map[string]string{"": strings.Repeat("01", 32)}
				data.TargetEdges[label] = []string{label + "_source1", label + "_source2", label + "_dependency"}
			}
			path := filepath.Join(b.TempDir(), "seed.json")
			if err := WritePersistedData(path, data); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := LoadPersistedHashes(path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestLoadHashesForDiffSharedReader(t *testing.T) {
	for name, input := range map[string]string{
		"hashes first":         `{"target_hashes":{"//pkg:a":{"":"01"}},"git_commit_sha":"seed"}`,
		"unknown fields first": `{"future":{"nested":[1e1000,null,{"x":[1,2]}]},"target_edges":{"//pkg:a":["//pkg:b"]},"git_commit_sha":"seed","target_hashes":{"//pkg:a":{"":"01"}}}`,
		"stop before graph":    `{"git_commit_sha":"seed","target_hashes":{"//pkg:a":{"":"01"}},"target_edges":invalid`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hashes.json")
			require.NoError(t, os.WriteFile(path, []byte(input), 0600))
			sha, hashes, err := loadHashesForDiff(path)
			require.NoError(t, err)
			require.Equal(t, "seed", sha)
			require.Equal(t, map[string]map[string]string{"//pkg:a": {"": "01"}}, hashes)
			if name == "stop before graph" {
				_, err := LoadPersistedHashes(path)
				require.Error(t, err)
			}
		})
	}
	for _, input := range []string{`{}`, `null`, `{"git_commit_sha":"seed","target_hashes":null}`, `{"target_hashes":[]}`, `{"future":[1,}`} {
		path := filepath.Join(t.TempDir(), "invalid.json")
		require.NoError(t, os.WriteFile(path, []byte(input), 0600))
		_, _, err := loadHashesForDiff(path)
		require.Error(t, err)
	}
}

func BenchmarkLoadHashesForDiff(b *testing.B) {
	data := &PersistedHashData{GitCommitSha: "seed", TargetHashes: make(map[string]map[string]string, 100000), TargetEdges: make(map[string][]string, 100000)}
	for i := 0; i < 100000; i++ {
		label := fmt.Sprintf("//representative/package_%06d:target", i)
		data.TargetHashes[label] = map[string]string{"": strings.Repeat("01", 32)}
		data.TargetEdges[label] = []string{label + "_source", label + "_dependency"}
	}
	for _, graphFirst := range []bool{false, true} {
		b.Run(fmt.Sprintf("graph_first_%t", graphFirst), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "hashes.json")
			var content []byte
			var err error
			if graphFirst {
				content, err = json.Marshal(struct {
					Edges  map[string][]string          `json:"target_edges"`
					Sha    string                       `json:"git_commit_sha"`
					Hashes map[string]map[string]string `json:"target_hashes"`
				}{data.TargetEdges, data.GitCommitSha, data.TargetHashes})
			} else {
				content, err = json.Marshal(data)
			}
			if err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := loadHashesForDiff(path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
