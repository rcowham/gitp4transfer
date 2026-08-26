package main

// repo_p4transfer - Transfer Android repos managed by the "repo" tool to Perforce
// This processes multiple git repositories from a manifest file and imports them to P4
// in chronological order using git fast-export

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rcowham/gitp4transfer/config"

	"github.com/perforce/p4prometheus/version"
	"github.com/rcowham/kingpin"
	"github.com/sirupsen/logrus"
	yaml "gopkg.in/yaml.v2"
)

var (
	app = kingpin.New("repo_p4transfer", "Transfer Android repos to Perforce using git fast-export")

	manifestFile = app.Flag("manifest", "XML manifest file defining repos").
			Default("manifest.xml").String()
	configFile = app.Flag("config", "YAML config file with filtering and P4 settings").
			Required().String()
	repoRoot = app.Flag("repo-root", "Root directory containing all repos").
			Required().String()
	exportDir = app.Flag("export-dir", "Directory where to store export files").
			String()
	logLevel = app.Flag("log-level", "Log level").
			Default("info").String()

	logger *logrus.Logger
)

// RepoManifest represents the Android repo manifest structure
type RepoManifest struct {
	XMLName  xml.Name          `xml:"manifest"`
	Remote   []ManifestRemote  `xml:"remote"`
	Default  ManifestDefault   `xml:"default"`
	Projects []ManifestProject `xml:"project"`
}

type ManifestRemote struct {
	Name  string `xml:"name,attr"`
	Fetch string `xml:"fetch,attr"`
}

type ManifestDefault struct {
	Remote   string `xml:"remote,attr"`
	Revision string `xml:"revision,attr"`
}

type ManifestProject struct {
	Path string `xml:"path,attr"`
	Name string `xml:"name,attr"`
}

// GitExportOptions configures git fast-export behavior
type GitExportOptions struct {
	TagOfFilteredObject string `yaml:"tag_of_filtered_object"`
	SignedTags          string `yaml:"signed_tags"`
	UseDoneFeature      bool   `yaml:"use_done_feature"`
}

// RepoP4TransferConfig combines base config with repo-specific settings
type RepoP4TransferConfig struct {
	DateRange struct {
		StartDate string `yaml:"start_date"`
		EndDate   string `yaml:"end_date"`
	} `yaml:"date_range"`
	Branches       []string               `yaml:"branches"`
	SelectedRepos  []string               `yaml:"selected_repos"`
	ImportDepot    string                 `yaml:"import_depot"`
	ImportPath     string                 `yaml:"import_path"`
	DefaultBranch  string                 `yaml:"default_branch"`
	BranchMappings []config.BranchMapping `yaml:"branch_mappings"`
	TypeMaps       []string               `yaml:"typemaps"`
	GitExport      GitExportOptions       `yaml:"git_export"`
}

// CommitMetadata holds minimal metadata for timeline building
type CommitMetadata struct {
	Hash      string
	Timestamp time.Time
	RepoPath  string
	RepoName  string
	Author    string
	Message   string
	Branch    string
}

// CommitsByTime implements sort.Interface for CommitMetadata
type CommitsByTime []*CommitMetadata

func (c CommitsByTime) Len() int           { return len(c) }
func (c CommitsByTime) Less(i, j int) bool { return c[i].Timestamp.Before(c[j].Timestamp) }
func (c CommitsByTime) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

// RepoP4Transfer orchestrates the entire transfer process
type RepoP4Transfer struct {
	logger       *logrus.Logger
	manifestFile string
	configFile   string
	repoRoot     string
	exportDir    string
	manifest     *RepoManifest
	cfg          *RepoP4TransferConfig
	projects     map[string]*ManifestProject // key: project path
	commits      []*CommitMetadata
	startDate    time.Time
	endDate      time.Time
}

// NewRepoP4Transfer creates a new transfer coordinator
func NewRepoP4Transfer(manifestFile, configFile, repoRoot, exportDir string, logger *logrus.Logger) *RepoP4Transfer {
	return &RepoP4Transfer{
		logger:       logger,
		manifestFile: manifestFile,
		configFile:   configFile,
		repoRoot:     repoRoot,
		exportDir:    exportDir,
		projects:     make(map[string]*ManifestProject),
		commits:      make([]*CommitMetadata, 0),
	}
}

