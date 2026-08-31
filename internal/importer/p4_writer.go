package importer

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/rcowham/gitp4transfer/journal"
)

// CommitSource produces source-neutral commits in import order.
type CommitSource interface {
	Next() (*Commit, error)
}

// P4WriterOptions configures a linear Perforce journal and archive import.
type P4WriterOptions struct {
	ImportDepot     string
	ArchiveRoot     string
	Journal         io.Writer
	User            string
	Client          string
	TypeMaps        []TypeMap
	ConvertCRLF     bool
	ParallelWorkers int
	DryRun          bool
	CaseInsensitive bool
	BranchPath      func(ref string) string
	// InitialState resumes a previously interrupted import: it seeds revision
	// and dedup tracking from a prior ExportState() snapshot, and skips writing
	// the journal header (the caller is expected to open Journal in append mode).
	InitialState *WriterState
}

// FileRevisionState is the exported, serializable form of a tracked depot file revision.
type FileRevisionState struct {
	Revision int    `json:"revision"`
	LbrFile  string `json:"lbr_file"`
	LbrRev   int    `json:"lbr_rev"`
	ObjectID string `json:"object_id"`
	Active   bool   `json:"active"`
}

// BlobArchiveState is the exported, serializable form of a deduplicated blob archive reference.
type BlobArchiveState struct {
	LbrFile string `json:"lbr_file"`
	LbrRev  int    `json:"lbr_rev"`
}

// WriterState is a serializable snapshot of P4Writer's incremental import
// state, used to resume a native Git import after a crash without re-deriving
// revision history from scratch. See P4Writer.ExportState and P4WriterOptions.InitialState.
type WriterState struct {
	ChangeNumber   int                          `json:"change_number"`
	FileRevisions  map[string]FileRevisionState `json:"file_revisions"`
	BlobArchives   map[string]BlobArchiveState  `json:"blob_archives"`
	CommitBranches map[string]string            `json:"commit_branches"`
	BranchReady    map[string]bool              `json:"branch_ready"`
}

// TypeMap selects a Perforce file type for paths matching Pattern.
type TypeMap struct {
	Pattern  *regexp.Regexp
	FileType journal.FileType
}

type fileRevision struct {
	revision int
	lbrFile  string
	lbrRev   int
	objectID string
	active   bool
}

type blobArchive struct {
	lbrFile string
	lbrRev  int
}

type archiveJob struct {
	stagedFile  string
	archiveFile string
	fileType    journal.FileType
}

// P4Writer writes source-neutral commits to Perforce journal and archive paths.
// It handles linear add, edit, and delete history plus duplicate blob librarian
// references. Branch and merge integration records remain a later migration step.
type P4Writer struct {
	journal         journal.Journal
	importDepot     string
	archiveRoot     string
	user            string
	typeMaps        []TypeMap
	convertCRLF     bool
	dryRun          bool
	caseInsensitive bool
	branchPath      func(ref string) string
	stagingDir      string
	changeNumber    int
	fileRevisions   map[string]fileRevision
	commitBranches  map[string]string
	branchReady     map[string]bool
	blobArchives    map[string]blobArchive
	archiveJobs     chan archiveJob
	archiveWG       sync.WaitGroup
	archiveMu       sync.Mutex
	archiveErr      error
	closed          bool
}

// ChangeCount returns the number of changelists written so far.
func (w *P4Writer) ChangeCount() int {
	return w.changeNumber
}

// ExportState snapshots the writer's revision and dedup tracking so it can be
// persisted and later restored via P4WriterOptions.InitialState.
func (w *P4Writer) ExportState() WriterState {
	state := WriterState{
		ChangeNumber:   w.changeNumber,
		FileRevisions:  make(map[string]FileRevisionState, len(w.fileRevisions)),
		BlobArchives:   make(map[string]BlobArchiveState, len(w.blobArchives)),
		CommitBranches: make(map[string]string, len(w.commitBranches)),
		BranchReady:    make(map[string]bool, len(w.branchReady)),
	}
	for depotFile, revision := range w.fileRevisions {
		state.FileRevisions[depotFile] = FileRevisionState{
			Revision: revision.revision, LbrFile: revision.lbrFile, LbrRev: revision.lbrRev,
			ObjectID: revision.objectID, Active: revision.active,
		}
	}
	for objectID, archive := range w.blobArchives {
		state.BlobArchives[objectID] = BlobArchiveState{LbrFile: archive.lbrFile, LbrRev: archive.lbrRev}
	}
	for commitID, branch := range w.commitBranches {
		state.CommitBranches[commitID] = branch
	}
	for branch, ready := range w.branchReady {
		state.BranchReady[branch] = ready
	}
	return state
}

