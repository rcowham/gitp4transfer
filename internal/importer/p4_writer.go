package importer

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/rcowham/gitp4transfer/journal"
)

// CommitSource produces source-neutral commits in import order.
type CommitSource interface {
	Next() (*Commit, error)
}

// P4WriterOptions configures a linear Perforce journal and archive import.
type P4WriterOptions struct {
	ImportDepot string
	ArchiveRoot string
	Journal     io.Writer
	User        string
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

// P4Writer writes source-neutral commits to Perforce journal and archive paths.
// It handles linear add, edit, and delete history plus duplicate blob librarian
// references. Branch and merge integration records remain a later migration step.
type P4Writer struct {
	journal       journal.Journal
	importDepot   string
	archiveRoot   string
	user          string
	changeNumber  int
	fileRevisions map[string]fileRevision
	blobArchives  map[string]blobArchive
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
	writer := &P4Writer{
		importDepot:   strings.Trim(options.ImportDepot, "/"),
		archiveRoot:   options.ArchiveRoot,
		user:          options.User,
		fileRevisions: make(map[string]fileRevision),
		blobArchives:  make(map[string]blobArchive),
	}
	writer.journal.SetWriter(options.Journal)
	writer.journal.WriteHeader(strings.TrimPrefix(options.ImportDepot, "//"), false)
	return writer, nil
}

// WriteSource consumes commits until io.EOF.
func (w *P4Writer) WriteSource(source CommitSource) error {
	for {
		commit, err := source.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := w.WriteCommit(commit); err != nil {
			return err
		}
	}
}

// WriteCommit writes one source commit as a Perforce changelist.
func (w *P4Writer) WriteCommit(commit *Commit) error {
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
		w.journal.WriteRev(depotFile, revision, journal.Delete, journal.UBinary, changeNumber, previous.lbrFile, previous.lbrRev, changeTime)
		w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: previous.lbrFile, lbrRev: previous.lbrRev}
		return nil
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
		archiveFile := path.Join(depotRoot, fmt.Sprintf("1.%d", changeNumber))
		archive, err := os.Create(archiveFile)
		if err != nil {
			content.Close()
			return err
		}
		_, copyErr := io.Copy(archive, content)
		closeErr := content.Close()
		archiveErr := archive.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if archiveErr != nil {
			return archiveErr
		}
		lbr = blobArchive{lbrFile: depotFile, lbrRev: changeNumber}
		w.blobArchives[change.ObjectID] = lbr
	}

	action := journal.Add
	if exists {
		action = journal.Edit
	}
	w.journal.WriteRev(depotFile, revision, action, journal.UBinary, changeNumber, lbr.lbrFile, lbr.lbrRev, changeTime)
	w.fileRevisions[depotFile] = fileRevision{revision: revision, lbrFile: lbr.lbrFile, lbrRev: lbr.lbrRev}
	return nil
}