// Init loads manifest and config files
func (r *RepoP4Transfer) Init() error {
	// Load and parse manifest
	if err := r.loadManifest(); err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}

	// Load and parse config
	if err := r.loadConfig(); err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Parse date range
	if err := r.parseDateRange(); err != nil {
		return fmt.Errorf("failed to parse date range: %w", err)
	}

	r.logger.Infof("Loaded %d projects from manifest", len(r.projects))
	r.logger.Infof("Date range: %s to %s", r.startDate.Format("2006-01-02"), r.endDate.Format("2006-01-02"))
	if r.exportDir != "" {
		r.logger.Infof("Export directory: %s", r.exportDir)
	}

	return nil
}

// loadManifest reads and parses the XML manifest file
func (r *RepoP4Transfer) loadManifest() error {
	data, err := os.ReadFile(r.manifestFile)
	if err != nil {
		return fmt.Errorf("failed to read manifest file: %w", err)
	}

	manifest := &RepoManifest{}
	err = xml.Unmarshal(data, manifest)
	if err != nil {
		return fmt.Errorf("failed to parse manifest XML: %w", err)
	}

	r.manifest = manifest

	// Index projects by path for easy lookup
	for i := range manifest.Projects {
		project := &manifest.Projects[i]
		r.projects[project.Path] = project
	}

	return nil
}

// loadConfig reads and parses the YAML config file
func (r *RepoP4Transfer) loadConfig() error {
	data, err := os.ReadFile(r.configFile)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := &RepoP4TransferConfig{}
	err = yaml.Unmarshal(data, cfg)
	if err != nil {
		return fmt.Errorf("failed to parse config YAML: %w", err)
	}

	// Validate git export options
	if err := r.validateGitExportOptions(cfg); err != nil {
		return err
	}

	// If no selected repos specified, use all from manifest
	if len(cfg.SelectedRepos) == 0 {
		for path := range r.projects {
			cfg.SelectedRepos = append(cfg.SelectedRepos, path)
		}
	}

	r.cfg = cfg
	return nil
}

// validateGitExportOptions validates git export configuration
func (r *RepoP4Transfer) validateGitExportOptions(cfg *RepoP4TransferConfig) error {
	validTagModes := map[string]bool{"abort": true, "drop": true, "rewrite": true}
	validSignedModes := map[string]bool{
		"verbatim": true, "warn-verbatim": true, "warn-strip": true,
		"strip": true, "abort": true,
	}

	// Check tag_of_filtered_object if specified
	if cfg.GitExport.TagOfFilteredObject != "" {
		if !validTagModes[cfg.GitExport.TagOfFilteredObject] {
			return fmt.Errorf("invalid git_export.tag_of_filtered_object: %s (must be abort, drop, or rewrite)",
				cfg.GitExport.TagOfFilteredObject)
		}
		r.logger.Debugf("Git export tag_of_filtered_object: %s", cfg.GitExport.TagOfFilteredObject)
	}

	// Check signed_tags if specified
	if cfg.GitExport.SignedTags != "" {
		if !validSignedModes[cfg.GitExport.SignedTags] {
			return fmt.Errorf("invalid git_export.signed_tags: %s (must be verbatim, warn-verbatim, warn-strip, strip, or abort)",
				cfg.GitExport.SignedTags)
		}
		r.logger.Debugf("Git export signed_tags: %s", cfg.GitExport.SignedTags)
	}

	return nil
}

// parseDateRange parses the date range from config
func (r *RepoP4Transfer) parseDateRange() error {
	var err error
	r.startDate, err = time.Parse("2006-01-02", r.cfg.DateRange.StartDate)
	if err != nil {
		return fmt.Errorf("invalid start_date: %w", err)
	}

	r.endDate, err = time.Parse("2006-01-02", r.cfg.DateRange.EndDate)
	if err != nil {
		return fmt.Errorf("invalid end_date: %w", err)
	}

	// Extend end date to include entire day
	r.endDate = r.endDate.Add(24 * time.Hour)

	return nil
}