// NewP4Writer creates a source-neutral Perforce writer.
func NewP4Writer(options P4WriterOptions) (*P4Writer, error) {
	if options.ImportDepot == "" {
		return nil, fmt.Errorf("import depot is required")
	}
	if options.ArchiveRoot == "" {
		return nil, fmt.Errorf("archive root is required")
	}
	if options.Journal == nil {
		return nil, fmt.Errorf("journal writer is required")
	}
	if options.User == "" {
		options.User = "git-user"
	}
	if options.ParallelWorkers < 1 {
		options.ParallelWorkers = 1
	}
	writer := &P4Writer{
		importDepot:     strings.Trim(options.ImportDepot, "/"),
		archiveRoot:     options.ArchiveRoot,
		user:            options.User,
		typeMaps:        options.TypeMaps,
		convertCRLF:     options.ConvertCRLF,
		dryRun:          options.DryRun,
		caseInsensitive: options.CaseInsensitive,
		branchPath:      options.BranchPath,
		stagingDir:      path.Join(options.ArchiveRoot, ".gitp4transfer-staging"),
		fileRevisions:   make(map[string]fileRevision),
		commitBranches:  make(map[string]string),
		branchReady:     make(map[string]bool),
		blobArchives:    make(map[string]blobArchive),
		archiveJobs:     make(chan archiveJob, options.ParallelWorkers),
	}
	writer.journal.SetWriter(options.Journal)
	writer.journal.SetClient(options.Client)
	if options.InitialState != nil {
		state := options.InitialState
		writer.changeNumber = state.ChangeNumber
		for depotFile, revision := range state.FileRevisions {
			writer.fileRevisions[depotFile] = fileRevision{
				revision: revision.Revision, lbrFile: revision.LbrFile, lbrRev: revision.LbrRev,
				objectID: revision.ObjectID, active: revision.Active,
			}
		}
		for objectID, archive := range state.BlobArchives {
			writer.blobArchives[objectID] = blobArchive{lbrFile: archive.LbrFile, lbrRev: archive.LbrRev}
		}
		for commitID, branch := range state.CommitBranches {
			writer.commitBranches[commitID] = branch
		}
		for branch, ready := range state.BranchReady {
			writer.branchReady[branch] = ready
		}
	} else {
		writer.journal.WriteHeader(strings.TrimPrefix(options.ImportDepot, "//"), options.CaseInsensitive)
	}
	for range options.ParallelWorkers {
		writer.archiveWG.Add(1)
		go writer.runArchiveWorker()
	}
	return writer, nil
}

// WriteSource consumes commits until io.EOF.
func (w *P4Writer) WriteSource(source CommitSource) error {
	for {
		commit, err := source.Next()
		if err == io.EOF {
			return w.Close()
		}
		if err != nil {
			_ = w.Close()
			return err
		}
		if err := w.WriteCommit(commit); err != nil {
			_ = w.Close()
			return err
		}
	}
}

// WriteCommit writes one source commit as a Perforce changelist.
func (w *P4Writer) WriteCommit(commit *Commit) error {
	if err := w.archiveError(); err != nil {
		return err
	}
	if w.closed {
		return fmt.Errorf("writer is closed")
	}
	if commit == nil {
		return fmt.Errorf("commit is required")
	}
	w.changeNumber++
	changeNumber := w.changeNumber
	changeTime := int(commit.Author.When.Unix())
	branch := ""
	if w.branchPath != nil {
		branch = strings.Trim(w.branchPath(commit.Ref), "/")
	}
	w.commitBranches[commit.ID] = branch
	parentBranch := ""
	if len(commit.Parents) > 0 {
		parentBranch = w.commitBranches[commit.Parents[0]]
	}
	mergeBranches := make([]string, 0)
	if len(commit.Parents) > 1 {
		mergeBranches = make([]string, 0, len(commit.Parents)-1)
		for _, parentID := range commit.Parents[1:] {
			if mergeBranch := w.commitBranches[parentID]; mergeBranch != "" && mergeBranch != branch {
				mergeBranches = append(mergeBranches, mergeBranch)
			}
		}
	}
	w.journal.WriteChange(changeNumber, userForCommit(commit, w.user), commit.Message, changeTime)
	if err := w.materializeBranch(branch, parentBranch, commit.Files, changeNumber, changeTime); err != nil {
		return fmt.Errorf("commit %s: %w", commit.ID, err)
	}

	for _, change := range commit.Files {
		if err := w.writeChange(change, branch, parentBranch, mergeBranches, changeNumber, changeTime); err != nil {
			return fmt.Errorf("commit %s, path %s: %w", commit.ID, change.Path, err)
		}
	}
	return nil
}

