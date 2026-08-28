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
	TypeMaps        []TypeMap
	ConvertCRLF     bool
	ParallelWorkers int
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
	journal       journal.Journal
	importDepot   string
	archiveRoot   string
	user          string
	typeMaps      []TypeMap
	convertCRLF   bool
	stagingDir    string
	changeNumber  int
	fileRevisions map[string]fileRevision
	blobArchives  map[string]blobArchive
	archiveJobs   chan archiveJob
	archiveWG     sync.WaitGroup
	archiveMu     sync.Mutex
	archiveErr    error
	closed        bool
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
		importDepot:   strings.Trim(options.ImportDepot, "/"),
		archiveRoot:   options.ArchiveRoot,
		user:          options.User,
		typeMaps:      options.TypeMaps,
		convertCRLF:   options.ConvertCRLF,
		stagingDir:    path.Join(options.ArchiveRoot, ".gitp4transfer-staging"),
		fileRevisions: make(map[string]fileRevision),
		blobArchives:  make(map[string]blobArchive),
		archiveJobs:   make(chan archiveJob, options.ParallelWorkers),
	}
	writer.journal.SetWriter(options.Journal)
	writer.journal.WriteHeader(strings.TrimPrefix(options.ImportDepot, "//"), false)
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
	w.journal.WriteChange(changeNumber, w.user, commit.Message, changeTime)

	for _, change := range commit.Files {
		if err := w.writeChange(change, changeNumber, changeTime); err != nil {
			return fmt.Errorf("commit %s, path %s: %w", commit.ID, change.Path, err)
		}
	}
	return nil
}

func (w *P4Writer) writeChange(change FileChange, changeNumber, changeTime int) error {
	depotFile := "//" + path.Join(w.importDepot, strings.TrimPrefix(change.Path, "/"))
	if !strings.HasPrefix(depotFile, "//") {
		return fmt.Errorf("invalid depot path %q", change.Path)
	}
	previous, exists := w.fileRevisions[depotFile]
	revision := previous.revision + 1

	if change.Action == FileDelete {
		if !exists {
			return fmt.Errorf("cannot delete path with no previous revision")
		}
		w.journal.WriteRev(depotFile, revision, journal.Delete, w.fileTypeFor(depotFile), changeNumber, previous.lbrFile, previous.lbrRev, changeTime)
		w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: previous.lbrFile, lbrRev: previous.lbrRev}
		return nil
	}
	if change.Action == FileCopy || change.Action == FileRename {
		return w.writeCopyOrRename(change, depotFile, exists, revision, changeNumber, changeTime)
	}
	if change.Action != FileModify {
		return fmt.Errorf("unsupported source action %s", change.Action)
	}
	if change.Content == nil {
		return fmt.Errorf("missing blob content")
	}

	lbr := blobArchive{}
	if known, ok := w.blobArchives[change.ObjectID]; ok {
		lbr = known
	} else {
		content, err := change.Content()
		if err != nil {
			return err
		}
		depotRoot := path.Join(w.archiveRoot, strings.TrimPrefix(depotFile, "//")+",d")
		if err := os.MkdirAll(depotRoot, 0755); err != nil {
			content.Close()
			return err
		}
		fileType := w.fileTypeFor(depotFile)
		archiveFile := path.Join(depotRoot, fmt.Sprintf("1.%d", changeNumber))
		if fileType == journal.CText || fileType == journal.Binary {
			archiveFile += ".gz"
		}
		if err := w.stageArchive(archiveFile, content, fileType); err != nil {
			return err
		}
		lbr = blobArchive{lbrFile: depotFile, lbrRev: changeNumber}
		w.blobArchives[change.ObjectID] = lbr
	}

	action := journal.Add
	if exists {
		action = journal.Edit
	}
	w.journal.WriteRev(depotFile, revision, action, w.fileTypeFor(depotFile), changeNumber, lbr.lbrFile, lbr.lbrRev, changeTime)
	w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: lbr.lbrFile, lbrRev: lbr.lbrRev}
	return nil
}

func (w *P4Writer) writeCopyOrRename(change FileChange, depotFile string, destinationExists bool, destinationRev, changeNumber, changeTime int) error {
	if change.SourcePath == "" {
		return fmt.Errorf("missing source path")
	}
	if destinationExists {
		return fmt.Errorf("destination path already exists")
	}
	sourceDepotFile := "//" + path.Join(w.importDepot, strings.TrimPrefix(change.SourcePath, "/"))
	source, sourceExists := w.fileRevisions[sourceDepotFile]
	if !sourceExists {
		return fmt.Errorf("source path has no previous revision")
	}
	fileType := w.fileTypeFor(sourceDepotFile)

	if change.Action == FileRename {
		sourceDeleteRev := source.revision + 1
		w.journal.WriteRev(sourceDepotFile, sourceDeleteRev, journal.Delete, fileType, changeNumber, source.lbrFile, source.lbrRev, changeTime)
		w.fileRevisions[sourceDepotFile] = fileRevision{revision: sourceDeleteRev, lbrFile: source.lbrFile, lbrRev: source.lbrRev}
	}

	w.journal.WriteRev(depotFile, destinationRev, journal.Add, fileType, changeNumber, source.lbrFile, source.lbrRev, changeTime)
	w.journal.WriteInteg(depotFile, sourceDepotFile, 0, source.revision, 0, destinationRev, journal.BranchFrom, journal.BranchInto, changeNumber)
	w.fileRevisions[depotFile] = fileRevision{revision: destinationRev, lbrFile: source.lbrFile, lbrRev: source.lbrRev}
	return nil
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