// BuildTimeline scans all repos and builds a chronological timeline of commits
func (r *RepoP4Transfer) BuildTimeline() error {
	r.logger.Info("Building timeline of commits across all repos...")
	r.logger.Debugf("Scanning %d selected repos", len(r.cfg.SelectedRepos))

	for i, repoPath := range r.cfg.SelectedRepos {
		project, ok := r.projects[repoPath]
		if !ok {
			r.logger.Warnf("Repo %s not found in manifest", repoPath)
			continue
		}

		repoFullPath := filepath.Join(r.repoRoot, repoPath)

		r.logger.Debugf("[%d/%d] Scanning repo: %s", i+1, len(r.cfg.SelectedRepos), repoPath)

		// Check if repo exists
		if _, err := os.Stat(repoFullPath); os.IsNotExist(err) {
			r.logger.Warnf("Repo directory does not exist: %s", repoFullPath)
			continue
		}

		if err := r.scanRepoCommits(project, repoFullPath); err != nil {
			r.logger.Errorf("Failed to scan repo %s: %v", repoPath, err)
			continue
		}
	}

	// Sort commits by timestamp
	r.logger.Debugf("Sorting %d total commits chronologically", len(r.commits))
	sort.Sort(CommitsByTime(r.commits))

	r.logger.Infof("Found %d commits across all repos in date range", len(r.commits))

	if len(r.commits) > 0 {
		r.logger.Debugf("Timeline range: %s to %s",
			r.commits[0].Timestamp.Format("2006-01-02 15:04:05"),
			r.commits[len(r.commits)-1].Timestamp.Format("2006-01-02 15:04:05"))
	}

	return nil
}

// scanRepoCommits scans a single git repo for commits in the date range
func (r *RepoP4Transfer) scanRepoCommits(project *ManifestProject, repoPath string) error {
	r.logger.Debugf("Scanning repo: %s (%s)", project.Path, repoPath)

	// Build git log command to get commits in date range for specified branches
	// Get list of available branches first
	branches, err := r.getRepoBranches(repoPath)
	if err != nil {
		return fmt.Errorf("failed to get branches: %w", err)
	}

	if len(branches) == 0 {
		r.logger.Warnf("No branches found in repo %s", project.Path)
		return nil
	}

	// Filter branches based on config
	selectedBranches := r.filterBranches(branches)
	r.logger.Debugf("Filtered %d branches to %d matching branches", len(branches), len(selectedBranches))

	if len(selectedBranches) == 0 {
		r.logger.Debugf("No matching branches in repo %s after filtering", project.Path)
		return nil
	}

	for i, branch := range selectedBranches {
		r.logger.Debugf("[%d/%d] Scanning branch: %s", i+1, len(selectedBranches), branch)
	}

	// Get commits for each branch
	for _, branch := range selectedBranches {
		if err := r.getCommitsForBranch(project, repoPath, branch); err != nil {
			r.logger.Warnf("Failed to get commits for branch %s in %s: %v", branch, project.Path, err)
			continue
		}
	}

	return nil
}

// executeGitCommand runs a git command and captures stdout and stderr
func (r *RepoP4Transfer) executeGitCommand(repoPath string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.logger.Debugf("[%s] Executing: git %s", repoPath, strings.Join(args, " "))

	err := cmd.Run()
	stderrOutput := stderr.String()

	// Log stderr if present (regardless of error status)
	if stderrOutput != "" {
		if err != nil {
			r.logger.Warnf("[%s] Command stderr: %s", repoPath, stderrOutput)
		} else {
			r.logger.Debugf("[%s] Command stderr: %s", repoPath, stderrOutput)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("git command failed: %w (stderr: %s)", err, stderrOutput)
	}

	return stdout.Bytes(), nil
}

// getRepoBranches lists all branches in a git repository
func (r *RepoP4Transfer) getRepoBranches(repoPath string) ([]string, error) {
	output, err := r.executeGitCommand(repoPath, "branch", "-r")
	if err != nil {
		return nil, fmt.Errorf("failed to run git branch: %w", err)
	}

	branches := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		branch := strings.TrimSpace(scanner.Text())
		// Skip HEAD and remote-tracking branches like origin/HEAD
		if branch != "" && !strings.Contains(branch, "/HEAD") {
			// Remove origin/ prefix to get local branch name
			branch = strings.TrimPrefix(branch, "origin/")
			branches = append(branches, branch)
		}
	}

	r.logger.Debugf("[%s] Found %d branches", repoPath, len(branches))
	return branches, nil
}

// filterBranches filters branches based on config patterns
func (r *RepoP4Transfer) filterBranches(branches []string) []string {
	if len(r.cfg.Branches) == 0 {
		return branches
	}

	var filtered []string
	for _, branch := range branches {
		for _, pattern := range r.cfg.Branches {
			// Simple glob pattern matching
			if matchPattern(branch, pattern) {
				filtered = append(filtered, branch)
				break
			}
		}
	}
	return filtered
}

// matchPattern does simple glob pattern matching (* wildcard)
func matchPattern(s, pattern string) bool {
	if pattern == "*" {
		return true
	}

	if !strings.Contains(pattern, "*") {
		return s == pattern
	}

	parts := strings.Split(pattern, "*")
	if len(parts) == 2 {
		return strings.HasPrefix(s, parts[0]) && strings.HasSuffix(s, parts[1])
	}

	return false
}

