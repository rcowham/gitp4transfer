package importer

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rcowham/gitp4transfer/journal"
)

type commitSliceSource struct {
	commits []*Commit
	index   int
}

func (s *commitSliceSource) Next() (*Commit, error) {
	if s.index >= len(s.commits) {
		return nil, io.EOF
	}
	commit := s.commits[s.index]
	s.index++
	return commit, nil
}

func TestP4WriterWritesArchivesAndDuplicateReferences(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("blob data")), nil }
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	source := &commitSliceSource{commits: []*Commit{
		{ID: "one", Author: Identity{When: when}, Message: "one", Files: []FileChange{{Action: FileModify, Path: "packages/services/Car/a.txt", ObjectID: "blob-id", Content: content}}},
		{ID: "two", Author: Identity{When: when}, Message: "two", Files: []FileChange{{Action: FileModify, Path: "packages/apps/Car/Settings/b.txt", ObjectID: "blob-id", DuplicateOf: "blob-id", Content: content}}},
	}}
	if err := writer.WriteSource(source); err != nil {
		t.Fatal(err)
	}

	archiveFile := filepath.Join(archiveRoot, "import/packages/services/Car/a.txt,d", "1.1.gz")
	archive, err := os.Open(archiveFile)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	reader, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "blob data" {
		t.Errorf("Archive content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "import/packages/apps/Car/Settings/b.txt,d", "1.2.gz")); !os.IsNotExist(err) {
		t.Errorf("Duplicate archive unexpectedly created: %v", err)
	}
	journalOutput := output.String()
	if !strings.Contains(journalOutput, "@//import/packages/services/Car/a.txt@") || !strings.Contains(journalOutput, "@//import/packages/apps/Car/Settings/b.txt@") {
		t.Errorf("Journal missing manifest-prefixed paths:\n%s", journalOutput)
	}
	if !strings.Contains(journalOutput, "@//import/packages/services/Car/a.txt@ @1.1@") {
		t.Errorf("Journal missing duplicate librarian reference:\n%s", journalOutput)
	}
}

func TestP4WriterExportStateResumesAcrossInstances(t *testing.T) {
	archiveRoot := t.TempDir()
	var firstOutput bytes.Buffer
	first, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &firstOutput})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("blob data")), nil }
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	firstCommit := &Commit{ID: "one", Author: Identity{When: when}, Message: "one",
		Files: []FileChange{{Action: FileModify, Path: "a.txt", ObjectID: "blob-id", Content: content}}}
	if err := first.WriteCommit(firstCommit); err != nil {
		t.Fatal(err)
	}
	state := first.ExportState()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A second writer, seeded from the exported state, must not repeat the
	// header and must continue the file's revision history and change count.
	var secondOutput bytes.Buffer
	second, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &secondOutput, InitialState: &state})
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := &Commit{ID: "two", Author: Identity{When: when}, Message: "two",
		Files: []FileChange{{Action: FileModify, Path: "a.txt", ObjectID: "blob-id-2", Content: content}}}
	if err := second.WriteCommit(secondCommit); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(secondOutput.String(), "db.depot") {
		t.Errorf("resumed writer unexpectedly re-wrote the journal header:\n%s", secondOutput.String())
	}
	if second.ChangeCount() != 2 {
		t.Errorf("ChangeCount() = %d, want 2 (continuing from resumed state)", second.ChangeCount())
	}
	if !strings.Contains(secondOutput.String(), "@//import/a.txt@ 2 ") {
		t.Errorf("resumed writer did not continue file revision history:\n%s", secondOutput.String())
	}
}

func TestP4WriterUsesAuthorEmailAsChangeUser(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	commit := &Commit{
		ID:     "author-commit",
		Author: Identity{Email: "sanketa@google.com", When: time.Now()},
		Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "blob", Content: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("content")), nil
		}}},
	}
	if err := writer.WriteSource(&commitSliceSource{commits: []*Commit{commit}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "@git-client@ @sanketa@") {
		t.Fatalf("journal does not contain author user:\n%s", output.String())
	}
}

