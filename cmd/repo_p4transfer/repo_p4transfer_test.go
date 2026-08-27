package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestManifestLoading(t *testing.T) {
	// Create a temporary manifest file for testing
	manifestContent := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="aosp" fetch=".." />
  <default revision="main" remote="aosp" />
  <project path="build/make" name="platform/build" />
  <project path="platform/art" name="platform/art" />
  <project path="device/amlogic/yukawa" name="device/amlogic/yukawa" />
</manifest>`

	tmpDir := t.TempDir()
	manifestFile := filepath.Join(tmpDir, "manifest.xml")
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Write manifest file
	if err := os.WriteFile(manifestFile, []byte(manifestContent), 0644); err != nil {
		t.Fatalf("Failed to create test manifest: %v", err)
	}

	// Create a minimal config file
	configContent := `date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"
branches:
  - "main"
import_depot: "//depot/test"`

	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	// Create transfer instance and load manifest
	transfer := NewRepoP4Transfer(manifestFile, configFile, tmpDir, "", nil)

	if err := transfer.loadManifest(); err != nil {
		t.Fatalf("Failed to load manifest: %v", err)
	}

	// Verify manifest was parsed
	if len(transfer.projects) != 3 {
		t.Errorf("Expected 3 projects, got %d", len(transfer.projects))
	}

	// Verify specific projects exist
	expectedProjects := []string{"build/make", "platform/art", "device/amlogic/yukawa"}
	for _, path := range expectedProjects {
		if _, ok := transfer.projects[path]; !ok {
			t.Errorf("Expected project %s not found in manifest", path)
		}
	}
}

func TestConfigLoading(t *testing.T) {
	tmpDir := t.TempDir()
	manifestFile := filepath.Join(tmpDir, "manifest.xml")
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create minimal manifest
	manifestContent := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="aosp" fetch=".." />
  <default revision="main" remote="aosp" />
  <project path="build/make" name="platform/build" />
</manifest>`

	if err := os.WriteFile(manifestFile, []byte(manifestContent), 0644); err != nil {
		t.Fatalf("Failed to create test manifest: %v", err)
	}

	// Create config file
	configContent := `date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"
branches:
  - "main"
  - "android*-release"
import_depot: "//depot/android_import"
import_path: "android"
default_branch: "main"`

	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	// Create transfer instance
	transfer := NewRepoP4Transfer(manifestFile, configFile, tmpDir, "", nil)

	// Load both manifest and config
	if err := transfer.loadManifest(); err != nil {
		t.Fatalf("Failed to load manifest: %v", err)
	}

	if err := transfer.loadConfig(); err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify config was loaded
	if transfer.cfg.ImportDepot != "//depot/android_import" {
		t.Errorf("Expected import_depot '//depot/android_import', got '%s'", transfer.cfg.ImportDepot)
	}

	if len(transfer.cfg.Branches) != 2 {
		t.Errorf("Expected 2 branches, got %d", len(transfer.cfg.Branches))
	}

	// Verify date range parsing
	if err := transfer.parseDateRange(); err != nil {
		t.Fatalf("Failed to parse date range: %v", err)
	}

	expectedStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !transfer.startDate.Equal(expectedStart) {
		t.Errorf("Expected start date %v, got %v", expectedStart, transfer.startDate)
	}
}

func TestBranchPatternMatching(t *testing.T) {
	testCases := []struct {
		branch  string
		pattern string
		expect  bool
	}{
		{"main", "main", true},
		{"main", "*", true},
		{"android13-release", "android*-release", true},
		{"android14-release", "android*-release", true},
		{"develop", "android*-release", false},
		{"main", "develop", false},
		{"v1.0", "v*", true},
	}

	for _, tc := range testCases {
		result := matchPattern(tc.branch, tc.pattern)
		if result != tc.expect {
			t.Errorf("matchPattern(%q, %q) = %v, expected %v", tc.branch, tc.pattern, result, tc.expect)
		}
	}
}

func TestCommitSorting(t *testing.T) {
	// Create commits in random order
	commits := []*CommitMetadata{
		{Hash: "c3", Timestamp: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)},
		{Hash: "c1", Timestamp: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Hash: "c2", Timestamp: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}

	// Sort commits by timestamp
	sort.Sort(CommitsByTime(commits))

	// Verify order
	expectedOrder := []string{"c1", "c2", "c3"}
	for i, expected := range expectedOrder {
		if commits[i].Hash != expected {
			t.Errorf("Expected commit %s at position %d, got %s", expected, i, commits[i].Hash)
		}
	}
}

func TestExportDirSetting(t *testing.T) {
	tmpDir := t.TempDir()
	customExportDir := filepath.Join(tmpDir, "custom_exports")

	transfer := NewRepoP4Transfer("manifest.xml", "config.yaml", tmpDir, customExportDir, nil)

	if transfer.exportDir != customExportDir {
		t.Errorf("Expected exportDir %s, got %s", customExportDir, transfer.exportDir)
	}
}

func TestContiguousGrouping(t *testing.T) {
	// Create globally sorted commits from interleaved repos: A, A, B, B, A
	commits := []*CommitMetadata{
		{Hash: "a1", RepoPath: "repoA", Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)},
		{Hash: "a2", RepoPath: "repoA", Timestamp: time.Date(2024, 1, 1, 10, 5, 0, 0, time.UTC)},
		{Hash: "b1", RepoPath: "repoB", Timestamp: time.Date(2024, 1, 1, 10, 10, 0, 0, time.UTC)},
		{Hash: "b2", RepoPath: "repoB", Timestamp: time.Date(2024, 1, 1, 10, 15, 0, 0, time.UTC)},
		{Hash: "a3", RepoPath: "repoA", Timestamp: time.Date(2024, 1, 1, 10, 20, 0, 0, time.UTC)},
	}

	transfer := NewRepoP4Transfer("manifest.xml", "config.yaml", "", "", nil)
	transfer.commits = commits

	batches := transfer.groupContiguousCommits()

	if len(batches) != 3 {
		t.Fatalf("Expected 3 contiguous batches, got %d", len(batches))
	}

	// Batch 1: repoA with 2 commits
	if batches[0].RepoPath != "repoA" || len(batches[0].Commits) != 2 {
		t.Errorf("Batch 1 mismatch: repo=%s, commits=%d", batches[0].RepoPath, len(batches[0].Commits))
	}
	if batches[0].Commits[0].Hash != "a1" || batches[0].Commits[1].Hash != "a2" {
		t.Errorf("Batch 1 commits mismatch")
	}

	// Batch 2: repoB with 2 commits
	if batches[1].RepoPath != "repoB" || len(batches[1].Commits) != 2 {
		t.Errorf("Batch 2 mismatch: repo=%s, commits=%d", batches[1].RepoPath, len(batches[1].Commits))
	}
	if batches[1].Commits[0].Hash != "b1" || batches[1].Commits[1].Hash != "b2" {
		t.Errorf("Batch 2 commits mismatch")
	}

	// Batch 3: repoA with 1 commit
	if batches[2].RepoPath != "repoA" || len(batches[2].Commits) != 1 {
		t.Errorf("Batch 3 mismatch: repo=%s, commits=%d", batches[2].RepoPath, len(batches[2].Commits))
	}
	if batches[2].Commits[0].Hash != "a3" {
		t.Errorf("Batch 3 commit mismatch")
	}
}