func userForCommit(commit *Commit, fallback string) string {
	if commit == nil || commit.Author.Email == "" {
		return fallback
	}
	parts := strings.SplitN(commit.Author.Email, "@", 2)
	if parts[0] == "" {
		return fallback
	}
	return parts[0]
}

func (w *P4Writer) writeChange(change FileChange, branch, parentBranch string, mergeBranches []string, changeNumber, changeTime int) error {
	depotFile := w.depotPath(branch, change.Path)
	if !strings.HasPrefix(depotFile, "//") {
		return fmt.Errorf("invalid depot path %q", change.Path)
	}
	previous, exists := w.fileRevisions[depotFile]
	revision := previous.revision + 1

	if change.Action == FileDelete {
		if !exists {
			// Date-filtered imports may begin after the file was added. Deleting
			// an unimported path leaves the selected-range Perforce state unchanged.
			return nil
		}
		w.journal.WriteRev(depotFile, revision, journal.Delete, w.fileTypeFor(depotFile), changeNumber, previous.lbrFile, previous.lbrRev, changeTime)
		w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: previous.lbrFile, lbrRev: previous.lbrRev, objectID: previous.objectID}
		return nil
	}
	if change.Action == FileCopy || change.Action == FileRename {
		return w.writeCopyOrRename(change, branch, parentBranch, mergeBranches, depotFile, exists, revision, changeNumber, changeTime)
	}
	if change.Action != FileModify {
		return fmt.Errorf("unsupported source action %s", change.Action)
	}
	if change.Content == nil {
		return fmt.Errorf("missing blob content")
	}
	if !exists && branch != "" && parentBranch != "" && branch != parentBranch {
		parentDepotFile := w.depotPath(parentBranch, change.Path)
		if parent, ok := w.fileRevisions[parentDepotFile]; ok && parent.objectID == change.ObjectID {
			fileType := w.fileTypeFor(parentDepotFile)
			w.journal.WriteRev(depotFile, revision, journal.Add, fileType, changeNumber, parent.lbrFile, parent.lbrRev, changeTime)
			w.journal.WriteInteg(depotFile, parentDepotFile, 0, parent.revision, 0, revision, journal.BranchFrom, journal.BranchInto, changeNumber)
			w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: parent.lbrFile, lbrRev: parent.lbrRev, objectID: change.ObjectID, active: true}
			return nil
		}
	}

	lbr := blobArchive{}
	if known, ok := w.blobArchives[change.ObjectID]; ok {
		lbr = known
	} else {
		content, err := change.Content()
		if err != nil {
			return err
		}
		archiveDepotFile := journal.ReplaceWildcards(depotFile)
		depotRoot := path.Join(w.archiveRoot, strings.TrimPrefix(archiveDepotFile, "//")+",d")
		if err := os.MkdirAll(depotRoot, 0755); err != nil {
			content.Close()
			return err
		}
		fileType := w.fileTypeFor(depotFile)
		archiveFile := path.Join(depotRoot, fmt.Sprintf("1.%d", changeNumber))
		if fileType == journal.CText || fileType == journal.Binary {
			archiveFile += ".gz"
		}
		if w.dryRun {
			if err := content.Close(); err != nil {
				return err
			}
		} else {
			if err := w.stageArchive(archiveFile, content, fileType); err != nil {
				return err
			}
		}
		lbr = blobArchive{lbrFile: depotFile, lbrRev: changeNumber}
		w.blobArchives[change.ObjectID] = lbr
	}

	action := journal.Add
	if exists {
		action = journal.Edit
	}
	w.journal.WriteRev(depotFile, revision, action, w.fileTypeFor(depotFile), changeNumber, lbr.lbrFile, lbr.lbrRev, changeTime)
	w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: lbr.lbrFile, lbrRev: lbr.lbrRev, objectID: change.ObjectID, active: true}
	w.writeMergeIntegrations(depotFile, change.Path, change.ObjectID, branch, mergeBranches, revision, changeNumber)
	return nil
}

