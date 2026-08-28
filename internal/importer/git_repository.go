package importer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// GitRepositorySourceOptions configures native Git source lifecycle behavior.
type GitRepositorySourceOptions struct {
	PathPrefix     string
	Ref            string
	Context        context.Context
	CheckpointFile string
	Dummy          bool
}

type gitRepositoryCheckpoint struct {
	CommitID string `json:"commit_id"`
}

// GitRepositorySource reads planned commits directly from a Git repository.
// It currently reports add, modify, and delete actions; rename/copy inference
// is intentionally deferred to a later migration step.
type GitRepositorySource struct {
	repository string
	pathPrefix string
	ref        string
	commitIDs  []string
	index      int
	context    context.Context
	checkpoint string
	lastCommit string
	dummy      bool
	seenBlobs  map[string]struct{}
	catFile    *exec.Cmd
	catInput   io.WriteCloser
	catWriter  *bufio.Writer
	catOutput  *bufio.Reader
	catError   bytes.Buffer
	activeBlob bool
}

// NewGitRepositorySource creates a source for commits in the caller-provided order.
func NewGitRepositorySource(repository string, commitIDs []string) *GitRepositorySource {
	return NewGitRepositorySourceWithOptions(repository, commitIDs, GitRepositorySourceOptions{})
}

// NewGitRepositorySourceWithPrefix creates a source that prefixes every
// emitted file path with a manifest project path.
func NewGitRepositorySourceWithPrefix(repository string, commitIDs []string, pathPrefix string) *GitRepositorySource {
	return NewGitRepositorySourceWithOptions(repository, commitIDs, GitRepositorySourceOptions{PathPrefix: pathPrefix})
}

// NewGitRepositorySourceWithOptions creates a source with optional cancellation
// and checkpoint support.
func NewGitRepositorySourceWithOptions(repository string, commitIDs []string, options GitRepositorySourceOptions) *GitRepositorySource {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return &GitRepositorySource{
		repository: repository,
		pathPrefix: strings.Trim(options.PathPrefix, "/"),
		ref:        options.Ref,
		commitIDs:  commitIDs,
		context:    ctx,
		checkpoint: options.CheckpointFile,
		dummy:      options.Dummy,
		seenBlobs:  make(map[string]struct{}),
	}
}

// Next returns the next planned commit, or io.EOF after the final commit.
func (s *GitRepositorySource) Next() (*Commit, error) {
	if err := s.context.Err(); err != nil {
		return nil, err
	}
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
	s.lastCommit = commit.ID
	return commit, nil
}

