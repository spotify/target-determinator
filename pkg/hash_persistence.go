package pkg

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	gazelle_label "github.com/bazelbuild/bazel-gazelle/label"
)

// PersistedHashData represents the structure of a persisted hash file
type PersistedHashData struct {
	// FormatVersion identifies the persisted format. 0 (absent) = v8; 9 = v9+edges.
	FormatVersion int `json:"format_version,omitempty"`
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
	// TargetConfigurations maps each target label to its configuration checksum.
	// Present in format_version >= 9.
	TargetConfigurations map[string]string `json:"target_configurations,omitempty"`
	// Metadata contains additional information about the computation
	Metadata HashMetadata `json:"metadata"`
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

// ExtractEdges extracts direct-dependency edges and configuration info from
// QueryResults. Edges use label strings as keys and values; configurations
// map each label to its configuration checksum. Because CI operates in query
// mode (single null configuration), edges are configuration-independent.
func ExtractEdges(queryResults *QueryResults) (edges map[string][]string, configs map[string]string) {
	edges = make(map[string][]string)
	configs = make(map[string]string)

	for lbl, configMap := range queryResults.TransitiveConfiguredTargets {
		lblStr := lbl.String()
		for cfg, ct := range configMap {
			configs[lblStr] = cfg.String()
			target := ct.GetTarget()
			if target.GetType() == build.Target_RULE {
				rule := target.GetRule()
				deps := make([]string, 0, len(rule.RuleInput))
				for _, input := range rule.RuleInput {
					deps = append(deps, input)
				}
				sort.Strings(deps)
				if len(deps) > 0 {
					edges[lblStr] = deps
				}
			}
		}
	}
	return edges, configs
}

// PersistHashes saves the computed hashes to a JSON file
func PersistHashes(filePath string, gitCommitSha string, queryResults *QueryResults, context *Context, targetsPattern string) error {
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

	// Extract edges for v9 format
	var targetEdges map[string][]string
	var targetConfigurations map[string]string
	if queryResults.TransitiveConfiguredTargets != nil {
		targetEdges, targetConfigurations = ExtractEdges(queryResults)
	}

	persistedData := PersistedHashData{
		FormatVersion:        9,
		GitCommitSha:         gitCommitSha,
		Timestamp:            time.Now(),
		BazelRelease:         queryResults.BazelRelease,
		TargetHashes:         targetHashes,
		TargetEdges:          targetEdges,
		TargetConfigurations: targetConfigurations,
		Metadata: HashMetadata{
			TargetsPattern: targetsPattern,
			WorkspacePath:  context.WorkspacePath,
			TotalTargets:   totalTargets,
		},
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create hash file %s: %w", filePath, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(persistedData); err != nil {
		return fmt.Errorf("failed to encode hash data to %s: %w", filePath, err)
	}

	return nil
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

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
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