func (w *P4Writer) writeMergeIntegrations(depotFile, sourcePath, objectID, branch string, mergeBranches []string, revision, changeNumber int) {
	for _, mergeBranch := range mergeBranches {
		mergeDepotFile := w.depotPath(mergeBranch, sourcePath)
		mergeRevision, ok := w.fileRevisions[mergeDepotFile]
		if !ok || mergeRevision.objectID != objectID {
			continue
		}
		w.journal.WriteInteg(depotFile, mergeDepotFile, 0, mergeRevision.revision, revision-1, revision, journal.MergeFrom, journal.MergeInto, changeNumber)
	}
}

func (w *P4Writer) writeCopyOrRename(change FileChange, branch, parentBranch string, mergeBranches []string, depotFile string, destinationExists bool, destinationRev, changeNumber, changeTime int) error {
	if change.SourcePath == "" {
		return fmt.Errorf("missing source path")
	}
	if destinationExists {
		if change.Content == nil || change.ObjectID == "" {
			return fmt.Errorf("destination path already exists")
		}
		return w.writeChange(FileChange{
			Action:   FileModify,
			Path:     change.Path,
			Mode:     change.Mode,
			ObjectID: change.ObjectID,
			Content:  change.Content,
		}, branch, parentBranch, mergeBranches, changeNumber, changeTime)
	}
	sourceDepotFile := w.depotPath(branch, change.SourcePath)
	source, sourceExists := w.fileRevisions[sourceDepotFile]
	if !sourceExists {
		var matchedPath string
		for candidatePath, candidate := range w.fileRevisions {
			if candidate.active && strings.HasSuffix(candidatePath, "/"+strings.TrimPrefix(change.SourcePath, "/")) {
				if matchedPath != "" {
					matchedPath = ""
					break
				}
				matchedPath = candidatePath
				source = candidate
			}
		}
		if matchedPath == "" {
			if change.Content == nil || change.ObjectID == "" {
				return fmt.Errorf("source path has no previous revision")
			}
			return w.writeChange(FileChange{
				Action:   FileModify,
				Path:     change.Path,
				Mode:     change.Mode,
				ObjectID: change.ObjectID,
				Content:  change.Content,
			}, branch, parentBranch, mergeBranches, changeNumber, changeTime)
		}
		sourceDepotFile = matchedPath
	}
	fileType := w.fileTypeFor(sourceDepotFile)

	if change.Action == FileRename {
		sourceDeleteRev := source.revision + 1
		w.journal.WriteRev(sourceDepotFile, sourceDeleteRev, journal.Delete, fileType, changeNumber, source.lbrFile, source.lbrRev, changeTime)
		w.fileRevisions[sourceDepotFile] = fileRevision{revision: sourceDeleteRev, lbrFile: source.lbrFile, lbrRev: source.lbrRev, objectID: source.objectID}
	}

	w.journal.WriteRev(depotFile, destinationRev, journal.Add, fileType, changeNumber, source.lbrFile, source.lbrRev, changeTime)
	w.journal.WriteInteg(depotFile, sourceDepotFile, 0, source.revision, 0, destinationRev, journal.BranchFrom, journal.BranchInto, changeNumber)
	w.fileRevisions[depotFile] = fileRevision{revision: destinationRev, lbrFile: source.lbrFile, lbrRev: source.lbrRev, objectID: source.objectID, active: true}
	return nil
}

func (w *P4Writer) materializeBranch(branch, parentBranch string, changes []FileChange, changeNumber, changeTime int) error {
	if branch == "" || parentBranch == "" || branch == parentBranch || w.branchReady[branch] || !w.branchReady[parentBranch] {
		if branch != "" {
			w.branchReady[branch] = true
		}
		return nil
	}
	affected := make(map[string]bool)
	for _, change := range changes {
		affected[change.Path] = true
		if change.SourcePath != "" {
			affected[change.SourcePath] = true
		}
	}
	parentPrefix := w.depotPath(parentBranch, "") + "/"
	for parentDepotFile, parent := range w.fileRevisions {
		if !parent.active || !strings.HasPrefix(parentDepotFile, parentPrefix) {
			continue
		}
		sourcePath := strings.TrimPrefix(parentDepotFile, parentPrefix)
		if affected[sourcePath] {
			continue
		}
		depotFile := w.depotPath(branch, sourcePath)
		w.journal.WriteRev(depotFile, 1, journal.Add, w.fileTypeFor(parentDepotFile), changeNumber, parent.lbrFile, parent.lbrRev, changeTime)
		w.journal.WriteInteg(depotFile, parentDepotFile, 0, parent.revision, 0, 1, journal.BranchFrom, journal.BranchInto, changeNumber)
		w.fileRevisions[depotFile] = fileRevision{revision: 1, lbrFile: parent.lbrFile, lbrRev: parent.lbrRev, objectID: parent.objectID, active: true}
	}
	w.branchReady[branch] = true
	return nil
}

