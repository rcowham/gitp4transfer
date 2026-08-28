package importer

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GitRepositorySource reads planned commits directly from a Git repository.
// It currently reports add, modify, and delete actions; rename/copy inference
// is intentionally deferred to a later migration step.
type GitRepositorySource struct {
	repository string
	commitIDs  []string
	index      int
	catFile    *exec.Cmd
	catInput   io.WriteCloser
	catWriter  *bufio.Writer
	catOutput  *bufio.Reader
	catError   bytes.Buffer
	activeBlob bool
}

// NewGitRepositorySource creates a source for commits in the caller-provided order.
func NewGitRepositorySource(repository string, commitIDs []string) *GitRepositorySource {
	return &GitRepositorySource{repository: repository, commitIDs: commitIDs}
}

// Next returns the next planned commit, or io.EOF after the final commit.
func (s *GitRepositorySource) Next() (*Commit, error) {
	if s.activeBlob {
		return nil, fmt.Errorf("blob content must be closed before reading the next commit")
	}
	if s.index >= len(s.commitIDs) {
		return nil, io.EOF
	}

	commitID := s.commitIDs[s.index]
	s.index++
	commit, err := s.readCommit(commitID)
	if err != nil {
		return nil, err
	}
	return commit, nil
}

func (s *GitRepositorySource) readCommit(commitID string) (*Commit, error) {
	output, err := s.gitOutput("show", "-s", "--format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%B", commitID)
	if err != nil {
		return nil, fmt.Errorf("read commit %s: %w", commitID, err)
	}
	fields := strings.SplitN(string(output), "\x00", 9)
	if len(fields) != 9 {
		return nil, fmt.Errorf("unexpected metadata format for commit %s", commitID)
	}
	authorTime, err := time.Parse(time.RFC3339, fields[4])
	if err != nil {
		return nil, fmt.Errorf("parse author time for commit %s: %w", commitID, err)
	}
	committerTime, err := time.Parse(time.RFC3339, fields[7])
	if err != nil {
		return nil, fmt.Errorf("parse committer time for commit %s: %w", commitID, err)
	}

	commit := &Commit{
		ID:        fields[0],
		Parents:   strings.Fields(fields[1]),
		Author:    Identity{Name: fields[2], Email: fields[3], When: authorTime},
		Committer: Identity{Name: fields[5], Email: fields[6], When: committerTime},
		Message:   fields[8],
	}
	commit.Files, err = s.readChanges(commitID)
	if err != nil {
		return nil, err
	}
	return commit, nil
}

func (s *GitRepositorySource) readChanges(commitID string) ([]FileChange, error) {
	output, err := s.gitOutput("diff-tree", "--root", "--no-commit-id", "-r", "--raw", "-z", commitID)
	if err != nil {
		return nil, fmt.Errorf("read changes for commit %s: %w", commitID, err)
	}

	parts := strings.Split(string(output), "\x00")
	changes := make([]FileChange, 0)
	for index := 0; index+1 < len(parts); index += 2 {
		metadata := strings.Fields(parts[index])
		if len(metadata) < 5 {
			return nil, fmt.Errorf("unexpected diff-tree record for commit %s: %q", commitID, parts[index])
		}
		status := metadata[4]
		if len(status) == 0 {
			return nil, fmt.Errorf("missing diff-tree status for commit %s", commitID)
		}
		change := FileChange{Path: parts[index+1], Mode: metadata[1]}
		switch status[0] {
		case 'A', 'M', 'T':
			change.Action = FileModify
			change.ObjectID = metadata[3]
			objectID := change.ObjectID
			change.Content = func() (io.ReadCloser, error) { return s.OpenBlob(objectID) }
		case 'D':
			change.Action = FileDelete
		default:
			return nil, fmt.Errorf("unsupported diff-tree status %q for commit %s", status, commitID)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// OpenBlob opens a blob through one persistent git cat-file process. The reader
// must be closed before another blob or commit is requested from this source.
func (s *GitRepositorySource) OpenBlob(objectID string) (io.ReadCloser, error) {
	if s.activeBlob {
		return nil, fmt.Errorf("previous blob content has not been closed")
	}
	if err := s.startCatFile(); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(s.catWriter, "contents %s\n", objectID); err != nil {
		return nil, fmt.Errorf("request blob %s: %w", objectID, err)
	}
	if err := s.catWriter.Flush(); err != nil {
		return nil, fmt.Errorf("flush blob request %s: %w", objectID, err)
	}

	header, err := s.catOutput.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read blob header %s: %w", objectID, err)
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, fmt.Errorf("blob %s unavailable: %s", objectID, strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse blob size %s: %w", objectID, err)
	}

	s.activeBlob = true
	return &catFileBlobReader{source: s, reader: io.LimitReader(s.catOutput, size)}, nil
}

func (s *GitRepositorySource) startCatFile() error {
	if s.catFile != nil {
		return nil
	}
	cmd := exec.Command("git", "cat-file", "--batch-command")
	cmd.Dir = s.repository
	input, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open git cat-file input: %w", err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git cat-file output: %w", err)
	}
	cmd.Stderr = &s.catError
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git cat-file: %w", err)
	}
	s.catFile = cmd
	s.catInput = input
	s.catWriter = bufio.NewWriter(input)
	s.catOutput = bufio.NewReader(output)
	return nil
}

// Close stops the persistent cat-file process.
func (s *GitRepositorySource) Close() error {
	if s.catFile == nil {
		return nil
	}
	if s.activeBlob {
		return fmt.Errorf("blob content must be closed before closing source")
	}
	if err := s.catInput.Close(); err != nil {
		return err
	}
	err := s.catFile.Wait()
	if err != nil {
		return fmt.Errorf("git cat-file failed: %w: %s", err, s.catError.String())
	}
	s.catFile = nil
	return nil
}

func (s *GitRepositorySource) gitOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = s.repository
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

type catFileBlobReader struct {
	source *GitRepositorySource
	reader io.Reader
	closed bool
}

func (r *catFileBlobReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *catFileBlobReader) Close() error {
	if r.closed {
		return nil
	}
	_, _ = io.Copy(io.Discard, r.reader)
	if _, err := r.source.catOutput.ReadByte(); err != nil {
		return fmt.Errorf("read blob terminator: %w", err)
	}
	r.source.activeBlob = false
	r.closed = true
	return nil
}
