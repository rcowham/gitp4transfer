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
