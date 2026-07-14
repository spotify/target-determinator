// hash-persister is a binary to compute and persist target hashes for a given git commit SHA.
// This allows for later comparison between commits without recomputing hashes.

package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazel-contrib/target-determinator/cli"
	"github.com/bazel-contrib/target-determinator/pkg"
)

type hashPersisterFlags struct {
	commonFlags *cli.CommonFlags
	commitSha   string
	outputFile  string
	seedFile    string
	seedSha     string
	verifySeed  bool
}

type config struct {
	Context    *pkg.Context
	CommitSha  string
	Targets    pkg.TargetsList
	OutputFile string
	SeedFile   string
	SeedSha    string
	VerifySeed bool
}

func main() {
	start := time.Now()
	defer func() { log.Printf("Finished after %v", time.Since(start)) }()

	flags, err := parseFlags()
	if err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "Failed to parse flags: %v\n", err)
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  %s <git-commit-sha>\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "Where <git-commit-sha> is the commit SHA to compute and persist hashes for.\n")
		fmt.Fprintf(flag.CommandLine.Output(), "Optional flags:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	cfg, err := resolveConfig(*flags)
	if err != nil {
		fmt.Println("Hash Persister invocation Error")
		log.Fatalf("Error during preprocessing: %v", err)
	}

	defer func() {
		innerErr := gitCheckout(cfg.Context.WorkspacePath, cfg.Context.OriginalRevision)
		if innerErr != nil && err == nil {
			err = fmt.Errorf("failed to check out original commit during cleanup: %v", innerErr)
		}
	}()

	if cfg.VerifySeed && cfg.SeedFile != "" {
		runVerifySeed(cfg)
		return
	}

	if cfg.SeedFile != "" {
		runSeeded(cfg)
		return
	}

	runFull(cfg)
}

func runFull(cfg *config) {
	commitRev, err := pkg.NewLabelledGitRev(cfg.Context.WorkspacePath, cfg.CommitSha, "commit")
	if err != nil {
		log.Fatalf("Failed to resolve commit %s: %v", cfg.CommitSha, err)
	}

	log.Printf("Computing hashes for commit %s (full mode)", cfg.CommitSha)

	phaseStart := time.Now()
	queryResults, cleanup, err := pkg.LoadIncompleteMetadata(cfg.Context, commitRev, cfg.Targets)
	defer cleanup()
	if err != nil {
		log.Fatalf("Failed to load metadata for commit %s: %v", cfg.CommitSha, err)
	}
	log.Printf("Phase query+parse completed in %v", time.Since(phaseStart))

	phaseStart = time.Now()
	queryResults.TargetHashCache.HashDebug = cfg.Context.HashDebug
	log.Println("Computing target hashes")
	if err := queryResults.PrefillCache(); err != nil {
		log.Fatalf("Failed to compute hashes for commit %s: %v", cfg.CommitSha, err)
	}
	log.Printf("Phase hash completed in %v (%d targets)",
		time.Since(phaseStart), len(queryResults.MatchingTargets.Labels()))

	phaseStart = time.Now()
	log.Printf("Persisting hashes to %s", cfg.OutputFile)
	if err := pkg.PersistHashes(cfg.OutputFile, cfg.CommitSha, queryResults, cfg.Context, cfg.Targets.String()); err != nil {
		log.Fatalf("Failed to persist hashes: %v", err)
	}
	log.Printf("Phase persist completed in %v", time.Since(phaseStart))

	log.Printf("Successfully persisted hashes for %d targets to %s",
		len(queryResults.MatchingTargets.Labels()), cfg.OutputFile)
}

