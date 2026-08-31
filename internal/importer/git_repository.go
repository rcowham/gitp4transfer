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
	PathPrefix      string
	Ref             string
	CommitRefs      map[string]string
	Context         context.Context
	CheckpointFile  string
	Dummy           bool
	InitialSnapshot bool
	// ValidateBoundary excludes a commit (and everything reachable from it)
	// from Validate()'s object enumeration. Set it to the last commit already
	// validated for this repository so Validate() only walks objects newly
	// introduced by commitIDs, instead of the entire repository history on
	// every call.
	ValidateBoundary string
}

type gitRepositoryCheckpoint struct {
	CommitID string `json:"commit_id"`
}

// rawCommitMetadata holds one commit's fields parsed from a batched
// `git log --stdin` response, keyed by commit ID in GitRepositorySource.metadataCache.
type rawCommitMetadata struct {
	parents        []string
	authorName     string
	authorEmail    string
	authorTime     time.Time
	committerName  string
	committerEmail string
	committerTime  time.Time
	message        string
}

// GitRepositorySource reads planned commits directly from a Git repository.
// It currently reports add, modify, and delete actions; rename/copy inference
// is intentionally deferred to a later migration step.
type GitRepositorySource struct {
	repository       string
	pathPrefix       string
	ref              string
	commitRefs       map[string]string
	commitIDs        []string
	index            int
	context          context.Context
	checkpoint       string
	validateBoundary string
	lastCommit       string
	dummy            bool
	initialSnapshot  bool
	snapshotPending  bool
	seenBlobs        map[string]struct{}
	metadataCache    map[string]rawCommitMetadata
	catFile          *exec.Cmd
	catInput         io.WriteCloser
	catWriter        *bufio.Writer
	catOutput        *bufio.Reader
	catError         bytes.Buffer
	activeBlob       bool
	diffTree         *exec.Cmd
	diffInput        io.WriteCloser
	diffOutput       *bufio.Reader
	diffError        bytes.Buffer
	diffPending      string
	diffHasPending   bool
	diffWriterDone   chan error
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
		repository:       repository,
		pathPrefix:       strings.Trim(options.PathPrefix, "/"),
		ref:              options.Ref,
		commitRefs:       options.CommitRefs,
		commitIDs:        commitIDs,
		context:          ctx,
		checkpoint:       options.CheckpointFile,
		validateBoundary: options.ValidateBoundary,
		dummy:            options.Dummy,
		initialSnapshot:  options.InitialSnapshot,
		snapshotPending:  options.InitialSnapshot && len(commitIDs) > 0,
		seenBlobs:        make(map[string]struct{}),
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
	if s.snapshotPending {
		snapshotPending := s.snapshotPending
		s.snapshotPending = false
		ref := s.ref
		if commitRef, ok := s.commitRefs[commitID]; ok {
			ref = commitRef
		}
		commit, err := s.readCommitMetadata(commitID, ref)
		if err != nil {
			s.snapshotPending = snapshotPending
			return nil, err
		}
		commit.ID = "snapshot:" + commit.ID
		commit.Parents = nil
		commit.Message = fmt.Sprintf("Starting snapshot at Git commit %s (%s)", commitID, commit.Committer.When.Format(time.RFC3339))
		commit.Files, err = s.readSnapshot(commitID)
		if err != nil {
			s.snapshotPending = snapshotPending
			return nil, err
		}
		return commit, nil
	}
	s.index++
	ref := s.ref
	if commitRef, ok := s.commitRefs[commitID]; ok {
		ref = commitRef
	}
	commit, err := s.readCommit(commitID, ref)
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
	args := []string{"rev-list", "--no-object-names", "--stdin"}
	if !s.dummy {
		args = append(args, "--objects")
	}
	// Commit IDs are passed via stdin, not argv: large batches can otherwise
	// exceed the OS command-line length limit ("argument list too long").
	// ValidateBoundary excludes already-validated ancestry (via argv "--not",
	// which applies uniformly to stdin-supplied revisions) so this walks only
	// the objects newly introduced by commitIDs rather than the whole repo
	// history on every call.
	if s.validateBoundary != "" {
		args = append(args, "--not", s.validateBoundary)
	}
	var stdin strings.Builder
	for _, id := range s.commitIDs {
		stdin.WriteString(id)
		stdin.WriteByte('\n')
	}
	revListCmd := exec.CommandContext(s.context, "git", args...)
	revListCmd.Dir = s.repository
	revListCmd.Stdin = strings.NewReader(stdin.String())
	var revListStderr bytes.Buffer
	revListCmd.Stderr = &revListStderr
	objectOutput, err := revListCmd.Output()
	if err != nil {
		return fmt.Errorf("enumerate repository objects: %w: %s", err, revListStderr.String())
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

func (s *GitRepositorySource) readCommit(commitID, ref string) (*Commit, error) {
	commit, err := s.readCommitMetadata(commitID, ref)
	if err != nil {
		return nil, err
	}
	commit.Files, err = s.readChanges(commitID)
	if err != nil {
		return nil, err
	}
	return commit, nil
}

func (s *GitRepositorySource) readCommitMetadata(commitID, ref string) (*Commit, error) {
	if err := s.loadMetadata(); err != nil {
		return nil, err
	}
	meta, ok := s.metadataCache[commitID]
	if !ok {
		return nil, fmt.Errorf("commit metadata not found for %s", commitID)
	}
	return &Commit{
		ID:        commitID,
		Parents:   meta.parents,
		Ref:       ref,
		Author:    Identity{Name: meta.authorName, Email: meta.authorEmail, When: meta.authorTime},
		Committer: Identity{Name: meta.committerName, Email: meta.committerEmail, When: meta.committerTime},
		Message:   meta.message,
	}, nil
}

// loadMetadata fetches every planned commit's metadata in a single `git log
// --stdin` call instead of spawning `git show` once per commit.
func (s *GitRepositorySource) loadMetadata() error {
	if s.metadataCache != nil {
		return nil
	}
	cache := make(map[string]rawCommitMetadata, len(s.commitIDs))
	if len(s.commitIDs) == 0 {
		s.metadataCache = cache
		return nil
	}
	seen := make(map[string]struct{}, len(s.commitIDs))
	unique := make([]string, 0, len(s.commitIDs))
	for _, id := range s.commitIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	cmd := exec.CommandContext(s.context, "git", "log", "--no-walk", "--stdin",
		"--format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%B%x00")
	cmd.Dir = s.repository
	cmd.Stdin = strings.NewReader(strings.Join(unique, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read commit metadata: %w: %s", err, stderr.String())
	}
	// git inserts a "\n" separator before each record after the first, in
	// addition to the "%x00" terminator we append to every record ourselves.
	tokens := strings.Split(string(output), "\x00")
	for index := 0; index+9 <= len(tokens); index += 9 {
		hash := strings.TrimPrefix(tokens[index], "\n")
		authorTime, err := time.Parse(time.RFC3339, tokens[index+4])
		if err != nil {
			return fmt.Errorf("parse author time for commit %s: %w", hash, err)
		}
		committerTime, err := time.Parse(time.RFC3339, tokens[index+7])
		if err != nil {
			return fmt.Errorf("parse committer time for commit %s: %w", hash, err)
		}
		cache[hash] = rawCommitMetadata{
			parents:        strings.Fields(tokens[index+1]),
			authorName:     tokens[index+2],
			authorEmail:    tokens[index+3],
			authorTime:     authorTime,
			committerName:  tokens[index+5],
			committerEmail: tokens[index+6],
			committerTime:  committerTime,
			message:        tokens[index+8],
		}
	}
	s.metadataCache = cache
	return nil
}

func (s *GitRepositorySource) readSnapshot(commitID string) ([]FileChange, error) {
	output, err := s.gitOutput("ls-tree", "-r", "-z", commitID)
	if err != nil {
		return nil, fmt.Errorf("read tree for commit %s: %w", commitID, err)
	}
	changes := make([]FileChange, 0)
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\t", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("unexpected tree record for commit %s: %q", commitID, record)
		}
		metadata := strings.Fields(fields[0])
		if len(metadata) != 3 || metadata[1] != "blob" {
			continue
		}
		objectID := metadata[2]
		change := FileChange{
			Action:   FileModify,
			Path:     s.prefixedPath(fields[1]),
			Mode:     metadata[0],
			ObjectID: objectID,
		}
		if _, seen := s.seenBlobs[objectID]; seen {
			change.DuplicateOf = objectID
		} else {
			s.seenBlobs[objectID] = struct{}{}
		}
		change.Content = func() (io.ReadCloser, error) { return s.OpenBlob(objectID) }
		changes = append(changes, change)
	}
	return changes, nil
}

func (s *GitRepositorySource) readChanges(commitID string) ([]FileChange, error) {
	if err := s.startDiffTree(); err != nil {
		return nil, err
	}
	echo, err := s.nextDiffToken()
	if err != nil {
		return nil, fmt.Errorf("read changes for commit %s: %w", commitID, err)
	}
	if echo != commitID {
		return nil, fmt.Errorf("diff-tree commit mismatch for %s: got %q", commitID, echo)
	}

	changes := make([]FileChange, 0)
	for {
		token, err := s.nextDiffToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read changes for commit %s: %w", commitID, err)
		}
		if !strings.HasPrefix(token, ":") {
			// Not a status line: it's the next commit's echoed ID, buffered
			// for that commit's own readChanges call.
			s.diffPending = token
			s.diffHasPending = true
			break
		}
		metadata := strings.Fields(token)
		if len(metadata) < 5 {
			return nil, fmt.Errorf("unexpected diff-tree record for commit %s: %q", commitID, token)
		}
		status := metadata[4]
		if len(status) == 0 {
			return nil, fmt.Errorf("missing diff-tree status for commit %s", commitID)
		}
		firstPath, err := s.nextDiffToken()
		if err != nil {
			return nil, fmt.Errorf("read diff-tree path for commit %s: %w", commitID, err)
		}
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
			destPath, err := s.nextDiffToken()
			if err != nil {
				return nil, fmt.Errorf("read diff-tree destination path for commit %s: %w", commitID, err)
			}
			change.SourcePath = s.prefixedPath(firstPath)
			change.Path = s.prefixedPath(destPath)
			if status[0] == 'R' {
				change.Action = FileRename
			} else {
				change.Action = FileCopy
			}
			objectID, err := s.resolveObjectID(commitID + "^:" + firstPath)
			if err == nil {
				change.ObjectID = objectID
				change.Content = func() (io.ReadCloser, error) { return s.OpenBlob(objectID) }
			}
		default:
			return nil, fmt.Errorf("unsupported diff-tree status %q for commit %s", status, commitID)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// nextDiffToken returns the next NUL-delimited token from the persistent
// diff-tree process, or io.EOF once the process has closed its output.
func (s *GitRepositorySource) nextDiffToken() (string, error) {
	if s.diffHasPending {
		s.diffHasPending = false
		token := s.diffPending
		s.diffPending = ""
		return token, nil
	}
	raw, err := s.diffOutput.ReadBytes(0)
	if err != nil {
		if err == io.EOF && len(raw) == 0 {
			return "", io.EOF
		}
		return "", fmt.Errorf("read diff-tree token: %w", err)
	}
	return string(raw[:len(raw)-1]), nil
}

// startDiffTree starts one persistent `git diff-tree --stdin` process for the
// whole source lifetime, replacing a per-commit spawn. A writer goroutine
// feeds every planned commit ID ahead of the reader so that detecting the
// boundary between one commit's records and the next never blocks: the
// stdin/stdout pipes provide the necessary backpressure.
func (s *GitRepositorySource) startDiffTree() error {
	if s.diffTree != nil {
		return nil
	}
	cmd := exec.CommandContext(s.context, "git", "diff-tree", "--stdin", "--root", "-r", "--raw", "-z", "-M", "-C", "--find-copies-harder")
	cmd.Dir = s.repository
	input, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open git diff-tree input: %w", err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git diff-tree output: %w", err)
	}
	cmd.Stderr = &s.diffError
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git diff-tree: %w", err)
	}
	s.diffTree = cmd
	s.diffInput = input
	s.diffOutput = bufio.NewReader(output)
	s.diffWriterDone = make(chan error, 1)
	// startDiffTree is lazily triggered from readChanges, which Next() always
	// calls after advancing s.index past the commit being read (and, for a
	// resumed source, past any earlier commits already skipped) - so
	// s.index-1 is the first commit this process must actually be fed.
	commitIDs := s.commitIDs[s.index-1:]
	go func() {
		var writeErr error
		for _, id := range commitIDs {
			if _, err := fmt.Fprintf(input, "%s\n", id); err != nil {
				writeErr = err
				break
			}
		}
		closeErr := input.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		s.diffWriterDone <- writeErr
	}()
	return nil
}

