package importer

import (
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