func TestP4WriterUsesConfiguredClient(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output, Client: "repo-import"})
	if err != nil {
		t.Fatal(err)
	}
	commit := &Commit{
		ID:     "client-commit",
		Author: Identity{Email: "sanketa@google.com", When: time.Now()},
		Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "blob", Content: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("content")), nil
		}}},
	}
	if err := writer.WriteSource(&commitSliceSource{commits: []*Commit{commit}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "git-client") {
		t.Fatalf("journal unexpectedly contains default client:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "@repo-import@ @sanketa@") {
		t.Fatalf("journal does not contain configured client:\n%s", output.String())
	}
}

func TestP4WriterBoundedArchiveWorkers(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output, ParallelWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	content := func(value string) BlobContent {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(value)), nil }
	}
	when := time.Now()
	if err := writer.WriteCommit(&Commit{ID: "one", Author: Identity{When: when}, Files: []FileChange{
		{Action: FileModify, Path: "one.txt", ObjectID: "one", Content: content("one")},
		{Action: FileModify, Path: "two.txt", ObjectID: "two", Content: content("two")},
		{Action: FileModify, Path: "three.txt", ObjectID: "three", Content: content("three")},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if _, err := os.Stat(filepath.Join(archiveRoot, "import", name+",d", "1.1.gz")); err != nil {
			t.Errorf("Archive for %s missing: %v", name, err)
		}
	}
}
func TestP4WriterTypeMapAndCRLFConversion(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		ConvertCRLF: true,
		TypeMaps:    []TypeMap{{Pattern: regexp.MustCompile(`\\.bin$`), FileType: journal.UBinary}},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("one\r\ntwo\r\n")), nil }
	commit := &Commit{ID: "one", Author: Identity{When: time.Now()}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "text", Content: content}}}
	if err := writer.WriteCommit(commit); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(filepath.Join(archiveRoot, "import/file.txt,d", "1.1.gz"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	archive.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("Normalized archive = %q", data)
	}
}

func TestP4WriterCopyAndRename(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("content")), nil }
	when := time.Now()
	source := &commitSliceSource{commits: []*Commit{
		{ID: "one", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "source.txt", ObjectID: "source", Content: content}}},
		{ID: "two", Author: Identity{When: when}, Files: []FileChange{{Action: FileCopy, SourcePath: "source.txt", Path: "copy.txt"}}},
		{ID: "three", Author: Identity{When: when}, Files: []FileChange{{Action: FileRename, SourcePath: "source.txt", Path: "renamed.txt"}}},
	}}
	if err := writer.WriteSource(source); err != nil {
		t.Fatal(err)
	}
	journalOutput := output.String()
	for _, depotFile := range []string{"//import/source.txt", "//import/copy.txt", "//import/renamed.txt"} {
		if !strings.Contains(journalOutput, "@"+depotFile+"@") {
			t.Errorf("Journal missing %s:\n%s", depotFile, journalOutput)
		}
	}
	if strings.Count(journalOutput, "@db.integed@") != 4 {
		t.Errorf("Expected two integration record pairs, got:\n%s", journalOutput)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "import/copy.txt,d", "1.2.gz")); !os.IsNotExist(err) {
		t.Errorf("Copy unexpectedly created an archive: %v", err)
	}
}

func TestP4WriterCopyFindsSnapshotSourceAcrossRefSpellings(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		BranchPath:  func(ref string) string { return ref },
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("content")), nil }
	when := time.Now()
	commits := []*Commit{
		{ID: "snapshot", Ref: "main", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "source.txt", ObjectID: "source", Content: content}}},
		{ID: "copy", Ref: "refs/heads/main", Author: Identity{When: when}, Files: []FileChange{{Action: FileCopy, SourcePath: "source.txt", Path: "copy.txt"}}},
	}
	if err := writer.WriteSource(&commitSliceSource{commits: commits}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "@//import/refs/heads/main/copy.txt@") {
		t.Fatalf("copy revision missing after ref spelling change:\n%s", output.String())
	}
}

func TestP4WriterCopyFallsBackToBlobWhenSourceIsNotImported(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("copied content")), nil }
	commit := &Commit{ID: "copy", Author: Identity{When: time.Now()}, Files: []FileChange{{
		Action: FileCopy, SourcePath: "omitted/source.txt", Path: "copy.txt", ObjectID: "source-blob", Content: content,
	}}}
	if err := writer.WriteCommit(commit); err != nil {
		t.Fatalf("WriteCommit() returned an error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "@//import/copy.txt@") {
		t.Fatalf("fallback copy revision missing:\n%s", output.String())
	}
}

func TestP4WriterCopyUpdatesExistingDestination(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("replacement")), nil }
	commits := []*Commit{
		{ID: "existing", Author: Identity{When: time.Now()}, Files: []FileChange{{Action: FileModify, Path: "copy.txt", ObjectID: "old", Content: content}}},
		{ID: "copy", Author: Identity{When: time.Now()}, Files: []FileChange{{Action: FileCopy, SourcePath: "omitted/source.txt", Path: "copy.txt", ObjectID: "new", Content: content}}},
	}
	if err := writer.WriteSource(&commitSliceSource{commits: commits}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "@db.rev@ @//import/copy.txt@") != 2 {
		t.Fatalf("expected add and edit for existing destination:\n%s", output.String())
	}
}

