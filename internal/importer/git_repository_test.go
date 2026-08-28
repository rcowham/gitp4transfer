package importer

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitRepositorySource(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	file := filepath.Join(repoDir, "file.txt")
	if err := os.WriteFile(file, []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file.txt")
	runGit("commit", "-m", "first commit")
	firstIDOutput, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	firstID := string(firstIDOutput[:len(firstIDOutput)-1])

	if err := os.WriteFile(file, []byte("second"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-am", "second commit")
	secondIDOutput, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	secondID := string(secondIDOutput[:len(secondIDOutput)-1])

	source := NewGitRepositorySource(repoDir, []string{firstID, secondID})
	defer source.Close()

	first, err := source.Next()
	if err != nil {
		t.Fatalf("Next() failed: %v", err)
	}
	if first.ID != firstID || len(first.Parents) != 0 || len(first.Files) != 1 || first.Files[0].Action != FileModify {
		t.Errorf("Unexpected first commit: %+v", first)
	}
	content, err := first.Files[0].Content()
	if err != nil {
		t.Fatalf("Open first blob: %v", err)
	}
	data, err := io.ReadAll(content)
	if err != nil {
		t.Fatalf("Read first blob: %v", err)
	}
	content.Close()
	if string(data) != "first" {
		t.Errorf("First blob = %q, want %q", data, "first")
	}

	second, err := source.Next()
	if err != nil {
		t.Fatalf("Next() failed: %v", err)
	}
	if second.ID != secondID || len(second.Parents) != 1 || len(second.Files) != 1 {
		t.Errorf("Unexpected second commit: %+v", second)
	}
	content, err = second.Files[0].Content()
	if err != nil {
		t.Fatalf("Open second blob: %v", err)
	}
	data, err = io.ReadAll(content)
	if err != nil {
		t.Fatalf("Read second blob: %v", err)
	}
	content.Close()
	if string(data) != "second" {
		t.Errorf("Second blob = %q, want %q", data, "second")
	}

	if _, err := source.Next(); err != io.EOF {
		t.Errorf("Next() error = %v, want io.EOF", err)
	}
}

func TestGitRepositorySourcePathPrefix(t *testing.T) {
	createRepository := func(contents string) (string, string) {
		repoDir := t.TempDir()
		runGit := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = repoDir
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, output)
			}
		}
		runGit("init", "-b", "main")
		runGit("config", "user.name", "Test User")
		runGit("config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repoDir, "Android.bp"), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "Android.bp")
		runGit("commit", "-m", "add Android.bp")
		output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return repoDir, string(output[:len(output)-1])
	}

	carRepo, carCommit := createRepository("car")
	settingsRepo, settingsCommit := createRepository("settings")
	carSource := NewGitRepositorySourceWithPrefix(carRepo, []string{carCommit}, "packages/services/Car")
	settingsSource := NewGitRepositorySourceWithPrefix(settingsRepo, []string{settingsCommit}, "packages/apps/Car/Settings")

	car, err := carSource.Next()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := settingsSource.Next()
	if err != nil {
		t.Fatal(err)
	}

	if got := car.Files[0].Path; got != "packages/services/Car/Android.bp" {
		t.Errorf("Car path = %q", got)
	}
	if got := settings.Files[0].Path; got != "packages/apps/Car/Settings/Android.bp" {
		t.Errorf("Settings path = %q", got)
	}
}

func TestGitRepositorySourceRef(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("content"), 0644)
	runGit("add", "file.txt")
	runGit("commit", "-m", "add file")
	output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	source := NewGitRepositorySourceWithOptions(repoDir, []string{string(output[:len(output)-1])}, GitRepositorySourceOptions{Ref: "refs/heads/android17-release"})
	commit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if commit.Ref != "refs/heads/android17-release" {
		t.Errorf("Commit ref = %q", commit.Ref)
	}
}