func runSeeded(cfg *config) {
	log.Printf("Computing hashes for commit %s (seeded from %s)", cfg.CommitSha, cfg.SeedSha)

	phaseStart := time.Now()
	seedData, err := pkg.LoadPersistedHashes(cfg.SeedFile)
	if err != nil {
		log.Fatalf("Failed to load seed file %s: %v", cfg.SeedFile, err)
	}
	log.Printf("Loaded seed file: %d target hashes, %d edges (format v%d) in %v",
		len(seedData.TargetHashes), len(seedData.TargetEdges), seedData.FormatVersion,
		time.Since(phaseStart))

	if seedData.FormatVersion < 9 || seedData.TargetEdges == nil {
		log.Printf("Seed file is v%d (no edges); falling back to full computation",
			seedData.FormatVersion)
		runFull(cfg)
		return
	}

	phaseStart = time.Now()
	changedFiles, err := gitDiffNameStatus(cfg.Context.WorkspacePath, cfg.SeedSha, cfg.CommitSha)
	if err != nil {
		log.Fatalf("Failed to compute git diff: %v", err)
	}
	log.Printf("Git diff: %d changed files in %v", len(changedFiles), time.Since(phaseStart))

	if len(changedFiles) == 0 {
		log.Printf("No files changed; copying seed file as output")
		copyFile(cfg.SeedFile, cfg.OutputFile)
		return
	}

	fingerprintFiles := make(map[string]bool)
	for _, fp := range cfg.Context.RuleClassFingerprints {
		for _, f := range fp.Files {
			fingerprintFiles[f] = true
		}
	}

	phaseStart = time.Now()
	allLabels := pkg.CollectAllLabels(seedData.TargetEdges, seedData.TargetHashes)
	dirtyResult := pkg.ComputeDirtySet(changedFiles, seedData.TargetEdges, allLabels, fingerprintFiles)
	log.Printf("Dirty set computed in %v: %d dirty, %d dirty* (fallback=%v)",
		time.Since(phaseStart), len(dirtyResult.DirtyLabels),
		len(dirtyResult.DirtyStarLabels), dirtyResult.NeedsFallback)

	if dirtyResult.NeedsFallback {
		log.Printf("Fallback triggered: %s", dirtyResult.FallbackReason)
		runFull(cfg)
		return
	}

	commitRev, err := pkg.NewLabelledGitRev(cfg.Context.WorkspacePath, cfg.CommitSha, "commit")
	if err != nil {
		log.Fatalf("Failed to resolve commit %s: %v", cfg.CommitSha, err)
	}

	phaseStart = time.Now()
	dirtyLabels := make([]string, 0, len(dirtyResult.DirtyStarLabels))
	for label := range dirtyResult.DirtyStarLabels {
		dirtyLabels = append(dirtyLabels, label)
	}

	queryResults, cleanup, err := pkg.LoadIncompleteMetadataScoped(cfg.Context, commitRev, dirtyLabels)
	defer cleanup()
	if err != nil {
		log.Printf("Scoped query failed: %v; falling back to full computation", err)
		runFull(cfg)
		return
	}
	log.Printf("Phase scoped query+parse completed in %v", time.Since(phaseStart))

	phaseStart = time.Now()
	seedHashes := make(map[string][]byte)
	for label, configMap := range seedData.TargetHashes {
		if dirtyResult.DirtyStarLabels[label] {
			continue
		}
		for configStr, hashHex := range configMap {
			hashBytes, err := hex.DecodeString(hashHex)
			if err != nil {
				log.Printf("Warning: invalid hash hex for %s: %v", label, err)
				continue
			}
			key := label + "\x00" + configStr
			seedHashes[key] = hashBytes
		}
	}

	if err := queryResults.TargetHashCache.SeedHashes(seedHashes); err != nil {
		log.Fatalf("Failed to seed hashes: %v", err)
	}
	log.Printf("Seeded %d unchanged hashes in %v", len(seedHashes), time.Since(phaseStart))

	phaseStart = time.Now()
	queryResults.TargetHashCache.HashDebug = cfg.Context.HashDebug
	log.Printf("Computing hashes for %d dirty* targets", len(dirtyResult.DirtyStarLabels))
	if err := queryResults.PrefillCache(); err != nil {
		log.Fatalf("Failed to compute hashes: %v", err)
	}
	log.Printf("Phase hash completed in %v", time.Since(phaseStart))

	phaseStart = time.Now()
	mergedHashes := make(map[string]map[string]string)

	for label, configMap := range seedData.TargetHashes {
		if dirtyResult.DirtyStarLabels[label] {
			continue
		}
		if isInDeletedPackage(label, dirtyResult.DeletedPackages) {
			continue
		}
		mergedHashes[label] = configMap
	}

	freshHashes := queryResults.TargetHashCache.ExtractHashes()
	for key, hash := range freshHashes {
		idx := strings.IndexByte(key, '\x00')
		if idx < 0 {
			continue
		}
		label := key[:idx]
		configStr := key[idx+1:]
		if mergedHashes[label] == nil {
			mergedHashes[label] = make(map[string]string)
		}
		mergedHashes[label][configStr] = hex.EncodeToString(hash)
	}

	mergedEdges := make(map[string][]string)
	mergedConfigs := make(map[string]string)

	for label, deps := range seedData.TargetEdges {
		if dirtyResult.DirtyStarLabels[label] || isInDeletedPackage(label, dirtyResult.DeletedPackages) {
			continue
		}
		mergedEdges[label] = deps
	}
	for label, c := range seedData.TargetConfigurations {
		if dirtyResult.DirtyStarLabels[label] || isInDeletedPackage(label, dirtyResult.DeletedPackages) {
			continue
		}
		mergedConfigs[label] = c
	}

	if queryResults.TransitiveConfiguredTargets != nil {
		freshEdges, freshConfigs := pkg.ExtractEdges(queryResults)
		for label, deps := range freshEdges {
			mergedEdges[label] = deps
		}
		for label, c := range freshConfigs {
			mergedConfigs[label] = c
		}
	}

	persistedData := pkg.PersistedHashData{
		FormatVersion:        9,
		GitCommitSha:         cfg.CommitSha,
		Timestamp:            time.Now(),
		BazelRelease:         queryResults.BazelRelease,
		TargetHashes:         mergedHashes,
		TargetEdges:          mergedEdges,
		TargetConfigurations: mergedConfigs,
		Metadata: pkg.HashMetadata{
			TargetsPattern: cfg.Targets.String(),
			WorkspacePath:  cfg.Context.WorkspacePath,
			TotalTargets:   countHashes(mergedHashes),
		},
	}

	if err := pkg.WritePersistedData(cfg.OutputFile, &persistedData); err != nil {
		log.Fatalf("Failed to persist merged hashes: %v", err)
	}
	log.Printf("Phase persist+merge completed in %v", time.Since(phaseStart))

	log.Printf("Successfully persisted %d target hashes to %s (seeded mode, %d recomputed)",
		len(mergedHashes), cfg.OutputFile, len(dirtyResult.DirtyStarLabels))
}

