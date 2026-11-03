package pkg

import (
	"bufio"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	gazelle_label "github.com/bazelbuild/bazel-gazelle/label"
)

const (
	// CurrentPersistedHashFormatVersion is the first format with dependency
	// edges and an explicit seed-compatibility fingerprint.
	CurrentPersistedHashFormatVersion = 9
	// HashAlgorithmVersion must change whenever target hashing or persisted
	// dependency semantics change in a way that makes old hashes unsafe to seed.
	HashAlgorithmVersion = 2
)

// PersistedHashData represents the structure of a persisted hash file.
//
// FIELD ORDER MATTERS for performance. encodePersistedData writes fields in
// declaration order, and CompareHashFiles uses a streaming parser that
// stops reading as soon as it has git_commit_sha and target_hashes.
// Keeping TargetHashes before TargetEdges and DependencyHashes lets the
// differ skip ~1 GB of JSON it doesn't need (~370ms vs ~4.5s per file).
// If you reorder these fields, the differ still works correctly but
// degrades to parsing the intervening fields.
type PersistedHashData struct {
	// FormatVersion identifies the persisted format. 0 (absent) = v8; 9 is
	// the first format that supports compatible incremental seeding.
	FormatVersion int `json:"format_version,omitempty"`
	// SeedCompatibilityFingerprint identifies the non-source inputs and hash
	// semantics under which this file was produced.
	SeedCompatibilityFingerprint string `json:"seed_compatibility_fingerprint,omitempty"`
	// GitCommitSha is the git commit SHA this hash data was computed for
	GitCommitSha string `json:"git_commit_sha"`
	// Timestamp when the hash was computed
	Timestamp time.Time `json:"timestamp"`
	// BazelRelease version used for computing hashes
	BazelRelease string `json:"bazel_release"`
	// TargetHashes maps target labels to their configurations and hashes.
	// Keep this before TargetEdges/DependencyHashes — see struct comment.
	TargetHashes map[string]map[string]string `json:"target_hashes"`
	// TargetEdges maps each target label to its direct dependency labels.
	// Present in format_version >= 9. Edges are label-only (configuration-
	// independent) because CI uses --query-backend=query with a single null
	// configuration.
	TargetEdges map[string][]string `json:"target_edges,omitempty"`
	// DependencyHashes holds hashes for labels that appear in TargetEdges
	// without being matching targets, such as manual-tagged deps and
	// platform() rules. Incremental hashing needs them to tell whether such
	// a label changed, but they are not part of the target set and must
	// stay out of TargetHashes, which is what diffing compares.
	DependencyHashes map[string]map[string]string `json:"dependency_hashes,omitempty"`
	// Metadata contains additional information about the computation
	Metadata HashMetadata `json:"metadata"`
}

// SeedHashes returns the recorded per-configuration hashes for a label,
// whether it was persisted as a matching target or as a dependency.
func (d *PersistedHashData) SeedHashes(label string) map[string]string {
	if hashes, ok := d.TargetHashes[label]; ok {
		return hashes
	}
	return d.DependencyHashes[label]
}

// SeedCompatibility describes every invocation-level input that must remain
// identical for persisted hashes to be safely reused. Git revision, timestamp,
// workspace path, and output path are deliberately excluded.
type SeedCompatibility struct {
	HashAlgorithmVersion int                    `json:"hash_algorithm_version"`
	BazelRelease         string                 `json:"bazel_release"`
	TargetsPattern       string                 `json:"targets_pattern"`
	Context              map[string]interface{} `json:"context"`
}

