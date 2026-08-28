package importer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	archiveFile := filepath.Join(archiveRoot, "import/packages/services/Car/a.txt,d", "1.1")
	data, err := os.ReadFile(archiveFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "blob data" {
		t.Errorf("Archive content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "import/packages/apps/Car/Settings/b.txt,d", "1.2")); !os.IsNotExist(err) {
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
