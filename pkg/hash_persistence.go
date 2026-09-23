package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
	HashAlgorithmVersion = 1
)

// PersistedHashData represents the structure of a persisted hash file
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
	// TargetHashes maps target labels to their configurations and hashes
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
	edgeSets := make(map[string]map[string]struct{})
	edges := make(map[string][]string)

	for lbl, configMap := range queryResults.TransitiveConfiguredTargets {
		lblStr := lbl.String()
		for _, ct := range configMap {
			target := ct.GetTarget()
			var dependencyLabels []gazelle_label.Label
			switch target.GetType() {
			case build.Target_RULE:
				var err error
				dependencyLabels, err = canonicalRuleInputLabels(queryResults.TargetHashCache, target.GetRule())
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies of %s: %w", lblStr, err)
				}
			case build.Target_GENERATED_FILE:
				generatingRule, err := canonicalGeneratingRuleLabel(queryResults.TargetHashCache, target.GetGeneratedFile())
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies of %s: %w", lblStr, err)
				}
				dependencyLabels = append(dependencyLabels, generatingRule)
			}
			if len(dependencyLabels) > 0 {
				if edgeSets[lblStr] == nil {
					edgeSets[lblStr] = make(map[string]struct{})
				}
				for _, dep := range dependencyLabels {
					edgeSets[lblStr][dep.String()] = struct{}{}
				}
			}
		}
	}
	for lbl, deps := range edgeSets {
		for dep := range deps {
			edges[lbl] = append(edges[lbl], dep)
		}
		sort.Strings(edges[lbl])
	}
	return edges, nil
}

// PersistHashes saves computed hashes in the legacy format used by existing
// full-mode callers. Incremental metadata is omitted to preserve artifact size
// and behavior unless the caller explicitly requests a seedable artifact.
func PersistHashes(filePath string, gitCommitSha string, queryResults *QueryResults, context *Context, targetsPattern string) error {
	return persistHashes(filePath, gitCommitSha, queryResults, context, targetsPattern, false)
}

// PersistSeedableHashes saves computed hashes together with the dependency
// graph and compatibility fingerprint required by incremental hashing.
func PersistSeedableHashes(filePath string, gitCommitSha string, queryResults *QueryResults, context *Context, targetsPattern string) error {
	return persistHashes(filePath, gitCommitSha, queryResults, context, targetsPattern, true)
}

func persistHashes(filePath string, gitCommitSha string, queryResults *QueryResults, context *Context, targetsPattern string, seedable bool) error {
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
				return fmt.Errorf("failed to get hash for target %s with configuration %s: %w", labelStr, config, err)
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
			return fmt.Errorf("failed to extract target edges: %w", err)
		}
		persistedData.DependencyHashes = ExtractDependencyHashes(targetHashes, targetEdges, queryResults.TargetHashCache)
		compatibilityFingerprint, err := ComputeSeedCompatibilityFingerprint(context, targetsPattern, queryResults.BazelRelease)
		if err != nil {
			return err
		}
		persistedData.FormatVersion = CurrentPersistedHashFormatVersion
		persistedData.SeedCompatibilityFingerprint = compatibilityFingerprint
		persistedData.TargetEdges = targetEdges
	}

	return writePersistedData(filePath, &persistedData, !seedable)
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

	dependencyHashes := make(map[string]map[string]string)
	for key, hashHex := range cache.ExtractHexHashes() {
		label, configuration, ok := splitHashKey(key)
		if !ok || !wanted[label] {
			continue
		}
		// The cache yields a zero-length sentinel rather than a hash for
		// labels whose file does not exist or is a directory. Persisting
		// one would fail seed validation, which requires every hash to be
		// sha256-sized, and cause the whole seed to be rejected. Omitting
		// the label instead just makes incremental hashing treat it as
		// changed, which is the safe direction.
		if len(hashHex) != hex.EncodedLen(sha256.Size) {
			continue
		}
		configs := dependencyHashes[label]
		if configs == nil {
			configs = make(map[string]string)
			dependencyHashes[label] = configs
		}
		configs[configuration] = hashHex
	}
	return dependencyHashes
}

// WritePersistedData writes a PersistedHashData struct directly to a JSON file.
// Used by the seeded path which builds PersistedHashData by merging seed and
// freshly computed data instead of extracting from QueryResults.
func WritePersistedData(filePath string, data *PersistedHashData) error {
	return writePersistedData(filePath, data, false)
}

func writePersistedData(filePath string, data *PersistedHashData, pretty bool) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create hash file %s: %w", filePath, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode hash data to %s: %w", filePath, err)
	}
	return nil
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

// CompareHashFiles compares two persisted hash files and returns the differences
func CompareHashFiles(beforeFile, afterFile string) (*HashComparisonResult, error) {
	// Load files in parallel
	type loadResult struct {
		data *PersistedHashData
		err  error
	}

	beforeChan := make(chan loadResult, 1)
	afterChan := make(chan loadResult, 1)

	go func() {
		data, err := LoadPersistedHashes(beforeFile)
		beforeChan <- loadResult{data: data, err: err}
	}()

	go func() {
		data, err := LoadPersistedHashes(afterFile)
		afterChan <- loadResult{data: data, err: err}
	}()

	beforeResult := <-beforeChan
	afterResult := <-afterChan

	if beforeResult.err != nil {
		return nil, fmt.Errorf("failed to load before hash file: %w", beforeResult.err)
	}

	if afterResult.err != nil {
		return nil, fmt.Errorf("failed to load after hash file: %w", afterResult.err)
	}

	beforeData := beforeResult.data
	afterData := afterResult.data

	var differences []HashDiff
	affectedTargetsSet := make(map[string]bool)

	// Check for changed and removed targets
	for label, beforeConfigs := range beforeData.TargetHashes {
		afterConfigs, exists := afterData.TargetHashes[label]
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
	for label, afterConfigs := range afterData.TargetHashes {
		if _, exists := beforeData.TargetHashes[label]; !exists {
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

	afterTargetsSet := make(map[string]bool, len(afterData.TargetHashes))
	for label := range afterData.TargetHashes {
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
		BeforeCommit: beforeData.GitCommitSha,
		AfterCommit:  afterData.GitCommitSha,
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