// resolveObjectID resolves a rev-spec such as "<commit>^:<path>" to a blob ID
// through the persistent cat-file process, avoiding a `git rev-parse` spawn
// per renamed/copied file.
func (s *GitRepositorySource) resolveObjectID(rev string) (string, error) {
	if s.activeBlob {
		return "", fmt.Errorf("previous blob content has not been closed")
	}
	if err := s.startCatFile(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(s.catWriter, "info %s\n", rev); err != nil {
		return "", fmt.Errorf("request object info %s: %w", rev, err)
	}
	if err := s.catWriter.Flush(); err != nil {
		return "", fmt.Errorf("flush object info request %s: %w", rev, err)
	}
	line, err := s.catOutput.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read object info %s: %w", rev, err)
	}
	fields := strings.Fields(line)
	if len(fields) == 2 && fields[1] == "missing" {
		return "", fmt.Errorf("object %s missing", rev)
	}
	if len(fields) != 3 {
		return "", fmt.Errorf("unexpected object info response for %s: %s", rev, strings.TrimSpace(line))
	}
	return fields[0], nil
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

// Close stops the persistent cat-file and diff-tree processes.
func (s *GitRepositorySource) Close() error {
	if s.activeBlob {
		return fmt.Errorf("blob content must be closed before closing source")
	}
	var err error
	if s.catFile != nil {
		if closeErr := s.catInput.Close(); closeErr != nil {
			err = closeErr
		}
		if waitErr := s.catFile.Wait(); waitErr != nil && err == nil {
			err = fmt.Errorf("git cat-file failed: %w: %s", waitErr, s.catError.String())
		}
		s.catFile = nil
	}
	if s.diffTree != nil {
		// Best-effort: the writer may still have unwritten commit IDs if the
		// caller stops early, so kill rather than wait for a natural EOF.
		_ = s.diffTree.Process.Kill()
		<-s.diffWriterDone
		_ = s.diffTree.Wait()
		s.diffTree = nil
	}
	return err
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