// ComputeSeedCompatibilityFingerprint returns a deterministic identifier for
// the non-source inputs that affect target discovery or hashing.
func ComputeSeedCompatibilityFingerprint(context *Context, targetsPattern, bazelRelease string) (string, error) {
	compatibility := SeedCompatibility{
		HashAlgorithmVersion: HashAlgorithmVersion,
		BazelRelease:         bazelRelease,
		TargetsPattern:       targetsPattern,
		Context:              collectCacheContextFields(context),
	}
	data, err := json.Marshal(compatibility)
	if err != nil {
		return "", fmt.Errorf("failed to marshal seed compatibility: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// HashMetadata contains metadata about the hash computation
type HashMetadata struct {
	// TargetsPattern is the target pattern used (e.g., "//...")
	TargetsPattern string `json:"targets_pattern"`
	// WorkspacePath is the absolute path to the workspace
	WorkspacePath string `json:"workspace_path"`
	// TotalTargets is the number of targets for which hashes were computed
	TotalTargets int `json:"total_targets"`
}

// ExtractEdges extracts the label-level projection of every direct dependency
// used by the target hash function. Multiple configurations are unioned,
// making the graph conservative. Because seeded mode is query-only in normal
// operation, each label ordinarily has a single null configuration.
func ExtractEdges(queryResults *QueryResults) (map[string][]string, error) {
	edges := make(map[string][]string)

	// Rule inputs arrive as strings and leave as strings, but each one has
	// to be canonicalised in between, which parses it into a Label and
	// formats it back. There are an order of magnitude more occurrences
	// than distinct labels, so canonicalise each distinct string once.
	//
	// Shortcutting on the label's shape instead would be wrong: "//pkg:pkg"
	// canonicalises to "//pkg", and skipping that would produce labels
	// inconsistent with how matching targets are recorded.
	canonical := make(map[string]string)
	canonicalise := func(input string) (string, error) {
		if cached, ok := canonical[input]; ok {
			return cached, nil
		}
		parsed, err := queryResults.TargetHashCache.ParseCanonicalLabel(input)
		if err != nil {
			return "", err
		}
		result := parsed.String()
		canonical[input] = result
		return result, nil
	}

	for lbl, configMap := range queryResults.TransitiveConfiguredTargets {
		lblStr := lbl.String()
		for _, ct := range configMap {
			target := ct.GetTarget()
			var dependencies []string
			switch target.GetType() {
			case build.Target_RULE:
				for _, input := range target.GetRule().RuleInput {
					dep, err := canonicalise(input)
					if err != nil {
						return nil, fmt.Errorf("failed to extract dependencies of %s: %w", lblStr, err)
					}
					dependencies = append(dependencies, dep)
				}
			case build.Target_GENERATED_FILE:
				generatingRule, err := canonicalGeneratingRuleLabel(queryResults.TargetHashCache, target.GetGeneratedFile())
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies of %s: %w", lblStr, err)
				}
				dependencies = append(dependencies, generatingRule.String())
			}
			if len(dependencies) > 0 {
				edges[lblStr] = append(edges[lblStr], dependencies...)
			}
		}
	}

	// Deduplicating by sorting and compacting, rather than through a set,
	// avoids hashing every dependency string. The result is sorted either
	// way, and a label may repeat across configurations.
	for lbl, deps := range edges {
		sort.Strings(deps)
		edges[lbl] = slices.Compact(deps)
	}
	return edges, nil
}

// BuildPersistedHashData extracts everything a hash file records from
// queryResults. The result holds no reference to queryResults, so callers
// that are about to write a large file can drop the query graph and hash
// cache first and let the garbage collector reclaim them during the write.
// Write the result with WritePersistedData.
//
// Without seedable, the result is the legacy format: incremental metadata is
// omitted to preserve artifact size. With seedable, it also carries the
// dependency graph and compatibility fingerprint required by incremental
// hashing.
func BuildPersistedHashData(gitCommitSha string, queryResults *QueryResults, context *Context, targetsPattern string, seedable bool) (*PersistedHashData, error) {
	targetHashes := make(map[string]map[string]string)
	totalTargets := 0

	// Extract hashes from QueryResults
	for _, label := range queryResults.MatchingTargets.Labels() {
		configurations := queryResults.MatchingTargets.ConfigurationsFor(label)
		labelStr := label.String()
		targetHashes[labelStr] = make(map[string]string)

		for _, config := range configurations {
			hash, err := queryResults.TargetHashCache.Hash(LabelAndConfiguration{
				Label:         label,
				Configuration: config,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to get hash for target %s with configuration %s: %w", labelStr, config, err)
			}
			targetHashes[labelStr][config.String()] = hex.EncodeToString(hash)
			totalTargets++
		}
	}

	persistedData := PersistedHashData{
		GitCommitSha: gitCommitSha,
		Timestamp:    time.Now(),
		BazelRelease: queryResults.BazelRelease,
		TargetHashes: targetHashes,
		Metadata: HashMetadata{
			TargetsPattern: targetsPattern,
			WorkspacePath:  context.WorkspacePath,
			TotalTargets:   totalTargets,
		},
	}

	if seedable {
		targetEdges, err := ExtractEdges(queryResults)
		if err != nil {
			return nil, fmt.Errorf("failed to extract target edges: %w", err)
		}
		persistedData.DependencyHashes = ExtractDependencyHashes(targetHashes, targetEdges, queryResults.TargetHashCache)
		compatibilityFingerprint, err := ComputeSeedCompatibilityFingerprint(context, targetsPattern, queryResults.BazelRelease)
		if err != nil {
			return nil, err
		}
		persistedData.FormatVersion = CurrentPersistedHashFormatVersion
		persistedData.SeedCompatibilityFingerprint = compatibilityFingerprint
		persistedData.TargetEdges = targetEdges
	}

	return &persistedData, nil
}

// ExtractDependencyHashes returns hashes for labels that appear in the edge
// map but are not matching targets — manual-tagged deps, platform() rules,
// generated file outputs. Their hashes were already computed in the cache
// via recursive Hash() calls, so recording them is free, and without them
// incremental hashing cannot tell whether such a label changed and must
// conservatively propagate its reverse dependencies.
//
// They are returned separately rather than merged into targetHashes: only
// matching targets belong in the target set that diffing compares, and
// mixing these in makes them surface as spurious added targets.
//
// Source files are deliberately excluded. The git diff already says whether
// a file changed, so hashing one to rediscover that is redundant, and they
// outnumber the labels that do need a hash by more than twenty to one.
//
// The caller must finish hashing and all other cache mutations before calling;
// extraction reads the completed cache without locking.
func ExtractDependencyHashes(targetHashes map[string]map[string]string, edges map[string][]string, cache *TargetHashCache) map[string]map[string]string {
	sourceFiles := cache.SourceFileLabels()

	// Both edge keys and dependency values: leaf labels (npm /ref targets)
	// never appear as keys, so iterating keys alone misses them.
	wanted := make(map[string]bool)
	consider := func(label string) {
		if _, ok := targetHashes[label]; ok {
			return
		}
		if sourceFiles[label] {
			return
		}
		wanted[label] = true
	}
	for label, deps := range edges {
		consider(label)
		for _, dep := range deps {
			consider(dep)
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	return cache.extractDependencyHashes(wanted)
}

// WritePersistedData writes a PersistedHashData struct directly to a JSON file.
// Used by the seeded path which builds PersistedHashData by merging seed and
// freshly computed data instead of extracting from QueryResults.
func WritePersistedData(filePath string, data *PersistedHashData) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create hash file %s: %w", filePath, err)
	}
	defer file.Close()

	buffered := bufio.NewWriterSize(file, 1<<20)
	if err := encodePersistedData(buffered, data); err != nil {
		return fmt.Errorf("failed to encode hash data to %s: %w", filePath, err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("failed to write hash data to %s: %w", filePath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close hash file %s: %w", filePath, err)
	}
	return nil
}

// encodePersistedData writes data as compact JSON byte-for-byte identical to
// json.Encoder.Encode, but streams the large maps one entry at a time.
// json.Encoder marshals the whole value into memory before writing anything,
// which for the hash file of a large repository means a buffer of gigabytes on
// top of the data it was built from.
//
// Fields come from PersistedHashData's json tags, in declaration order, so the
// output follows the struct as it changes. Map fields with string keys are
// streamed; every other field is marshalled whole.
//
// TODO: Replace this writer, streamingObjectEncoder, streamMap and jsonFields
// with json.MarshalWrite from encoding/json/v2 once the repository builds with
// Go 1.27 or later, where v2 is no longer behind GOEXPERIMENT. It streams to the
// writer without buffering the whole value. Pass json.DefaultOptionsV1() (or the
// equivalent v1 options, including deterministic map key order) to keep the
// output byte-identical, and measure marshalling speed first: v2 is reported to
// be slower on many small values, which is this file's shape.
func encodePersistedData(w io.Writer, data *PersistedHashData) error {
	enc := &streamingObjectEncoder{w: w}
	enc.begin()
	for _, f := range persistedDataFields {
		v := reflect.ValueOf(data).Elem().FieldByIndex(f.index)
		if f.omitEmpty && isEmptyJSONValue(v) {
			continue
		}
		enc.key(f.name)
		if f.stream {
			streamMap(enc, v)
		} else {
			enc.value(v.Interface())
		}
	}
	enc.end()
	enc.write("\n")
	return enc.err
}

type jsonField struct {
	name      string
	omitEmpty bool
	// stream marks a string-keyed map, written one entry at a time.
	stream bool
	index  []int
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// marshalsItself reports whether encoding/json would encode values of t with
// t's own MarshalJSON or MarshalText rather than by its structure.
func marshalsItself(t reflect.Type) bool {
	for _, candidate := range []reflect.Type{t, reflect.PointerTo(t)} {
		if candidate.Implements(jsonMarshalerType) || candidate.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

var persistedDataFields = jsonFields(reflect.TypeFor[PersistedHashData]())

// jsonFields lists the fields encoding/json would emit for t, which must be a
// struct without embedded fields.
func jsonFields(t reflect.Type) []jsonField {
	var fields []jsonField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		// Checked before exportedness: encoding/json promotes the exported
		// fields of an embedded struct even when its type is unexported.
		if sf.Anonymous {
			panic(fmt.Sprintf("%s.%s: embedded fields are not supported by the streaming writer", t.Name(), sf.Name))
		}
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = sf.Name
		}
		omitEmpty := false
		for _, opt := range strings.Split(opts, ",") {
			switch opt {
			case "omitempty":
				omitEmpty = true
			case "":
			default:
				panic(fmt.Sprintf("%s.%s: json option %q is not supported by the streaming writer", t.Name(), sf.Name, opt))
			}
		}
		stream := sf.Type.Kind() == reflect.Map && sf.Type.Key().Kind() == reflect.String
		if stream {
			// encoding/json would encode such a map with the type's own
			// MarshalJSON or MarshalText, which streamMap does not reproduce.
			if marshalsItself(sf.Type) {
				panic(fmt.Sprintf("%s.%s: map types with their own JSON or text marshalling are not supported by the streaming writer", t.Name(), sf.Name))
			}
			// encoding/json writes such keys with MarshalText, which
			// streamMap does not reproduce.
			key := sf.Type.Key()
			if key.Implements(textMarshalerType) || reflect.PointerTo(key).Implements(textMarshalerType) {
				panic(fmt.Sprintf("%s.%s: map keys implementing encoding.TextMarshaler are not supported by the streaming writer", t.Name(), sf.Name))
			}
		}
		fields = append(fields, jsonField{name: name, omitEmpty: omitEmpty, stream: stream, index: sf.Index})
	}
	return fields
}

// isEmptyJSONValue reports whether encoding/json's omitempty would drop v.
func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

// streamingObjectEncoder emits compact JSON objects member by member.
type streamingObjectEncoder struct {
	w io.Writer
	// first tracks, per open object, whether no member has been written yet.
	first []bool
	err   error
}

func (e *streamingObjectEncoder) write(s string) {
	if e.err == nil {
		_, e.err = io.WriteString(e.w, s)
	}
}

func (e *streamingObjectEncoder) begin() {
	e.write("{")
	e.first = append(e.first, true)
}

func (e *streamingObjectEncoder) end() {
	e.first = e.first[:len(e.first)-1]
	e.write("}")
}

func (e *streamingObjectEncoder) key(name string) {
	top := len(e.first) - 1
	if !e.first[top] {
		e.write(",")
	}
	e.first[top] = false
	e.value(name)
	e.write(":")
}

func (e *streamingObjectEncoder) value(v any) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		e.err = err
		return
	}
	_, e.err = e.w.Write(b)
}

// streamMap writes the string-keyed map m with sorted keys, as encoding/json
// does, marshalling one entry at a time so that only a single value is ever
// buffered.
func streamMap(e *streamingObjectEncoder, m reflect.Value) {
	if m.IsNil() {
		e.write("null")
		return
	}
	keys := m.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int {
		return strings.Compare(a.String(), b.String())
	})
	e.begin()
	for _, k := range keys {
		if e.err != nil {
			return
		}
		e.key(k.String())
		e.value(m.MapIndex(k).Interface())
	}
	e.end()
}

// LoadPersistedHashes loads persisted hash data from a JSON file
func LoadPersistedHashes(filePath string) (*PersistedHashData, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open hash file %s: %w", filePath, err)
	}
	defer file.Close()

	var persistedData PersistedHashData
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&persistedData); err != nil {
		return nil, fmt.Errorf("failed to decode hash data from %s: %w", filePath, err)
	}

	return &persistedData, nil
}

// HashDiff represents a difference between two hash files
type HashDiff struct {
	// Label is the target label
	Label string `json:"label"`
	// Configuration is the configuration checksum
	Configuration string `json:"configuration"`
	// Status indicates the type of change: "added", "removed", "changed"
	Status string `json:"status"`
	// BeforeHash is the hash in the before file (empty for added targets)
	BeforeHash string `json:"before_hash,omitempty"`
	// AfterHash is the hash in the after file (empty for removed targets)
	AfterHash string `json:"after_hash,omitempty"`
}

// HashComparisonResult contains the results of comparing two hash files
type HashComparisonResult struct {
	// BeforeCommit is the git commit SHA of the before hash file
	BeforeCommit string `json:"before_commit"`
	// AfterCommit is the git commit SHA of the after hash file
	AfterCommit string `json:"after_commit"`
	// Differences is a list of all target differences
	Differences []HashDiff `json:"differences"`
	// Summary contains aggregate statistics
	Summary HashComparisonSummary `json:"summary"`
}

// HashComparisonSummary contains summary statistics of the comparison
type HashComparisonSummary struct {
	// TotalChanged is the number of targets that changed
	TotalChanged int `json:"total_changed"`
	// TotalAdded is the number of targets that were added
	TotalAdded int `json:"total_added"`
	// TotalRemoved is the number of targets that were removed
	TotalRemoved int `json:"total_removed"`
	// AffectedTargets is a sorted list of unique target labels that were affected
	AffectedTargets []string `json:"affected_targets"`
	// AfterTargets is a set of target labels that exist in after data
	AfterTargets map[string]bool `json:"after_targets"`
}

// loadHashesForDiff loads only the fields needed for diffing (git_commit_sha
// and target_hashes) using a streaming JSON parser. It stops reading as soon
// as both fields are found, skipping the large target_edges and
// dependency_hashes fields entirely when they appear later in the file.
// Works correctly regardless of field order, but is fastest when
// target_hashes precedes target_edges (see PersistedHashData struct comment).
func loadHashesForDiff(filePath string) (string, map[string]map[string]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to open hash file %s: %w", filePath, err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)

	// Read opening {
	if _, err := dec.Token(); err != nil {
		return "", nil, fmt.Errorf("failed to read opening token from %s: %w", filePath, err)
	}

	var commitSha string
	var hashes map[string]map[string]string

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", nil, fmt.Errorf("failed to read key from %s: %w", filePath, err)
		}
		key, ok := tok.(string)
		if !ok {
			continue
		}

		switch key {
		case "git_commit_sha":
			if err := dec.Decode(&commitSha); err != nil {
				return "", nil, fmt.Errorf("failed to decode git_commit_sha from %s: %w", filePath, err)
			}
			if hashes != nil {
				return commitSha, hashes, nil
			}
		case "target_hashes":
			if err := dec.Decode(&hashes); err != nil {
				return "", nil, fmt.Errorf("failed to decode target_hashes from %s: %w", filePath, err)
			}
			if commitSha != "" {
				return commitSha, hashes, nil
			}
		default:
			// Skip fields we don't need. This allocates and discards the
			// value, which is unavoidable with encoding/json when a needed
			// field appears after an unneeded one.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return "", nil, fmt.Errorf("failed to skip field %q from %s: %w", key, filePath, err)
			}
		}
	}

	if hashes == nil {
		return "", nil, fmt.Errorf("target_hashes not found in %s", filePath)
	}
	return commitSha, hashes, nil
}

