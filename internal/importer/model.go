// Package importer defines source-neutral data used to create a Perforce import.
package importer

import (
	"io"
	"time"
)

// FileAction describes a change to a source-controlled file.
type FileAction int

const (
	FileModify FileAction = iota
	FileDelete
	FileCopy
	FileRename
)

func (a FileAction) String() string {
	return [...]string{"Modify", "Delete", "Copy", "Rename"}[a]
}

// Identity records a Git author or committer.
type Identity struct {
	Name  string
	Email string
	When  time.Time
}

// BlobContent opens the content identified by a source object ID.
// Callers must close the returned reader.
type BlobContent func() (io.ReadCloser, error)

// FileChange is one file-level change in a commit.
type FileChange struct {
	Action     FileAction
	Path       string
	SourcePath string
	Mode       string
	ObjectID   string
	Content    BlobContent
}

// Commit is the source-neutral representation consumed by the import writer.
type Commit struct {
	ID        string
	Parents   []string
	Ref       string
	Author    Identity
	Committer Identity
	Message   string
	Files     []FileChange
}
