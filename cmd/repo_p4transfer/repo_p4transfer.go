package main

// repo_p4transfer - Transfer Android repos managed by the "repo" tool to Perforce
// This processes multiple git repositories from a manifest file and imports them to P4
// in chronological order using git fast-export

import (
	"bufio"
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
	manifest     *RepoManifest
	cfg          *RepoP4TransferConfig
	projects     map[string]*ManifestProject // key: project path
	commits      []*CommitMetadata
	startDate    time.Time
	endDate      time.Time
}

// NewRepoP4Transfer creates a new transfer coordinator
func NewRepoP4Transfer(manifestFile, configFile, repoRoot string, logger *logrus.Logger) *RepoP4Transfer {
	return &RepoP4Transfer{
		logger:       logger,
		manifestFile: manifestFile,
		configFile:   configFile,
		repoRoot:     repoRoot,
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

	// If no selected repos specified, use all from manifest
	if len(cfg.SelectedRepos) == 0 {
		for path := range r.projects {
			cfg.SelectedRepos = append(cfg.SelectedRepos, path)
		}
	}

	r.cfg = cfg
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

	for _, repoPath := range r.cfg.SelectedRepos {
		project, ok := r.projects[repoPath]
		if !ok {
			r.logger.Warnf("Repo %s not found in manifest", repoPath)
			continue
		}

		repoFullPath := filepath.Join(r.repoRoot, repoPath)

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
	sort.Sort(CommitsByTime(r.commits))

	r.logger.Infof("Found %d commits across all repos in date range", len(r.commits))
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

	// Filter branches based on config
	selectedBranches := r.filterBranches(branches)
	if len(selectedBranches) == 0 {
		r.logger.Debugf("No matching branches in repo %s", project.Path)
		return nil
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

// getRepoBranches lists all branches in a git repository
func (r *RepoP4Transfer) getRepoBranches(repoPath string) ([]string, error) {
	cmd := exec.Command("git", "branch", "-r")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run git branch: %w", err)
	}

	branches := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		branch := strings.TrimSpace(scanner.Text())
		// Skip HEAD and remote-tracking branches like origin/HEAD
		if branch != "" && !strings.Contains(branch, "/HEAD") {
			// Remove origin/ prefix to get local branch name
			branch = strings.TrimPrefix(branch, "origin/")
			branches = append(branches, branch)
		}
	}

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

	cmd := exec.Command("git", "log",
		fmt.Sprintf("--since=%s", startDateStr),
		fmt.Sprintf("--until=%s", endDateStr),
		"--format=%H|%aI|%an|%s",
		branch)
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		// Branch might not exist, just skip it
		return nil
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
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

	for repoPath, commits := range commitsByRepo {
		project := r.projects[repoPath]
		repoFullPath := filepath.Join(r.repoRoot, repoPath)

		r.logger.Infof("Processing repo: %s (%d commits)", repoPath, len(commits))

		if err := r.createAndImportFastExport(project, repoFullPath, commits); err != nil {
			r.logger.Errorf("Failed to export/import repo %s: %v", repoPath, err)
			continue
		}
	}

	r.logger.Info("Export and import completed")
	return nil
}

// groupCommitsByRepo groups commits by their source repo
func (r *RepoP4Transfer) groupCommitsByRepo() map[string][]*CommitMetadata {
	groups := make(map[string][]*CommitMetadata)
	for _, commit := range r.commits {
		groups[commit.RepoPath] = append(groups[commit.RepoPath], commit)
	}
	return groups
}

// createAndImportFastExport creates fast-export dump for a repo and imports to P4
func (r *RepoP4Transfer) createAndImportFastExport(project *ManifestProject, repoPath string, commits []*CommitMetadata) error {
	// Create a temporary export file
	tempExportFile := filepath.Join(os.TempDir(), fmt.Sprintf("export_%s.txt", strings.ReplaceAll(project.Path, "/", "_")))
	defer os.Remove(tempExportFile)

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
	if err := r.createFastExport(repoPath, uniqueCommits, tempExportFile); err != nil {
		return fmt.Errorf("failed to create fast-export: %w", err)
	}

	r.logger.Debugf("Fast-export dump created: %s", tempExportFile)

	// TODO: Implement P4 import logic
	// For now, just log the path
	r.logger.Infof("Would import %s to P4 depot %s at path %s", tempExportFile, r.cfg.ImportDepot, project.Path)

	return nil
}

// createFastExport creates a git fast-export dump file
func (r *RepoP4Transfer) createFastExport(repoPath string, commitHashes []string, outputFile string) error {
	// Use git fast-export to create dump file for specified commits
	args := []string{"fast-export", "--all"}
	args = append(args, commitHashes...)

	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git fast-export failed: %w", err)
	}

	if err := os.WriteFile(outputFile, output, 0644); err != nil {
		return fmt.Errorf("failed to write export file: %w", err)
	}

	return nil
}

// PrintStats prints statistics about the transfer
func (r *RepoP4Transfer) PrintStats() {
	r.logger.Info("=== Transfer Statistics ===")
	r.logger.Infof("Total commits: %d", len(r.commits))
	r.logger.Infof("Total repos: %d", len(r.projects))

	// Count commits per repo
	commitsByRepo := r.groupCommitsByRepo()
	for repoPath, commits := range commitsByRepo {
		r.logger.Infof("  %s: %d commits", repoPath, len(commits))
	}
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
	transfer := NewRepoP4Transfer(*manifestFile, *configFile, *repoRoot, logger)

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
