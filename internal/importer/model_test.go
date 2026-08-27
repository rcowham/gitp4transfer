package importer

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	libfastimport "github.com/rcowham/go-libgitfastimport"
)

func TestFileActionString(t *testing.T) {
	tests := []struct {
		action FileAction
		want   string
	}{
		{FileModify, "Modify"},
		{FileDelete, "Delete"},
		{FileCopy, "Copy"},
		{FileRename, "Rename"},
	}

	for _, test := range tests {
		if got := test.action.String(); got != test.want {
			t.Errorf("String() = %q, want %q", got, test.want)
		}
	}
}

func TestBlobContent(t *testing.T) {
	change := FileChange{
		Action:   FileModify,
		Path:     "packages/services/Car/file.txt",
		ObjectID: "abc123",
		Content: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("content")), nil
		},
	}

	content, err := change.Content()
	if err != nil {
		t.Fatalf("Content() returned an error: %v", err)
	}
	defer content.Close()

	data, err := io.ReadAll(content)
	if err != nil {
		t.Fatalf("Failed to read content: %v", err)
	}
	if got := string(data); got != "content" {
		t.Errorf("Content() = %q, want %q", got, "content")
	}
}

func TestCommitMetadata(t *testing.T) {
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	commit := Commit{
		ID:      "commit-id",
		Parents: []string{"parent-id"},
		Ref:     "refs/heads/main",
		Author:  Identity{Name: "Author", Email: "author@example.com", When: when},
		Files:   []FileChange{{Action: FileDelete, Path: "removed.txt"}},
	}

	if commit.ID != "commit-id" || commit.Parents[0] != "parent-id" || !commit.Author.When.Equal(when) {
		t.Errorf("Commit metadata was not retained: %+v", commit)
	}
}

type testCommandSource struct {
	commands []libfastimport.Cmd
	index    int
}

func (s *testCommandSource) Next() (libfastimport.Cmd, error) {
	if s.index >= len(s.commands) {
		return nil, io.EOF
	}
	cmd := s.commands[s.index]
	s.index++
	return cmd, nil
}

func TestProcess(t *testing.T) {
	source := &testCommandSource{commands: []libfastimport.Cmd{
		libfastimport.CmdBlob{Mark: 1},
		libfastimport.CmdBlob{Mark: 2},
	}}
	var marks []int

	err := Process(source, func(cmd libfastimport.Cmd) error {
		marks = append(marks, cmd.(libfastimport.CmdBlob).Mark)
		return nil
	})
	if err != nil {
		t.Fatalf("Process() returned an error: %v", err)
	}
	if len(marks) != 2 || marks[0] != 1 || marks[1] != 2 {
		t.Errorf("Process() handled unexpected marks: %v", marks)
	}
}

func TestProcessPropagatesError(t *testing.T) {
	want := errors.New("handler failure")
	source := &testCommandSource{commands: []libfastimport.Cmd{libfastimport.CmdBlob{Mark: 1}}}

	err := Process(source, func(libfastimport.Cmd) error { return want })
	if !errors.Is(err, want) {
		t.Errorf("Process() error = %v, want %v", err, want)
	}
}