func TestP4WriterIgnoresDeleteBeforeImportRange(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{ImportDepot: "import", ArchiveRoot: archiveRoot, Journal: &output})
	if err != nil {
		t.Fatal(err)
	}
	commit := &Commit{ID: "delete", Author: Identity{When: time.Now()}, Files: []FileChange{{Action: FileDelete, Path: "OWNERS_networking"}}}
	if err := writer.WriteCommit(commit); err != nil {
		t.Fatalf("WriteCommit() returned an error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "@//import/OWNERS_networking@") {
		t.Errorf("Unexpected delete revision for pre-range path:\n%s", output.String())
	}
}

func TestP4WriterBranchPath(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		BranchPath: func(ref string) string {
			return strings.TrimPrefix(ref, "refs/heads/")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("content")), nil }
	commit := &Commit{ID: "commit-id", Ref: "refs/heads/android17-release", Author: Identity{When: time.Now()}, Files: []FileChange{{Action: FileModify, Path: "packages/services/Car/file.txt", ObjectID: "blob", Content: content}}}
	if err := writer.WriteCommit(commit); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "@//import/android17-release/packages/services/Car/file.txt@") {
		t.Errorf("Journal missing branch-prefixed depot path:\n%s", output.String())
	}
	if writer.commitBranches["commit-id"] != "android17-release" {
		t.Errorf("Commit branch = %q", writer.commitBranches["commit-id"])
	}
}

func TestP4WriterCrossBranchIntegration(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		BranchPath:  func(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") },
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("content")), nil }
	when := time.Now()
	source := &commitSliceSource{commits: []*Commit{
		{ID: "main-1", Ref: "refs/heads/main", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "blob", Content: content}}},
		{ID: "dev-1", Parents: []string{"main-1"}, Ref: "refs/heads/dev", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "blob", Content: content}}},
	}}
	if err := writer.WriteSource(source); err != nil {
		t.Fatal(err)
	}
	journalOutput := output.String()
	if !strings.Contains(journalOutput, "@//import/dev/file.txt@ @//import/main/file.txt@") {
		t.Errorf("Missing cross-branch integration:\n%s", journalOutput)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "import/dev/file.txt,d", "1.2.gz")); !os.IsNotExist(err) {
		t.Errorf("Cross-branch copy unexpectedly created archive: %v", err)
	}
}

func TestP4WriterMergeIntegration(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		BranchPath:  func(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") },
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func(value string) BlobContent {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(value)), nil }
	}
	when := time.Now()
	source := &commitSliceSource{commits: []*Commit{
		{ID: "main-1", Ref: "refs/heads/main", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "main", Content: content("main")}}},
		{ID: "dev-1", Parents: []string{"main-1"}, Ref: "refs/heads/dev", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "dev", Content: content("dev")}}},
		{ID: "main-2", Parents: []string{"main-1", "dev-1"}, Ref: "refs/heads/main", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "file.txt", ObjectID: "dev", DuplicateOf: "dev", Content: content("dev")}}},
	}}
	if err := writer.WriteSource(source); err != nil {
		t.Fatal(err)
	}
	journalOutput := output.String()
	if !strings.Contains(journalOutput, "@//import/main/file.txt@ @//import/dev/file.txt@") {
		t.Errorf("Missing merge integration:\n%s", journalOutput)
	}
	if strings.Count(journalOutput, "@db.integed@") < 2 {
		t.Errorf("Expected merge integration pair:\n%s", journalOutput)
	}
}

func TestP4WriterMaterializesUnchangedBranchFiles(t *testing.T) {
	archiveRoot := t.TempDir()
	var output bytes.Buffer
	writer, err := NewP4Writer(P4WriterOptions{
		ImportDepot: "import",
		ArchiveRoot: archiveRoot,
		Journal:     &output,
		BranchPath:  func(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") },
	})
	if err != nil {
		t.Fatal(err)
	}
	content := func(value string) BlobContent {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(value)), nil }
	}
	when := time.Now()
	source := &commitSliceSource{commits: []*Commit{
		{ID: "main-1", Ref: "refs/heads/main", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "unchanged.txt", ObjectID: "base", Content: content("base")}}},
		{ID: "dev-1", Parents: []string{"main-1"}, Ref: "refs/heads/dev", Author: Identity{When: when}, Files: []FileChange{{Action: FileModify, Path: "dev.txt", ObjectID: "dev", Content: content("dev")}}},
	}}
	if err := writer.WriteSource(source); err != nil {
		t.Fatal(err)
	}
	journalOutput := output.String()
	if !strings.Contains(journalOutput, "@//import/dev/unchanged.txt@ @//import/main/unchanged.txt@") {
		t.Errorf("Missing unchanged branch integration:\n%s", journalOutput)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "import/dev/unchanged.txt,d", "1.2.gz")); !os.IsNotExist(err) {
		t.Errorf("Unchanged branched file unexpectedly created archive: %v", err)
	}
}