func runVerifySeed(cfg *config) {
	log.Printf("Verify-seed mode: computing seeded AND full hashes for %s", cfg.CommitSha)

	seededOutput, err := os.CreateTemp("", "td-verify-seeded-*.json")
	if err != nil {
		log.Fatalf("Failed to create temp file for seeded output: %v", err)
	}
	seededPath := seededOutput.Name()
	seededOutput.Close()
	defer os.Remove(seededPath)

	fullOutput, err := os.CreateTemp("", "td-verify-full-*.json")
	if err != nil {
		log.Fatalf("Failed to create temp file for full output: %v", err)
	}
	fullPath := fullOutput.Name()
	fullOutput.Close()
	defer os.Remove(fullPath)

	seededCfg := *cfg
	seededCfg.OutputFile = seededPath
	seededCfg.VerifySeed = false
	runSeeded(&seededCfg)

	fullCfg := *cfg
	fullCfg.OutputFile = fullPath
	fullCfg.SeedFile = ""
	fullCfg.SeedSha = ""
	fullCfg.VerifySeed = false
	runFull(&fullCfg)

	result, err := pkg.CompareHashFiles(fullPath, seededPath)
	if err != nil {
		log.Fatalf("Failed to compare hash files: %v", err)
	}

	if len(result.Differences) == 0 {
		log.Printf("VERIFY-SEED: PASS — seeded output matches full computation")
		copyFile(fullPath, cfg.OutputFile)
	} else {
		log.Printf("VERIFY-SEED: FAIL — %d differences found:", len(result.Differences))
		log.Printf("  Changed: %d, Added: %d, Removed: %d",
			result.Summary.TotalChanged, result.Summary.TotalAdded, result.Summary.TotalRemoved)
		for i, diff := range result.Differences {
			if i >= 20 {
				log.Printf("  ... and %d more", len(result.Differences)-20)
				break
			}
			log.Printf("  %s [%s] %s: before=%s after=%s",
				diff.Label, diff.Configuration, diff.Status,
				diff.BeforeHash, diff.AfterHash)
		}
		copyFile(fullPath, cfg.OutputFile)
		os.Exit(1)
	}
}

func parseFlags() (*hashPersisterFlags, error) {
	var flags hashPersisterFlags
	flags.commonFlags = cli.RegisterCommonFlags()
	flag.StringVar(&flags.outputFile, "output", "", "Output file path for persisted hashes (required)")
	flag.StringVar(&flags.seedFile, "seed-file", "", "Path to a seed hash file (v9 format) for incremental hashing")
	flag.StringVar(&flags.seedSha, "seed-sha", "", "Git commit SHA of the seed file (required with --seed-file)")
	flag.BoolVar(&flags.verifySeed, "verify-seed", false, "Run both seeded and full computation, compare, exit non-zero on divergence")

	flag.Parse()

	if flags.outputFile == "" {
		return nil, fmt.Errorf("output file is required")
	}

	if flags.seedFile != "" && flags.seedSha == "" {
		return nil, fmt.Errorf("--seed-sha is required when --seed-file is specified")
	}
	if flags.verifySeed && flags.seedFile == "" {
		return nil, fmt.Errorf("--verify-seed requires --seed-file")
	}

	positional := flag.Args()
	if len(positional) != 1 {
		return nil, fmt.Errorf("expected one positional argument, <git-commit-sha>, but got %d", len(positional))
	}
	flags.commitSha = positional[0]

	return &flags, nil
}