// getCommitsForBranch extracts commits from a specific branch within date range
func (r *RepoP4Transfer) getCommitsForBranch(project *ManifestProject, repoPath, branch string) error {
	// Use git log to get commits in date range
	startDateStr := r.startDate.Format("2006-01-02")
	endDateStr := r.endDate.Format("2006-01-02")

	output, err := r.executeGitCommand(repoPath, "log",
		fmt.Sprintf("--since=%s", startDateStr),
		fmt.Sprintf("--until=%s", endDateStr),
		"--format=%H|%aI|%an|%s",
		branch)

	if err != nil {
		// Branch might not exist, just skip it
		r.logger.Debugf("[%s] Branch %s not found or error getting commits", repoPath, branch)
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	commitCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}

		hash := parts[0]
		timestamp, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			r.logger.Warnf("Failed to parse timestamp %s: %v", parts[1], err)
			continue
		}

		author := parts[2]
		message := parts[3]

		commit := &CommitMetadata{
			Hash:      hash,
			Timestamp: timestamp,
			RepoPath:  project.Path,
			RepoName:  project.Name,
			Author:    author,
			Message:   message,
			Branch:    branch,
		}

		r.commits = append(r.commits, commit)
		commitCount++
	}

	if commitCount > 0 {
		r.logger.Debugf("[%s:%s] Found %d commits", project.Path, branch, commitCount)
	}

	return nil
}

// ExportAndImport creates fast-export dumps and imports them to P4
func (r *RepoP4Transfer) ExportAndImport() error {
	r.logger.Info("Starting export and import to P4...")
	r.logger.Infof("Target P4 depot: %s", r.cfg.ImportDepot)

	if len(r.commits) == 0 {
		r.logger.Warn("No commits found to import")
		return nil
	}

	// Group commits by repo for fast-export
	commitsByRepo := r.groupCommitsByRepo()
	r.logger.Debugf("Grouping %d commits into %d repos for batched export", len(r.commits), len(commitsByRepo))

	for repoPath, commits := range commitsByRepo {
		project := r.projects[repoPath]
		repoFullPath := filepath.Join(r.repoRoot, repoPath)

		r.logger.Infof("Processing repo: %s (%d commits)", repoPath, len(commits))
		r.logger.Debugf("  Repo path: %s", project.Path)
		r.logger.Debugf("  Full path: %s", repoFullPath)
		r.logger.Debugf("  First commit: %s at %s", commits[0].Hash[:8], commits[0].Timestamp.Format("2006-01-02 15:04:05"))
		if len(commits) > 1 {
			r.logger.Debugf("  Last commit: %s at %s", commits[len(commits)-1].Hash[:8], commits[len(commits)-1].Timestamp.Format("2006-01-02 15:04:05"))
		}

		if err := r.createAndImportFastExport(project, repoFullPath, commits); err != nil {
			r.logger.Errorf("Failed to export/import repo %s: %v", repoPath, err)
			continue
		}
	}

	r.logger.Info("Export and import completed")
	return nil
}

// groupCommitsByRepo groups commits by their source repo and sorts by timestamp within each repo
func (r *RepoP4Transfer) groupCommitsByRepo() map[string][]*CommitMetadata {
	groups := make(map[string][]*CommitMetadata)
	for _, commit := range r.commits {
		groups[commit.RepoPath] = append(groups[commit.RepoPath], commit)
	}

	// Sort commits within each repo by timestamp (should already be sorted globally, but ensure it per-repo)
	for repoPath, commits := range groups {
		sort.Sort(CommitsByTime(commits))
		r.logger.Debugf("Repo '%s': %d commits", repoPath, len(commits))
	}

	return groups
}

// createAndImportFastExport creates fast-export dump for a repo and imports to P4
func (r *RepoP4Transfer) createAndImportFastExport(project *ManifestProject, repoPath string, commits []*CommitMetadata) error {
	var targetExportDir string
	if r.exportDir != "" {
		targetExportDir = r.exportDir
		if err := os.MkdirAll(targetExportDir, 0755); err != nil {
			return fmt.Errorf("failed to create export directory %s: %w", targetExportDir, err)
		}
	} else {
		targetExportDir = os.TempDir()
	}

	exportFile := filepath.Join(targetExportDir, fmt.Sprintf("export_%s.txt", strings.ReplaceAll(project.Path, "/", "_")))
	if r.exportDir == "" {
		defer os.Remove(exportFile)
	}

	r.logger.Debugf("Creating fast-export dump for %s", project.Path)

	// Get unique commits from the list
	commitHashes := make(map[string]bool)
	var uniqueCommits []string
	for _, commit := range commits {
		if !commitHashes[commit.Hash] {
			commitHashes[commit.Hash] = true
			uniqueCommits = append(uniqueCommits, commit.Hash)
		}
	}

	// Create fast-export dump
	if err := r.createFastExport(repoPath, uniqueCommits, exportFile); err != nil {
		return fmt.Errorf("failed to create fast-export: %w", err)
	}

	r.logger.Debugf("Fast-export dump created: %s", exportFile)

	// TODO: Implement P4 import logic
	// For now, just log the path
	r.logger.Infof("Would import %s to P4 depot %s at path %s", exportFile, r.cfg.ImportDepot, project.Path)

	return nil
}