func (w *P4Writer) depotPath(branch, sourcePath string) string {
	var depotFile string
	if branch == "" {
		depotFile = "//" + path.Join(w.importDepot, strings.TrimPrefix(sourcePath, "/"))
	} else {
		depotFile = "//" + path.Join(w.importDepot, branch, strings.TrimPrefix(sourcePath, "/"))
	}
	if w.caseInsensitive {
		return strings.ToLower(depotFile)
	}
	return depotFile
}

func (w *P4Writer) fileTypeFor(depotFile string) journal.FileType {
	fileType := journal.CText
	for _, typeMap := range w.typeMaps {
		if typeMap.Pattern != nil && typeMap.Pattern.MatchString(depotFile) {
			fileType = typeMap.FileType
		}
	}
	return fileType
}

func (w *P4Writer) writeArchive(filename string, content io.ReadCloser, fileType journal.FileType) error {
	defer content.Close()
	archive, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer archive.Close()

	var destination io.Writer = archive
	var compressor *gzip.Writer
	if fileType == journal.CText || fileType == journal.Binary {
		compressor = gzip.NewWriter(archive)
		destination = compressor
	}
	if w.convertCRLF && (fileType == journal.CText || fileType == journal.UText) {
		err = copyNormalizingCRLF(destination, content)
	} else {
		_, err = io.Copy(destination, content)
	}
	if closeErr := closeGzip(compressor); err == nil {
		err = closeErr
	}
	return err
}

func closeGzip(writer *gzip.Writer) error {
	if writer == nil {
		return nil
	}
	return writer.Close()
}

func copyNormalizingCRLF(destination io.Writer, source io.Reader) error {
	reader := bufio.NewReader(source)
	for {
		value, err := reader.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if value == '\r' {
			next, err := reader.Peek(1)
			if err == nil && next[0] == '\n' {
				if _, err := reader.ReadByte(); err != nil {
					return err
				}
				value = '\n'
			}
		}
		if _, err := destination.Write([]byte{value}); err != nil {
			return err
		}
	}
}

// Close waits for all staged archive work to complete. It is safe to call more
// than once.
func (w *P4Writer) Close() error {
	w.archiveMu.Lock()
	if w.closed {
		w.archiveMu.Unlock()
		return w.archiveError()
	}
	w.closed = true
	close(w.archiveJobs)
	w.archiveMu.Unlock()
	w.archiveWG.Wait()
	return w.archiveError()
}

func (w *P4Writer) stageArchive(archiveFile string, content io.ReadCloser, fileType journal.FileType) error {
	defer content.Close()
	if err := os.MkdirAll(w.stagingDir, 0755); err != nil {
		return err
	}
	stagedFile, err := os.CreateTemp(w.stagingDir, "blob-")
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(stagedFile, content)
	closeErr := stagedFile.Close()
	if copyErr != nil {
		os.Remove(stagedFile.Name())
		return copyErr
	}
	if closeErr != nil {
		os.Remove(stagedFile.Name())
		return closeErr
	}
	w.archiveJobs <- archiveJob{stagedFile: stagedFile.Name(), archiveFile: archiveFile, fileType: fileType}
	return w.archiveError()
}

func (w *P4Writer) runArchiveWorker() {
	defer w.archiveWG.Done()
	for job := range w.archiveJobs {
		staged, err := os.Open(job.stagedFile)
		if err == nil {
			err = w.writeArchive(job.archiveFile, staged, job.fileType)
		}
		if removeErr := os.Remove(job.stagedFile); err == nil {
			err = removeErr
		}
		if err != nil {
			w.archiveMu.Lock()
			if w.archiveErr == nil {
				w.archiveErr = err
			}
			w.archiveMu.Unlock()
		}
	}
}

func (w *P4Writer) archiveError() error {
	w.archiveMu.Lock()
	defer w.archiveMu.Unlock()
	return w.archiveErr
}