// CompareHashFiles compares two persisted hash files and returns the differences
func CompareHashFiles(beforeFile, afterFile string) (*HashComparisonResult, error) {
	// Load files in parallel using the streaming loader
	type loadResult struct {
		commitSha string
		hashes    map[string]map[string]string
		err       error
	}

	beforeChan := make(chan loadResult, 1)
	afterChan := make(chan loadResult, 1)

	go func() {
		sha, hashes, err := loadHashesForDiff(beforeFile)
		beforeChan <- loadResult{commitSha: sha, hashes: hashes, err: err}
	}()

	go func() {
		sha, hashes, err := loadHashesForDiff(afterFile)
		afterChan <- loadResult{commitSha: sha, hashes: hashes, err: err}
	}()

	beforeResult := <-beforeChan
	afterResult := <-afterChan

	if beforeResult.err != nil {
		return nil, fmt.Errorf("failed to load before hash file: %w", beforeResult.err)
	}

	if afterResult.err != nil {
		return nil, fmt.Errorf("failed to load after hash file: %w", afterResult.err)
	}

	beforeHashes := beforeResult.hashes
	afterHashes := afterResult.hashes

	var differences []HashDiff
	affectedTargetsSet := make(map[string]bool)

	// Check for changed and removed targets
	for label, beforeConfigs := range beforeHashes {
		afterConfigs, exists := afterHashes[label]
		if !exists {
			// Target was removed entirely
			for config, beforeHash := range beforeConfigs {
				differences = append(differences, HashDiff{
					Label:         label,
					Configuration: config,
					Status:        "removed",
					BeforeHash:    beforeHash,
				})
				affectedTargetsSet[label] = true
			}
			continue
		}

		// Check each configuration of the target
		for config, beforeHash := range beforeConfigs {
			afterHash, configExists := afterConfigs[config]
			if !configExists {
				// Configuration was removed
				differences = append(differences, HashDiff{
					Label:         label,
					Configuration: config,
					Status:        "removed",
					BeforeHash:    beforeHash,
				})
				affectedTargetsSet[label] = true
			} else if beforeHash != afterHash {
				// Hash changed
				differences = append(differences, HashDiff{
					Label:         label,
					Configuration: config,
					Status:        "changed",
					BeforeHash:    beforeHash,
					AfterHash:     afterHash,
				})
				affectedTargetsSet[label] = true
			}
		}

		// Check for added configurations in existing targets
		for config, afterHash := range afterConfigs {
			if _, configExists := beforeConfigs[config]; !configExists {
				differences = append(differences, HashDiff{
					Label:         label,
					Configuration: config,
					Status:        "added",
					AfterHash:     afterHash,
				})
				affectedTargetsSet[label] = true
			}
		}
	}

	// Check for entirely new targets
	for label, afterConfigs := range afterHashes {
		if _, exists := beforeHashes[label]; !exists {
			for config, afterHash := range afterConfigs {
				differences = append(differences, HashDiff{
					Label:         label,
					Configuration: config,
					Status:        "added",
					AfterHash:     afterHash,
				})
				affectedTargetsSet[label] = true
			}
		}
	}

	// Convert affected targets set to sorted slice
	var affectedTargets []string
	for label := range affectedTargetsSet {
		affectedTargets = append(affectedTargets, label)
	}
	sort.Strings(affectedTargets)

	afterTargetsSet := make(map[string]bool, len(afterHashes))
	for label := range afterHashes {
		afterTargetsSet[label] = true
	}

	// Calculate summary statistics
	summary := HashComparisonSummary{
		AffectedTargets: affectedTargets,
		AfterTargets:    afterTargetsSet,
	}
	for _, diff := range differences {
		switch diff.Status {
		case "added":
			summary.TotalAdded++
		case "removed":
			summary.TotalRemoved++
		case "changed":
			summary.TotalChanged++
		}
	}

	return &HashComparisonResult{
		BeforeCommit: beforeResult.commitSha,
		AfterCommit:  afterResult.commitSha,
		Differences:  differences,
		Summary:      summary,
	}, nil
}

// GetAffectedTargetLabels returns a list of unique target labels that are affected
func (result *HashComparisonResult) GetAffectedTargetLabels() ([]gazelle_label.Label, error) {
	var labels []gazelle_label.Label
	seenLabels := make(map[string]bool)

	for _, diff := range result.Differences {
		if !seenLabels[diff.Label] {
			label, err := gazelle_label.Parse(diff.Label)
			if err != nil {
				return nil, fmt.Errorf("failed to parse label %s: %w", diff.Label, err)
			}
			labels = append(labels, label)
			seenLabels[diff.Label] = true
		}
	}

	return labels, nil
}