// createFastExport creates a git fast-export dump file with configured options
func (r *RepoP4Transfer) createFastExport(repoPath string, commitHashes []string, outputFile string) error {
	// Build git fast-export command with configured options
	args := []string{"fast-export"}

	// Add tag handling option if specified
	if r.cfg.GitExport.TagOfFilteredObject != "" {
		args = append(args, fmt.Sprintf("--tag-of-filtered-object=%s", r.cfg.GitExport.TagOfFilteredObject))
		r.logger.Debugf("[%s] Using --tag-of-filtered-object=%s", repoPath, r.cfg.GitExport.TagOfFilteredObject)
	}

	// Add signed tags handling option if specified
	if r.cfg.GitExport.SignedTags != "" {
		args = append(args, fmt.Sprintf("--signed-tags=%s", r.cfg.GitExport.SignedTags))
		r.logger.Debugf("[%s] Using --signed-tags=%s", repoPath, r.cfg.GitExport.SignedTags)
	}

	// Add done feature if enabled
	if r.cfg.GitExport.UseDoneFeature {
		args = append(args, "--use-done-feature")
		r.logger.Debugf("[%s] Using --use-done-feature for resumable exports", repoPath)
	}

	args = append(args, "--all")
	args = append(args, commitHashes...)

	output, err := r.executeGitCommand(repoPath, args...)
	if err != nil {
		return fmt.Errorf("git fast-export failed: %w", err)
	}

	r.logger.Debugf("[%s] Fast-export output size: %d bytes", repoPath, len(output))

	if err := os.WriteFile(outputFile, output, 0644); err != nil {
		return fmt.Errorf("failed to write export file: %w", err)
	}

	r.logger.Debugf("[%s] Export file written to: %s", repoPath, outputFile)

	return nil
}

// PrintStats prints statistics about the transfer
func (r *RepoP4Transfer) PrintStats() {
	r.logger.Info("=== Transfer Statistics ===")
	r.logger.Infof("Total commits: %d", len(r.commits))
	r.logger.Infof("Total repos: %d", len(r.projects))
	r.logger.Infof("Selected repos: %d", len(r.cfg.SelectedRepos))

	// Count commits per repo
	commitsByRepo := r.groupCommitsByRepo()
	if len(commitsByRepo) > 0 {
		r.logger.Infof("Repos with commits: %d", len(commitsByRepo))
		for repoPath, commits := range commitsByRepo {
			r.logger.Infof("  %s: %d commits", repoPath, len(commits))
		}
	} else {
		r.logger.Info("No repos have commits in the date range")
	}

	r.logger.Debugf("Branch filters: %v", r.cfg.Branches)
	r.logger.Debugf("Date range: %s to %s", r.startDate.Format("2006-01-02"), r.endDate.Format("2006-01-02"))
}

// minInt returns the minimum of two integers
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func init() {
	app.Version(version.Print("repo_p4transfer"))
}

func main() {
	kingpin.MustParse(app.Parse(os.Args[1:]))

	// Setup logger
	logger = logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid log level: %s\n", *logLevel)
		os.Exit(1)
	}
	logger.SetLevel(level)

	// Create transfer coordinator
	transfer := NewRepoP4Transfer(*manifestFile, *configFile, *repoRoot, *exportDir, logger)

	// Initialize from config and manifest
	if err := transfer.Init(); err != nil {
		logger.Fatalf("Initialization failed: %v", err)
	}

	// Build timeline of commits
	if err := transfer.BuildTimeline(); err != nil {
		logger.Fatalf("Timeline building failed: %v", err)
	}

	// Print statistics
	transfer.PrintStats()

	// Export and import
	if err := transfer.ExportAndImport(); err != nil {
		logger.Fatalf("Export/import failed: %v", err)
	}

	logger.Info("Transfer completed successfully")
}