// Validate checks that planned commits are available locally. In normal mode it
// also checks all reachable objects. Dummy mode intentionally skips blob
// validation because it never reads blob content.
func (s *GitRepositorySource) Validate() error {
	if err := s.context.Err(); err != nil {
		return err
	}
	if len(s.commitIDs) == 0 {
		return nil
	}
	args := []string{"rev-list", "--no-object-names"}
	if !s.dummy {
		args = append(args, "--objects")
	}
	args = append(args, s.commitIDs...)
	objectOutput, err := s.gitOutput(args...)
	if err != nil {
		return fmt.Errorf("enumerate repository objects: %w", err)
	}
	cmd := exec.CommandContext(s.context, "git", "cat-file", "--batch-check")
	cmd.Dir = s.repository
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	cmd.Stdin = bytes.NewReader(objectOutput)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate repository objects: %w: %s", err, output)
	}
	var missing []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasSuffix(line, " missing") {
			missing = append(missing, strings.TrimSuffix(line, " missing"))
			if len(missing) == 10 {
				break
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("repository has missing objects (first %d): %s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// Checkpoint records the last successfully imported source commit. Call this
// only after its journal and archive work has completed.
func (s *GitRepositorySource) Checkpoint(commitID string) error {
	if s.checkpoint == "" {
		return nil
	}
	if commitID == "" || commitID != s.lastCommit {
		return fmt.Errorf("checkpoint commit %q is not the last emitted commit", commitID)
	}
	data, err := json.Marshal(gitRepositoryCheckpoint{CommitID: commitID})
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	tempFile := s.checkpoint + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tempFile, s.checkpoint); err != nil {
		return fmt.Errorf("replace checkpoint: %w", err)
	}
	return nil
}

// Resume advances the source past the commit stored in its checkpoint file.
func (s *GitRepositorySource) Resume() error {
	if s.checkpoint == "" {
		return nil
	}
	data, err := os.ReadFile(s.checkpoint)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read checkpoint: %w", err)
	}
	var checkpoint gitRepositoryCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return fmt.Errorf("parse checkpoint: %w", err)
	}
	for index, commitID := range s.commitIDs {
		if commitID == checkpoint.CommitID {
			s.index = index + 1
			return nil
		}
	}
	return fmt.Errorf("checkpoint commit %s is not in source plan", checkpoint.CommitID)
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
		Ref:       s.ref,
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
	output, err := s.gitOutput("diff-tree", "--root", "--no-commit-id", "-r", "--raw", "-z", "-M", "-C", "--find-copies-harder", commitID)
	if err != nil {
		return nil, fmt.Errorf("read changes for commit %s: %w", commitID, err)
	}

	parts := strings.Split(string(output), "\x00")
	changes := make([]FileChange, 0)
	for index := 0; index < len(parts)-1; {
		metadata := strings.Fields(parts[index])
		if len(metadata) < 5 {
			return nil, fmt.Errorf("unexpected diff-tree record for commit %s: %q", commitID, parts[index])
		}
		status := metadata[4]
		if len(status) == 0 {
			return nil, fmt.Errorf("missing diff-tree status for commit %s", commitID)
		}
		index++
		if index >= len(parts) || parts[index] == "" {
			return nil, fmt.Errorf("missing diff-tree path for commit %s", commitID)
		}
		firstPath := parts[index]
		index++
		change := FileChange{Path: s.prefixedPath(firstPath), Mode: metadata[1]}
		switch status[0] {
		case 'A', 'M', 'T':
			change.Action = FileModify
			change.ObjectID = metadata[3]
			if _, seen := s.seenBlobs[change.ObjectID]; seen {
				change.DuplicateOf = change.ObjectID
			} else {
				s.seenBlobs[change.ObjectID] = struct{}{}
			}
			objectID := change.ObjectID
			change.Content = func() (io.ReadCloser, error) { return s.OpenBlob(objectID) }
		case 'D':
			change.Action = FileDelete
		case 'R', 'C':
			if index >= len(parts) || parts[index] == "" {
				return nil, fmt.Errorf("missing destination path for diff-tree status %q in commit %s", status, commitID)
			}
			change.SourcePath = s.prefixedPath(firstPath)
			change.Path = s.prefixedPath(parts[index])
			index++
			if status[0] == 'R' {
				change.Action = FileRename
			} else {
				change.Action = FileCopy
			}
		default:
			return nil, fmt.Errorf("unsupported diff-tree status %q for commit %s", status, commitID)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (s *GitRepositorySource) prefixedPath(filePath string) string {
	if s.pathPrefix == "" {
		return filePath
	}
	return path.Join(s.pathPrefix, filePath)
}

// OpenBlob opens a blob through one persistent git cat-file process. The reader
// must be closed before another blob or commit is requested from this source.
func (s *GitRepositorySource) OpenBlob(objectID string) (io.ReadCloser, error) {
	if s.activeBlob {
		return nil, fmt.Errorf("previous blob content has not been closed")
	}
	if s.dummy {
		return io.NopCloser(strings.NewReader(objectID + "\n")), nil
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
	cmd := exec.CommandContext(s.context, "git", "cat-file", "--batch-command")
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
	cmd := exec.CommandContext(s.context, "git", args...)
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