func resolveConfig(flags hashPersisterFlags) (*config, error) {
	if *flags.commonFlags.QueryBackend == "query" && *flags.commonFlags.AnalysisCacheClearStrategy != "skip" {
		return nil, fmt.Errorf("--analysis-cache-clear-strategy=%s is incompatible with --query-backend=query: bazel query does not use the analysis cache", *flags.commonFlags.AnalysisCacheClearStrategy)
	}

	workingDirectory, err := filepath.Abs(*flags.commonFlags.WorkingDirectory)
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory from %v: %w", *flags.commonFlags.WorkingDirectory, err)
	}

	currentBranch, err := pkg.GitRevParse(workingDirectory, "HEAD", true)
	if err != nil {
		return nil, fmt.Errorf("failed to get current git revision: %w", err)
	}

	currentRev, err := pkg.NewLabelledGitRev(workingDirectory, currentBranch, "current")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the current git revision: %w", err)
	}

	bazelCmd := pkg.DefaultBazelCmd{
		BazelPath:        *flags.commonFlags.BazelPath,
		BazelStartupOpts: *flags.commonFlags.BazelStartupOpts,
		BazelOpts:        *flags.commonFlags.BazelOpts,
	}

	outputBase, err := pkg.BazelOutputBase(workingDirectory, bazelCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the bazel output base: %w", err)
	}

	context := &pkg.Context{
		WorkspacePath:                          workingDirectory,
		OriginalRevision:                       currentRev,
		BazelCmd:                               bazelCmd,
		BazelOutputBase:                        outputBase,
		DeleteCachedWorktree:                   flags.commonFlags.DeleteCachedWorktree,
		IgnoredFiles:                           *flags.commonFlags.IgnoredFiles,
		BeforeQueryErrorBehavior:               *flags.commonFlags.BeforeQueryErrorBehavior,
		AnalysisCacheClearStrategy:             *flags.commonFlags.AnalysisCacheClearStrategy,
		CompareQueriesAroundAnalysisCacheClear: flags.commonFlags.CompareQueriesAroundAnalysisCacheClear,
		FilterIncompatibleTargets:              flags.commonFlags.FilterIncompatibleTargets,
		EnforceCleanRepo:                       flags.commonFlags.EnforceCleanRepo == cli.EnforceClean,
		QueryBackend:                           *flags.commonFlags.QueryBackend,
		RuleClassFingerprints:                  []pkg.RuleClassFingerprint(*flags.commonFlags.RuleClassFingerprints),
		HashDebug:                              flags.commonFlags.HashDebug,
	}

	targetsList, err := pkg.ParseTargetsList(*flags.commonFlags.TargetsFlag)
	if err != nil {
		return nil, fmt.Errorf("failed to parse targets: %w", err)
	}

	outputDir := filepath.Dir(flags.outputFile)
	if outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
		}
	}

	return &config{
		Context:    context,
		CommitSha:  flags.commitSha,
		Targets:    targetsList,
		OutputFile: flags.outputFile,
		SeedFile:   flags.seedFile,
		SeedSha:    flags.seedSha,
		VerifySeed: flags.verifySeed,
	}, nil
}

func gitCheckout(workingDirectory string, rev pkg.LabelledGitRev) error {
	gitCmd := exec.Command("git", "checkout", rev.GitRevision.Revision)
	gitCmd.Dir = workingDirectory
	if output, err := gitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to check out %s: %w. Output: %v", rev, err, string(output))
	}
	return nil
}

func gitDiffNameStatus(workingDirectory, fromSha, toSha string) (map[string]string, error) {
	gitCmd := exec.Command("git", "diff", "--name-status", "--no-renames", fromSha+".."+toSha)
	gitCmd.Dir = workingDirectory
	var stdout, stderr bytes.Buffer
	gitCmd.Stdout = &stdout
	gitCmd.Stderr = &stderr
	if err := gitCmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff --name-status %s..%s failed: %w. Stderr: %s",
			fromSha, toSha, err, stderr.String())
	}

	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		result[parts[1]] = parts[0]
	}
	return result, nil
}

func copyFile(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		log.Fatalf("Failed to read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Fatalf("Failed to write %s: %v", dst, err)
	}
}

func countHashes(hashes map[string]map[string]string) int {
	n := 0
	for _, configs := range hashes {
		n += len(configs)
	}
	return n
}

func isInDeletedPackage(label string, deletedPkgs []string) bool {
	for _, p := range deletedPkgs {
		if pkg.LabelInPackageExported(label, p) {
			return true
		}
	}
	return false
}