func TestGitRepositorySourceDuplicateBlob(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte("same content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", "first.txt", "second.txt")
	runGit("commit", "-m", "add duplicate content")
	output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	source := NewGitRepositorySource(repoDir, []string{string(output[:len(output)-1])})
	defer source.Close()
	commit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(commit.Files) != 2 {
		t.Fatalf("Expected 2 changes, got %d", len(commit.Files))
	}
	if commit.Files[0].ObjectID != commit.Files[1].ObjectID {
		t.Fatalf("Expected matching blob IDs, got %q and %q", commit.Files[0].ObjectID, commit.Files[1].ObjectID)
	}
	if commit.Files[0].DuplicateOf != "" {
		t.Errorf("First use unexpectedly marked duplicate: %q", commit.Files[0].DuplicateOf)
	}
	if commit.Files[1].DuplicateOf != commit.Files[0].ObjectID {
		t.Errorf("DuplicateOf = %q, want %q", commit.Files[1].DuplicateOf, commit.Files[0].ObjectID)
	}
}

func TestGitRepositorySourceRenameAndCopy(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	commitID := func() string {
		output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(output[:len(output)-1])
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "source.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("commit", "-m", "add source")

	runGit("mv", "source.txt", "renamed.txt")
	runGit("commit", "-m", "rename source")
	renameID := commitID()

	if err := os.WriteFile(filepath.Join(repoDir, "copy.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "copy.txt")
	runGit("commit", "-m", "copy renamed")
	copyID := commitID()

	source := NewGitRepositorySource(repoDir, []string{renameID, copyID})
	renameCommit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(renameCommit.Files) != 1 || renameCommit.Files[0].Action != FileRename ||
		renameCommit.Files[0].SourcePath != "source.txt" || renameCommit.Files[0].Path != "renamed.txt" {
		t.Errorf("Unexpected rename changes: %+v", renameCommit.Files)
	}

	copyCommit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(copyCommit.Files) != 1 || copyCommit.Files[0].Action != FileCopy ||
		copyCommit.Files[0].SourcePath != "renamed.txt" || copyCommit.Files[0].Path != "copy.txt" {
		t.Errorf("Unexpected copy changes: %+v", copyCommit.Files)
	}
}

func TestGitRepositorySourceCheckpointResumeAndValidation(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	commitID := func() string {
		output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(output[:len(output)-1])
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(repoDir, "first.txt"), []byte("first"), 0644)
	runGit("add", "first.txt")
	runGit("commit", "-m", "first")
	firstID := commitID()
	os.WriteFile(filepath.Join(repoDir, "second.txt"), []byte("second"), 0644)
	runGit("add", "second.txt")
	runGit("commit", "-m", "second")
	secondID := commitID()

	checkpoint := filepath.Join(t.TempDir(), "checkpoint.json")
	source := NewGitRepositorySourceWithOptions(repoDir, []string{firstID, secondID}, GitRepositorySourceOptions{CheckpointFile: checkpoint})
	if err := source.Validate(); err != nil {
		t.Fatalf("Validate() failed: %v", err)
	}
	commit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Checkpoint(commit.ID); err != nil {
		t.Fatalf("Checkpoint() failed: %v", err)
	}

	resumed := NewGitRepositorySourceWithOptions(repoDir, []string{firstID, secondID}, GitRepositorySourceOptions{CheckpointFile: checkpoint})
	if err := resumed.Resume(); err != nil {
		t.Fatalf("Resume() failed: %v", err)
	}
	commit, err = resumed.Next()
	if err != nil {
		t.Fatal(err)
	}
	if commit.ID != secondID {
		t.Errorf("Resumed commit = %s, want %s", commit.ID, secondID)
	}
}

func TestGitRepositorySourceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := NewGitRepositorySourceWithOptions(t.TempDir(), []string{"commit-id"}, GitRepositorySourceOptions{Context: ctx})
	if _, err := source.Next(); err != context.Canceled {
		t.Errorf("Next() error = %v, want context.Canceled", err)
	}
}

func TestGitRepositorySourceDummyBlobContent(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("real blob content"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file.txt")
	runGit("commit", "-m", "add file")
	output, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	source := NewGitRepositorySourceWithOptions(repoDir, []string{string(output[:len(output)-1])}, GitRepositorySourceOptions{Dummy: true})
	if err := source.Validate(); err != nil {
		t.Fatalf("Validate() in dummy mode failed: %v", err)
	}
	commit, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	content, err := commit.Files[0].Content()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(content)
	if err != nil {
		t.Fatal(err)
	}
	content.Close()

	want := commit.Files[0].ObjectID + "\n"
	if string(data) != want {
		t.Errorf("Dummy content = %q, want %q", data, want)
	}
	if source.catFile != nil {
		t.Error("Dummy mode unexpectedly started git cat-file")
	}
}
