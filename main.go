package main

// gitp4transfer program
// This processes a git fast-export file and writes the following:
//   * a journal file with p4 records (written in 2004.2 format for simplicity)
//   * accompanying archive/depot files per revision.
// Note that the latter use CText filetype (compressed) rather than RCS
//
// Design:
// The main loop GitParse():
//     Reads the next record from the git file using libfastimport
//     Blobs are sent to a channel to be compressed and saved using their Mark (ID) as a filename
//         The idea is to write them to disk ASAP and later on to rename the files to their final
//         name and then release the file to GC - we want to avoid using up too much memory!
//     Other commands are processed and collected per GitCommit (e.g. File Modify/Delete/Rename/Copy records
//     are attached to the Commit).
//     As each Commit is processed, journal records are written.
//
// Global data structures:
// * Hash of GitCommit records
// * Hash of Blob records (without actual data)
// * Hash of GitFile action records
//
// Notes:
// * Plastic SCM writes git -fast-export files that are invalid for git! Thus we have to filter records
//   and handle anomalies, e.g.
//   - rename of a file already renamed
//   - delete of a file which has been renamed already
//   - etc.

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"               // profiling only
	_ "net/http/pprof" // profiling only
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alitto/pond"
	"github.com/emicklei/dot"
	"github.com/h2non/filetype"
	"github.com/rcowham/gitp4transfer/config"
	journal "github.com/rcowham/gitp4transfer/journal"
	node "github.com/rcowham/gitp4transfer/node"
	libfastimport "github.com/rcowham/go-libgitfastimport"

	"github.com/perforce/p4prometheus/version"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
)

var defaultP4user = "git-user" // Default user if non found

func Humanize(b int) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB",
		float64(b)/float64(div), "kMGTPE"[exp])
}

type GitParserOptions struct {
	config          *config.Config
	gitImportFile   string
	archiveRoot     string
	dryRun          bool
	dummyArchives   bool
	caseInsensitive bool // If true then create case insensitive checkpoint for Linux and lowercase archive files
	graphFile       string
	maxCommits      int
	debugCommit     int // For debug breakpoint
}

type GitAction int

const (
	unknown GitAction = iota
	modify
	delete
	copy
	rename
)

func (a GitAction) String() string {
	return [...]string{"Unknown", "Modify", "Delete", "Copy", "Rename"}[a]
}

// Performs simple hash
func getBlobIDPath(rootDir string, blobID int) (dir string, name string) {
	n := fmt.Sprintf("%08d", blobID)
	d := path.Join(rootDir, n[0:2], n[2:5], n[5:8])
	n = path.Join(d, n)
	return d, n
}

func writeBlob(rootDir string, blobID int, data *string) error {
	dir, name := getBlobIDPath(rootDir, blobID)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create %s: %v", dir, err)
		return err
	}
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprint(f, *data)
	if err != nil {
		return err
	}
	return nil
}

var gitFileID = 0 // Unique ID - set by newGitFile

// GitBlob - wrapper around CmdBlob
type GitBlob struct {
	blob         *libfastimport.CmdBlob
	compressed   bool
	fileType     journal.FileType
	hasData      bool
	dataRemoved  bool
	saved        bool
	lock         sync.RWMutex
	blobDirPath  string
	blobFileName string // Temporary storage location once read
	gitFileIDs   []int  // list of gitfiles referring to this blob
}

func newGitBlob(blob *libfastimport.CmdBlob) *GitBlob {
	b := &GitBlob{blob: blob, hasData: true, gitFileIDs: make([]int, 0), fileType: journal.CText}
	if blob != nil { // Construct a suitable filename
		filename := fmt.Sprintf("%07d", b.blob.Mark)
		b.blobFileName = filename
		b.blobDirPath = path.Join(filename[:1], filename[1:4])
	}
	return b
}

type GitFileMap map[int]*GitFile // Maps Gitfile ID to *GF
type BlobMap map[int]*GitBlob    // Maps Blob ID to *blob

// BlobFileMatcher - maps blobs to files and vice versa
type BlobFileMatcher struct {
	logger     *logrus.Logger
	gitFileMap GitFileMap
	blobMap    BlobMap
}

func newBlobFileMatcher(logger *logrus.Logger) *BlobFileMatcher {
	return &BlobFileMatcher{logger: logger, gitFileMap: GitFileMap{}, blobMap: BlobMap{}}
}

func (m *BlobFileMatcher) addBlob(b *GitBlob) {
	if _, ok := m.blobMap[b.blob.Mark]; !ok {
		m.blobMap[b.blob.Mark] = b
	} else {
		m.logger.Errorf("Found duplicate blob: %d", b.blob.Mark)
	}
}

func (m *BlobFileMatcher) addGitFile(gf *GitFile) {
	if _, ok := m.gitFileMap[gf.ID]; !ok {
		m.gitFileMap[gf.ID] = gf
	} else {
		m.logger.Errorf("Found duplicate gitfile: %d", gf.ID)
	}
	gf.blob.gitFileIDs = append(gf.blob.gitFileIDs, gf.ID) // Multiple gitFiles can reference same blob
}

func (m *BlobFileMatcher) removeGitFile(gf *GitFile) {
	if gf.blob == nil {
		return
	}
	oldIDs := gf.blob.gitFileIDs
	gf.blob.gitFileIDs = make([]int, 0)
	for _, id := range oldIDs {
		if id != gf.ID {
			gf.blob.gitFileIDs = append(gf.blob.gitFileIDs, id) // Multiple gitFiles can reference same blob
		}
	}
}

func (m *BlobFileMatcher) getBlob(blobID int) *GitBlob {
	if b, ok := m.blobMap[blobID]; ok {
		return b
	} else {
		m.logger.Errorf("Failed to find blob: %d", blobID)
		return nil
	}
}

// For a GitFile which is a duplicate, copies the original lbrFile/lbrRev
func (m *BlobFileMatcher) updateDuplicateGitFile(gf *GitFile) {
	b, ok := m.blobMap[gf.blob.blob.Mark]
	if !ok {
		gf.logger.Errorf("Failed to find GitFile ID1: %d %s", gf.blob.blob.Mark, gf.p4.depotFile)
		return
	}
	if len(b.gitFileIDs) == 0 {
		gf.logger.Errorf("Failed to find GitFile ID2: %d %s", gf.blob.blob.Mark, gf.p4.depotFile)
		return
	}
	origGF, ok := m.gitFileMap[b.gitFileIDs[0]]
	if !ok {
		gf.logger.Errorf("Failed to find GitFile ID: %d %s", b.gitFileIDs[0], gf.p4.depotFile)
		return
	}
	gf.p4.lbrFile = origGF.p4.lbrFile
	gf.p4.lbrRev = origGF.p4.lbrRev
	gf.logger.Debugf("Duplicate file %s %d of: %s %d", gf.p4.depotFile, gf.p4.rev, gf.p4.lbrFile, gf.p4.lbrRev)
	if gf.p4.lbrFile == "" {
		gf.logger.Errorf("Invalid referenced lbrFile in duplicate %s %d", gf.p4.depotFile, gf.p4.rev)
	}
}

// GitFile - A git file record - modify/delete/copy/move
type GitFile struct {
	name             string // Git filename (target for rename/copy)
	srcName          string // Name of git source file for rename/copy
	ID               int
	size             int
	p4               *P4File
	duplicateArchive bool
	action           GitAction
	actionInvalid    bool // Set to true if this is overriden by a later action in same commit
	isBranch         bool
	isMerge          bool
	isDirtyRename    bool // Rename where content changed in same commit
	isPseudoRename   bool // Rename where source file should not be deleted as updated in same commit
	isDoubleRename   bool // Second rename of same source file in same commit
	fileType         journal.FileType
	compressed       bool
	blob             *GitBlob
	logger           *logrus.Logger
	commit           *GitCommit
}

// Information for a P4File
type P4File struct {
	depotFile         string // Full depot path
	rev               int    // Depot rev
	lbrRev            int    // Lbr rev - usually same as Depot rev
	lbrFile           string // Lbr file - usually same as Depot file
	srcDepotFile      string // Depot path of source for a rename or a branched file
	srcRev            int    // Rev of source for a rename or a branched file
	origTargDepotFile string // For branched/merged rename - Depot path of original target of the rename
	origTargDepotRev  int    // For branched/merged rename - rev of original target of the rename
	origSrcDepotFile  string // For branched/merged rename - Depot path of original source of the rename
	origSrcDepotRev   int    // For branched/merged rename - rev of original target of the rename
	archiveFile       string
	p4action          journal.FileAction
}

func newGitFile(gf *GitFile) *GitFile {
	gitFileID += 1
	gf.ID = gitFileID
	if gf.fileType == 0 {
		gf.fileType = journal.CText // Default - may be overwritten later
	}
	gf.p4 = &P4File{}
	gf.updateFileDetails()
	return gf
}

// GitCommit - A git commit
type GitCommit struct {
	commit       *libfastimport.CmdCommit
	user         string
	branch       string   // branch name
	prevBranch   string   // set if first commit on new branch
	parentBranch string   // set to ancestor of current branch
	mergeBranch  string   // set if commit is a merge - assumes only 1 merge candidate!
	commitSize   int      // Size of all files in this commit - useful for memory sizing
	gNode        dot.Node // Optional link to GraphizNode
	files        []*GitFile
}

// HasPrefix tests whether the string s begins with prefix.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

func getUserFromEmail(email string) string {
	if email == "" {
		return defaultP4user
	}
	parts := strings.Split(email, "@")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return defaultP4user
}

func newGitCommit(commit *libfastimport.CmdCommit, commitSize int) *GitCommit {
	user := getUserFromEmail(commit.Author.Email)
	gc := &GitCommit{commit: commit, user: user, commitSize: commitSize, files: make([]*GitFile, 0)}
	gc.branch = strings.Replace(commit.Ref, "refs/heads/", "", 1)
	if hasPrefix(gc.branch, "refs/tags") || hasPrefix(gc.branch, "refs/remote") {
		gc.branch = ""
	}
	return gc
}

func (gc *GitCommit) findGitFile(name string) *GitFile {
	for _, gf := range gc.files {
		if gf.name == name {
			return gf
		}
	}
	return nil
}

func (gc *GitCommit) findGitFileRename(fromName string) *GitFile {
	for _, gf := range gc.files {
		if gf.srcName == fromName {
			return gf
		}
	}
	return nil
}

func (gc *GitCommit) ref() string {
	return fmt.Sprintf("%s:%d", gc.branch, gc.commit.Mark)
}

type CommitMap map[int]*GitCommit

type RevChange struct { // Struct to remember revs and changes per depotFile
	rev     int
	chgNo   int
	lbrRev  int    // Normally same as chgNo but not for renames/copies
	lbrFile string // Normally same as depotFile byt not for renames/copies
	action  GitAction
}

type BranchRegex struct {
	nameRegex *regexp.Regexp
	prefix    string
}

// BranchNameMapper - maps blobs to files and vice versa
type BranchNameMapper struct {
	branchMaps []BranchRegex
}

func newBranchNameMapper(config *config.Config) *BranchNameMapper {
	bm := &BranchNameMapper{
		branchMaps: make([]BranchRegex, 0),
	}
	if config == nil {
		return bm
	}
	for _, m := range config.BranchMappings {
		br := BranchRegex{
			nameRegex: regexp.MustCompile(m.Name),
			prefix:    m.Prefix,
		}
		bm.branchMaps = append(bm.branchMaps, br)
	}
	return bm
}

func (bm *BranchNameMapper) branchName(name string) string {
	for _, m := range bm.branchMaps {
		if m.nameRegex.MatchString(name) {
			return m.prefix + name
		}
	}
	return name
}

// GitP4Transfer - Transfer via git fast-export file
type GitP4Transfer struct {
	logger           *logrus.Logger
	gitChan          chan GitCommit
	opts             GitParserOptions
	depotFileRevs    map[string]*RevChange       // Map depotFile to latest rev/chg
	depotFileTypes   map[string]journal.FileType // Map depotFile#rev to filetype (for renames/branching)
	blobFileMatcher  *BlobFileMatcher            // Map between gitfile ID and record
	commits          map[int]*GitCommit
	testInput        string                // For testing only
	graph            *dot.Graph            // If outputting a graph
	filesOnBranch    map[string]*node.Node // Records current state of git tree per branch
	branchNameMapper *BranchNameMapper
}

func NewGitP4Transfer(logger *logrus.Logger, opts *GitParserOptions) (*GitP4Transfer, error) {

	g := &GitP4Transfer{logger: logger,
		opts:             *opts,
		depotFileRevs:    make(map[string]*RevChange),
		blobFileMatcher:  newBlobFileMatcher(logger),
		depotFileTypes:   make(map[string]journal.FileType),
		commits:          make(map[int]*GitCommit),
		filesOnBranch:    make(map[string]*node.Node),
		branchNameMapper: newBranchNameMapper(opts.config)}
	return g, nil
}

// Convert a branch name to a full depot path (using global options)
func (gf *GitFile) getDepotPath(opts GitParserOptions, mapper *BranchNameMapper, branch string, name string) string {
	bname := mapper.branchName(branch)
	if len(opts.config.ImportPath) == 0 {
		return fmt.Sprintf("//%s/%s/%s", opts.config.ImportDepot, bname, name)
	} else {
		return fmt.Sprintf("//%s/%s/%s/%s", opts.config.ImportDepot, opts.config.ImportPath, bname, name)
	}
}

// Sets the depot path according to branch name and whether this is a branched files
func (gf *GitFile) setDepotPaths(opts GitParserOptions, mapper *BranchNameMapper, fileRevs *map[string]*RevChange, gc *GitCommit) {
	gf.commit = gc
	gf.p4.depotFile = gf.getDepotPath(opts, mapper, gc.branch, gf.name)
	if gf.srcName != "" {
		gf.p4.srcDepotFile = gf.getDepotPath(opts, mapper, gc.branch, gf.srcName)
	} else if gc.prevBranch != "" {
		gf.srcName = gf.name
		gf.isBranch = true
		gf.p4.srcDepotFile = gf.getDepotPath(opts, mapper, gc.prevBranch, gf.srcName)
	}
	if gc.mergeBranch != "" && gc.mergeBranch != gc.branch {
		if gf.srcName == "" {
			srcDepotFile := gf.getDepotPath(opts, mapper, gc.mergeBranch, gf.name)
			if fr, ok := (*fileRevs)[srcDepotFile]; ok {
				if gf.action == delete {
					if fr.action == delete {
						gf.isMerge = true
					}
				} else if fr.action != delete {
					gf.isMerge = true
				}
			}
			if gf.isMerge {
				gf.srcName = gf.name
				gf.p4.srcDepotFile = srcDepotFile
			}
		} else {
			gf.isMerge = true
		}
	}
}

// Sets compression option and distinguishes binary/text according to mimetype or extension
func (b *GitBlob) setCompressionDetails() {
	b.fileType = journal.CText
	b.compressed = true
	l := len(b.blob.Data)
	if l > 261 {
		l = 261
	}
	head := []byte(b.blob.Data[:l])
	if filetype.IsImage(head) || filetype.IsVideo(head) || filetype.IsArchive(head) || filetype.IsAudio(head) {
		b.fileType = journal.UBinary
		b.compressed = false
		return
	}
	if filetype.IsDocument(head) {
		b.fileType = journal.Binary
		kind, _ := filetype.Match(head)
		switch kind.Extension {
		case "docx", "dotx", "potx", "ppsx", "pptx", "vsdx", "vstx", "xlsx", "xltx":
			b.compressed = false
		}
	}
}

// Sets p4 action
func (gf *GitFile) updateFileDetails() {
	switch gf.action {
	case delete:
		gf.p4.p4action = journal.Delete
		return
	case rename:
		gf.p4.p4action = journal.Rename
		return
	case modify:
		gf.p4.p4action = journal.Edit
	}
}

func getOID(dataref string) (int, error) {
	if !strings.HasPrefix(dataref, ":") {
		return 0, errors.New("invalid dataref")
	}
	return strconv.Atoi(dataref[1:])
}

// SaveBlob will save it to a temp dir, e.g. 1234567 -> 1/234/1234567[.gz]
// Later the file will be moved to the required depot location
// Uses a provided pool to get concurrency
// Allow for dummy data to be saved (used to speed up large conversions to check structure)
func (b *GitBlob) SaveBlob(pool *pond.WorkerPool, archiveRoot string, dummyFlag bool, matcher *BlobFileMatcher) error {
	if b.blob == nil || !b.hasData {
		matcher.logger.Debugf("NoBlobToSave")
		return nil
	}
	b.lock.Lock()
	b.setCompressionDetails()
	matcher.logger.Debugf("BlobFileType: %d: %s", b.blob.Mark, b.fileType)
	// Do the work in pool worker threads for concurrency, especially with compression
	rootDir := path.Join(archiveRoot, b.blobDirPath)
	if b.compressed {
		fname := path.Join(rootDir, fmt.Sprintf("%s.gz", b.blobFileName))
		matcher.logger.Debugf("SavingBlobCompressed: %s", fname)
		data := b.blob.Data
		if dummyFlag {
			data = fmt.Sprintf("%d", b.blob.Mark)
		}
		b.lock.Unlock()
		pool.Submit(
			func(b *GitBlob, rootDir string, fname string, data string) func() {
				return func() {
					err := os.MkdirAll(rootDir, 0755)
					if err != nil {
						panic(err)
					}
					f, err := os.Create(fname)
					if err != nil {
						panic(err)
					}
					defer f.Close()
					zw := gzip.NewWriter(f)
					defer zw.Close()
					b.lock.Lock()
					defer b.lock.Unlock()
					_, err = zw.Write([]byte(data))
					if err != nil {
						panic(err)
					}
					b.saved = true
					b.blob.Data = "" // Allow contents to be GC'ed
					b.dataRemoved = true
				}
			}(b, rootDir, fname, data))
	} else {
		fname := path.Join(rootDir, b.blobFileName)
		matcher.logger.Debugf("SavingBlobNotCompressed: %s", fname)
		data := b.blob.Data
		b.lock.Unlock()
		if dummyFlag {
			data = fmt.Sprintf("%d", b.blob.Mark)
		}
		pool.Submit(
			func(b *GitBlob, rootDir string, fname string, data string) func() {
				return func() {
					err := os.MkdirAll(rootDir, 0755)
					if err != nil {
						panic(err)
					}
					f, err := os.Create(fname)
					if err != nil {
						panic(err)
					}
					defer f.Close()
					b.lock.Lock()
					defer b.lock.Unlock()
					fmt.Fprint(f, data)
					if err != nil {
						panic(err)
					}
					b.saved = true
					b.blob.Data = "" // Allow contents to be GC'ed
					b.dataRemoved = true
				}
			}(b, rootDir, fname, data))
	}
	return nil
}

func (gf *GitFile) CreateArchiveFile(pool *pond.WorkerPool, caseInsensitive bool, depotRoot string, matcher *BlobFileMatcher, changeNo int) {
	if gf.action == delete || (gf.action == rename && !gf.isDirtyRename && !gf.isPseudoRename && !gf.isDoubleRename) ||
		gf.blob == nil || !gf.blob.hasData {
		return
	}
	// Fix wildcards
	depotFile := journal.ReplaceWildcards(gf.p4.depotFile[2:])
	if caseInsensitive {
		depotFile = strings.ToLower(depotFile)
	}
	rootDir := path.Join(depotRoot, fmt.Sprintf("%s,d", depotFile))
	if gf.blob.blobFileName == "" {
		gf.logger.Debugf(fmt.Sprintf("NoBlobFound: %s", depotFile))
		return
	}
	bname := path.Join(depotRoot, gf.blob.blobDirPath, gf.blob.blobFileName)
	fname := path.Join(rootDir, fmt.Sprintf("1.%d", changeNo))
	if gf.compressed {
		bname = path.Join(depotRoot, gf.blob.blobDirPath, fmt.Sprintf("%s.gz", gf.blob.blobFileName))
		fname = path.Join(rootDir, fmt.Sprintf("1.%d.gz", changeNo))
	}
	gf.logger.Debugf("CreateArchiveFile: %s -> %s", bname, fname)
	err := os.MkdirAll(rootDir, 0755)
	if err != nil {
		gf.logger.Errorf("Failed to Mkdir: %s - %v", rootDir, err)
		return
	}
	// Rename the archive file for first copy, expect a duplicate otherwise and save references to it
	// By submitting to the pool we allow files to be written first (we hope!)
	if !gf.duplicateArchive {
		if pool != nil {
			pool.Submit(
				func(gf *GitFile, bname string, fname string) func() {
					return func() {
						for {
							loopCount := 0
							gf.blob.lock.RLock()
							if !gf.blob.saved {
								gf.logger.Warnf("Waiting for blob to be saved: %d", gf.blob.blob.Mark)
								gf.blob.lock.RUnlock()
								time.Sleep(1 * time.Second)
								loopCount += 1
								if loopCount > 10 {
									gf.logger.Errorf("Rename wait failed: %d seconds", loopCount)
									break
								}
							} else {
								gf.blob.lock.RUnlock()
								break
							}
						}
						err = os.Rename(bname, fname)
						if err != nil {
							gf.logger.Errorf("Failed to Rename after waiting: %v", err)
						}
					}
				}(gf, bname, fname))
		} else {
			err = os.Rename(bname, fname)
			if err != nil {
				gf.logger.Errorf("Failed to Rename: %v", err)
			}
		}
	}
}

func minval(val, min int) int { // Minimum of specified val or min
	if val < min {
		return min
	}
	return val
}

func maxval(val, max int) int { // Maxiumum of specified val or max
	if val > max {
		return max
	}
	return val
}

// WriteJournal writes journal record for a GitFile
func (gf *GitFile) WriteJournal(j *journal.Journal, c *GitCommit) {
	dt := int(c.commit.Author.Time.Unix())
	chgNo := c.commit.Mark
	fileType := gf.fileType
	if fileType == 0 {
		gf.logger.Errorf("Unexpected filetype text: %s#%d", gf.p4.depotFile, gf.p4.rev)
		fileType = journal.CText
	}
	if gf.action == modify {
		if gf.isBranch || gf.isMerge {
			// we write rev for newly branched depot file, with link to old version
			action := journal.Add
			if gf.p4.rev > 1 { // TODO
				action = journal.Edit
			}
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			startFromRev := gf.p4.srcRev - 1
			endFromRev := gf.p4.srcRev
			startToRev := gf.p4.rev - 1
			endToRev := gf.p4.rev
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.BranchFrom, journal.DirtyBranchInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
		}
	} else if gf.action == delete {
		if gf.isMerge {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			startFromRev := gf.p4.srcRev - 1
			endFromRev := gf.p4.srcRev
			startToRev := gf.p4.rev - 1
			endToRev := gf.p4.rev
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
		}
	} else if gf.action == rename {
		if gf.isBranch { // Rename of a branched file - create integ records from parent
			if !gf.isPseudoRename && !gf.isDoubleRename {
				// Branched rename from dev -> main
				// Original file was dev/src
				// Branched as main/src -> main/targ
				// Create a delete rev for main/src
				j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
				// WriteInteg(toFile string, fromFile string, startFromRev int, endFromRev int, startToRev int, endToRev int,
				//            how IntegHow, reverseHow IntegHow, chgNo int)
				startFromRev := 0
				endFromRev := gf.p4.srcRev
				startToRev := 0
				endToRev := gf.p4.origSrcDepotRev
				// We create DeleteFrom integ between dev/src and main/src
				j.WriteInteg(gf.p4.srcDepotFile, gf.p4.origSrcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
			}
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			startFromRev := gf.p4.origSrcDepotRev - 1
			endFromRev := gf.p4.origSrcDepotRev
			startToRev := gf.p4.rev - 1
			endToRev := gf.p4.rev
			// We create BranchFrom integ between dev/targ and main/targ
			j.WriteInteg(gf.p4.depotFile, gf.p4.origSrcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
		} else if gf.isMerge { // Merging a rename
			// Merge rename from dev -> main
			// Original rename was dev/src -> dev/targ
			// Merged as main/src -> main/targ

			// Setup integ BranchFrom dev/targ -> main/targ
			startFromRev := 0
			endFromRev := gf.p4.origTargDepotRev
			startToRev := minval(gf.p4.rev-1, 0)
			endToRev := gf.p4.rev
			if gf.isPseudoRename || gf.isDoubleRename {
				endFromRev = gf.p4.srcRev // Not deleted for pseudo rename
			} else {
				j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
				// Integ DeleteFrom dev/src -> main/src, otherwise just do a normal rename
				if gf.p4.origSrcDepotFile != "" {
					startFromRev := 0
					endFromRev := gf.p4.origSrcDepotRev
					startToRev := gf.p4.srcRev - 1
					endToRev := gf.p4.srcRev
					j.WriteInteg(gf.p4.srcDepotFile, gf.p4.origSrcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
				}
			}
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			if gf.p4.origTargDepotFile != "" { // Merged rename
				j.WriteInteg(gf.p4.depotFile, gf.p4.origTargDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
			} else { // Simple rename
				startFromRev := minval(gf.p4.srcRev-2, 0)
				endFromRev := gf.p4.srcRev - 1
				startToRev := gf.p4.rev - 1
				endToRev := gf.p4.rev
				j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
			}
		} else { // Simple renamed file
			startFromRev := minval(gf.p4.srcRev-2, 0)
			endFromRev := gf.p4.srcRev - 1
			startToRev := gf.p4.rev - 1
			endToRev := gf.p4.rev
			if gf.isPseudoRename || gf.isDoubleRename {
				// The src is not deleted for pseudo/double renames
				endFromRev = gf.p4.srcRev
			} else {
				// Delete src record
				j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			}
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, startFromRev, endFromRev, startToRev, endToRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
		}
	} else {
		gf.logger.Errorf("Unexpected action: %s", gf.action.String())
	}
}

// DumpGit - incrementally parse the git file, collecting stats and optionally saving archives as we go
// Useful for parsing very large git fast-export files without loading too much into memory!
func (g *GitP4Transfer) DumpGit(saveFiles bool) {
	var buf io.Reader

	if g.testInput != "" {
		buf = strings.NewReader(g.testInput)
	} else {
		file, err := os.Open(g.opts.gitImportFile)
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", g.opts.gitImportFile, err)
			os.Exit(1)
		}
		defer file.Close()
		buf = bufio.NewReader(file)
	}

	commits := make(map[int]*GitCommit, 0)
	files := make(map[int]*GitFile, 0)
	extSizes := make(map[string]int)
	var currCommit *GitCommit
	var commitSize = 0

	f := libfastimport.NewFrontend(buf, nil, nil)
	for {
		cmd, err := f.ReadCmd()
		if err != nil {
			if err != io.EOF {
				fmt.Printf("ERROR: Failed to read cmd: %v", err)
			}
			break
		}
		switch ctype := cmd.(type) {
		case libfastimport.CmdBlob:
			blob := cmd.(libfastimport.CmdBlob)
			g.logger.Infof("Blob: Mark:%d OriginalOID:%s Size:%s", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
			size := len(blob.Data)
			commitSize += size
			gb := newGitBlob(&blob)
			// We write the blobs as we go to avoid using up too much memory
			if saveFiles {
				err = writeBlob(g.opts.archiveRoot, blob.Mark, &blob.Data)
				if err != nil {
					g.logger.Errorf("writeBlob: %+v", err)
				}
			}
			blob.Data = "" // Allow GC to avoid holding on to memory
			files[blob.Mark] = newGitFile(&GitFile{blob: gb, size: size, logger: g.logger})
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Infof("Reset: - %+v", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Infof("Commit:  %+v, size: %d/%s", commit, commitSize, Humanize(commitSize))
			currCommit = newGitCommit(&commit, commitSize)
			commitSize = 0
			commits[commit.Mark] = currCommit
		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Infof("CommitEnd:  %+v", commit)
			g.logger.Debugf("CommitSummary: Mark:%d Files:%d Size:%d/%s",
				currCommit.commit.Mark, len(currCommit.files), currCommit.commitSize, Humanize(currCommit.commitSize))
			for _, f := range currCommit.files {
				extSizes[filepath.Ext(f.name)] += f.size
			}
		case libfastimport.FileModify:
			f := cmd.(libfastimport.FileModify)
			g.logger.Infof("FileModify:  %+v", f)
			oid, err := getOID(f.DataRef)
			if err != nil {
				g.logger.Errorf("Failed to get oid: %+v", f)
			}
			gf, ok := files[oid]
			if ok {
				gf.name = string(f.Path)
				_, archName := getBlobIDPath(g.opts.archiveRoot, gf.blob.blob.Mark)
				gf.p4.archiveFile = archName
				g.logger.Infof("Path:%s Size:%d/%s Archive:%s", gf.name, gf.size, Humanize(gf.size), gf.p4.archiveFile)
				currCommit.files = append(currCommit.files, gf)
			}
		case libfastimport.FileDelete:
			f := cmd.(libfastimport.FileDelete)
			g.logger.Infof("FileDelete: Path:%s", f.Path)
		case libfastimport.FileCopy:
			f := cmd.(libfastimport.FileCopy)
			g.logger.Infof("FileCopy: Src:%s Dst:%s", f.Src, f.Dst)
		case libfastimport.FileRename:
			f := cmd.(libfastimport.FileRename)
			g.logger.Infof("FileRename: Src:%s Dst:%s", f.Src, f.Dst)
		case libfastimport.CmdTag:
			t := cmd.(libfastimport.CmdTag)
			g.logger.Infof("CmdTag: %+v", t)
		default:
			g.logger.Errorf("Not handled - found ctype %s cmd %+v", ctype, cmd)
			g.logger.Infof("Cmd type %T", cmd)
		}
	}
	for ext, size := range extSizes {
		g.logger.Infof("Ext %s: %s", ext, Humanize(size))
	}
}

// Is current head rev a deleted rev?
// func (g *GitP4Transfer) isSrcDeletedFile(gf *GitFile) bool {
// 	if gf.p4.srcDepotFile == "" {
// 		return false
// 	}
// 	if f, ok := g.depotFileRevs[gf.p4.srcDepotFile]; ok {
// 		return f.action == delete
// 	}
// 	return false
// }

// Maintain a list of latest revision counters indexed by depotFile and set lbrArchive/Rev
func (g *GitP4Transfer) updateDepotRevs(opts GitParserOptions, gf *GitFile, chgNo int) {
	prevAction := unknown
	if _, ok := g.depotFileRevs[gf.p4.depotFile]; !ok {
		g.depotFileRevs[gf.p4.depotFile] = &RevChange{rev: 0, chgNo: chgNo, lbrRev: chgNo,
			lbrFile: gf.p4.depotFile, action: gf.action}
	}
	if gf.action == delete && gf.srcName == "" && g.depotFileRevs[gf.p4.depotFile].rev != 0 {
		gf.fileType = g.getDepotFileTypes(gf.p4.depotFile, g.depotFileRevs[gf.p4.depotFile].rev)
	}
	g.depotFileRevs[gf.p4.depotFile].rev += 1
	if g.depotFileRevs[gf.p4.depotFile].rev > 1 {
		prevAction = g.depotFileRevs[gf.p4.depotFile].action
	}
	g.depotFileRevs[gf.p4.depotFile].action = gf.action
	g.depotFileRevs[gf.p4.depotFile].lbrRev = chgNo
	g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.depotFile
	gf.p4.lbrRev = chgNo
	gf.p4.lbrFile = gf.p4.depotFile
	gf.p4.rev = g.depotFileRevs[gf.p4.depotFile].rev
	if gf.action == modify {
		// modify defaults to edit, except when first rev or previously deleted
		if gf.p4.rev == 1 || prevAction == delete {
			gf.p4.p4action = journal.Add
		}
	}
	if gf.duplicateArchive {
		g.blobFileMatcher.updateDuplicateGitFile(gf)
		if !gf.isDirtyRename {
			g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
			g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
		}
	}
	if gf.srcName == "" { // Simple modify or delete
		g.recordDepotFileType(gf)
		g.logger.Debugf("UDR1: Submit: %d, depotFile: %s, rev %d, action %v, lbrFile %s, lbrRev %d, filetype %v",
			gf.commit.commit.Mark, gf.p4.depotFile, g.depotFileRevs[gf.p4.depotFile].rev,
			g.depotFileRevs[gf.p4.depotFile].action,
			g.depotFileRevs[gf.p4.depotFile].lbrFile,
			g.depotFileRevs[gf.p4.depotFile].lbrRev,
			gf.fileType)
		return
	}
	if gf.action != delete {
		gf.p4.p4action = journal.Add
	}
	if _, ok := g.depotFileRevs[gf.p4.srcDepotFile]; !ok {
		// Source file (typically rename/copy/branch) has not yet been seen
		if gf.action == delete {
			// A delete without a source file just becomes delete
			g.logger.Warnf("Integ of delete becomes delete: '%s' '%s'", gf.p4.depotFile, gf.p4.srcDepotFile)
			gf.p4.srcDepotFile = ""
			gf.srcName = ""
			gf.isMerge = false
		} else if gf.action == rename {
			g.logger.Debugf("Rename of branched file: '%s' <- '%s'", gf.p4.depotFile, gf.p4.srcDepotFile)
			// Create a record for the source of the rename referring to its branched source
			// dev/src -> main/src (delete) and main/targ
			srcOrigDepotPath := gf.getDepotPath(opts, g.branchNameMapper, gf.commit.parentBranch, gf.srcName)
			if _, ok := g.depotFileRevs[srcOrigDepotPath]; !ok {
				g.logger.Errorf("Failed to find original file: '%s'", srcOrigDepotPath)
			} else {
				gf.isBranch = true
				lbrFileOrig := g.depotFileRevs[srcOrigDepotPath].lbrFile
				lbrRevOrig := g.depotFileRevs[srcOrigDepotPath].lbrRev
				g.depotFileRevs[gf.p4.srcDepotFile] = &RevChange{rev: 1, chgNo: chgNo, lbrRev: lbrRevOrig,
					lbrFile: lbrFileOrig, action: delete}
				gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
				if !gf.isDirtyRename {
					gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
					gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
				}
				gf.p4.origSrcDepotFile = srcOrigDepotPath
				gf.p4.origSrcDepotRev = g.depotFileRevs[srcOrigDepotPath].rev
				gf.fileType = g.getDepotFileTypes(srcOrigDepotPath, g.depotFileRevs[srcOrigDepotPath].rev)
				g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
				g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
			}
		} else {
			// A copy or branch without a source file just becomes new file added on branch
			g.logger.Warnf("Copy/branch becomes add: '%s' <- '%s'", gf.p4.depotFile, gf.p4.srcDepotFile)
			gf.p4.srcDepotFile = ""
			gf.srcName = ""
			gf.isBranch = false
			gf.isMerge = false
		}
		g.recordDepotFileType(gf)
		g.logger.Debugf("UDR2: Submit: %d, depotFile: %s, rev %d, action %v, lbrFile %s, lbrRev %d, filetype %v",
			gf.commit.commit.Mark, gf.p4.depotFile, g.depotFileRevs[gf.p4.depotFile].rev,
			g.depotFileRevs[gf.p4.depotFile].action,
			g.depotFileRevs[gf.p4.depotFile].lbrFile,
			g.depotFileRevs[gf.p4.depotFile].lbrRev,
			gf.fileType)
		return
	}
	// At this point we have some form of merge/branch from a source file already known to us
	if gf.action == delete { // merge of delete
		gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
		gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
		gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
		g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
		g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
	} else if gf.action == rename { // Rename means old file is being deleted
		// If it is a merge of a rename then link to file on the branch, if we can find it!
		handled := false
		if gf.isMerge {
			// Rename of src->targ on main is being merged from rename of src->targ on mergeBranch (e.g. dev)
			// So check if we have records for //dev/src and //dev/targ
			targOrigDepotPath := gf.getDepotPath(opts, g.branchNameMapper, gf.commit.mergeBranch, gf.name)
			srcOrigDepotPath := gf.getDepotPath(opts, g.branchNameMapper, gf.commit.mergeBranch, gf.srcName)
			if _, ok := g.depotFileRevs[targOrigDepotPath]; !ok {
				targOrigDepotPath = ""
			}
			if _, ok := g.depotFileRevs[srcOrigDepotPath]; !ok {
				srcOrigDepotPath = ""
			}
			if targOrigDepotPath != "" && srcOrigDepotPath != "" {
				// Both //dev/src and //dev/targ exist
				handled = true
				if !gf.isPseudoRename && !gf.isDoubleRename {
					g.depotFileRevs[gf.p4.srcDepotFile].rev += 1
				}
				gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
				gf.p4.origTargDepotFile = targOrigDepotPath
				gf.p4.origTargDepotRev = g.depotFileRevs[targOrigDepotPath].rev
				gf.p4.origSrcDepotFile = srcOrigDepotPath
				gf.p4.origSrcDepotRev = g.depotFileRevs[srcOrigDepotPath].rev
				gf.fileType = g.getDepotFileTypes(targOrigDepotPath, g.depotFileRevs[targOrigDepotPath].rev)
				if g.depotFileRevs[targOrigDepotPath].action == delete {
					g.logger.Debugf("UDR7a: %s", gf.p4.depotFile) // We don't reference a deleted revision
					gf.p4.lbrFile = g.depotFileRevs[targOrigDepotPath].lbrFile
					gf.p4.lbrRev = g.depotFileRevs[targOrigDepotPath].lbrRev - 1
					g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
					g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
				} else {
					g.logger.Debugf("UDR7b: %s", gf.p4.depotFile)
					gf.p4.lbrFile = g.depotFileRevs[targOrigDepotPath].lbrFile
					gf.p4.lbrRev = g.depotFileRevs[targOrigDepotPath].lbrRev
					g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
					g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
				}
				g.recordDepotFileType(gf)
			}
		}
		if !handled {
			// Either //dev/src or //dev/targ didn't exist
			// So we handle as ??
			if !gf.isPseudoRename && !gf.isDoubleRename {
				g.depotFileRevs[gf.p4.srcDepotFile].rev += 1
				g.depotFileRevs[gf.p4.srcDepotFile].action = delete
			}
			gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
			gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
			gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
			srcRev := gf.p4.srcRev
			if srcRev > 1 { // TODO why?
				srcRev -= 1
			}
			gf.fileType = g.getDepotFileTypes(gf.p4.srcDepotFile, srcRev)
			g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
			g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
			g.recordDepotFileType(gf)
			g.logger.Debugf("UDR7c: %s", gf.p4.depotFile)
		}
	} else { // Copy/branch
		gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
		if g.depotFileRevs[gf.p4.srcDepotFile].action == delete { // Assume we are branching from previous rev
			gf.p4.srcRev -= 1
		}
		srcExists := g.depotFileTypeExists(gf.p4.srcDepotFile, gf.p4.srcRev)
		if !srcExists {
			g.logger.Debugf("UDR4: %s", gf.p4.depotFile)
			gf.isMerge = false
			gf.p4.srcDepotFile = ""
			gf.srcName = ""
		} else {
			gf.fileType = g.getDepotFileTypes(gf.p4.srcDepotFile, gf.p4.srcRev)
			if !gf.blob.hasData || gf.isMerge { // Copied but changed
				if g.depotFileRevs[gf.p4.srcDepotFile].action == delete {
					g.logger.Debugf("UDR5a: %s", gf.p4.depotFile) // We don't reference a deleted revision
				} else {
					g.logger.Debugf("UDR5b: %s", gf.p4.depotFile)
					gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
					gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
					g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
					g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
				}
			} else {
				g.logger.Debugf("UDR6: %s", gf.p4.depotFile)
				g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
				g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
			}
		}
		g.recordDepotFileType(gf)
	}
	g.logger.Debugf("UDR3: Submit: %d, depotFile: %s, rev %d, action %v, lbrFile %s, lbrRev %d, filetype %v",
		gf.commit.commit.Mark, gf.p4.depotFile, g.depotFileRevs[gf.p4.depotFile].rev,
		g.depotFileRevs[gf.p4.depotFile].action,
		g.depotFileRevs[gf.p4.depotFile].lbrFile,
		g.depotFileRevs[gf.p4.depotFile].lbrRev,
		gf.fileType)
}

// Maintain a list of latest revision counters indexed by depotFile/rev
func (g *GitP4Transfer) recordDepotFileType(gf *GitFile) {
	k := fmt.Sprintf("%s#%d", gf.p4.depotFile, gf.p4.rev)
	g.depotFileTypes[k] = gf.fileType
	if gf.isBranch && gf.action == rename {
		k := fmt.Sprintf("%s#%d", gf.p4.srcDepotFile, gf.p4.srcRev)
		g.depotFileTypes[k] = gf.fileType
	}
}

// Retrieve required filetype
func (g *GitP4Transfer) getDepotFileTypes(depotFile string, rev int) journal.FileType {
	k := fmt.Sprintf("%s#%d", depotFile, rev)
	if _, ok := g.depotFileTypes[k]; !ok {
		g.logger.Errorf("Failed to find filetype: %s#%d", depotFile, rev)
		return 0
	}
	return g.depotFileTypes[k]
}

func (g *GitP4Transfer) depotFileTypeExists(depotFile string, rev int) bool {
	k := fmt.Sprintf("%s#%d", depotFile, rev)
	_, ok := g.depotFileTypes[k]
	return ok
}

func (g *GitP4Transfer) setBranch(currCommit *GitCommit) {
	// Sets the branch for the current commit, using its parent if not otherwise specified
	if currCommit == nil {
		return
	}
	if currCommit.commit.From != "" {
		if intVar, err := strconv.Atoi(currCommit.commit.From[1:]); err == nil {
			parent := g.commits[intVar]
			if currCommit.branch == "" {
				currCommit.branch = parent.branch
			}
			if currCommit.branch != parent.branch {
				currCommit.prevBranch = parent.branch
			}
			currCommit.parentBranch = parent.parentBranch
			if currCommit.parentBranch == "" {
				currCommit.parentBranch = parent.branch
			}
		}
	} else {
		currCommit.branch = g.opts.config.DefaultBranch
	}
	if len(currCommit.commit.Merge) >= 1 {
		firstMerge := currCommit.commit.Merge[0]
		if intVar, err := strconv.Atoi(firstMerge[1:]); err == nil {
			mergeFrom := g.commits[intVar]
			if mergeFrom.branch != "" && mergeFrom.branch != currCommit.branch {
				currCommit.mergeBranch = mergeFrom.branch
			} else {
				g.logger.Errorf("Merge Commit mark %d has no branch", intVar)
			}
		}
	}
}

func (g *GitP4Transfer) createGraphEdges(cmt *GitCommit) {
	// Sets the branch for the current commit, using its parent if not otherwise specified
	if cmt == nil {
		return
	}
	if cmt.commit.From != "" {
		if intVar, err := strconv.Atoi(cmt.commit.From[1:]); err == nil {
			parent := g.commits[intVar]
			g.graph.Edge(parent.gNode, cmt.gNode, "p")
		}
	}
	if len(cmt.commit.Merge) < 1 {
		return
	}
	for _, merge := range cmt.commit.Merge {
		if intVar, err := strconv.Atoi(merge[1:]); err == nil {
			mergeFrom := g.commits[intVar]
			g.graph.Edge(mergeFrom.gNode, cmt.gNode, "m")
		}
	}
}

// Validate that all collected GitFiles make sense - remove any that don't!
// Note that we need to process things in order - later actions can override earlier ones
func (g *GitP4Transfer) validateCommit(cmt *GitCommit) {
	if cmt == nil {
		return
	}
	g.setBranch(cmt)
	g.logger.Debugf("CommitSummary: Mark:%d Files:%d Size:%d/%s",
		cmt.commit.Mark, len(cmt.files), cmt.commitSize, Humanize(cmt.commitSize))
	// If a new branch, then copy files from parent
	if _, ok := g.filesOnBranch[cmt.parentBranch]; !ok {
		g.filesOnBranch[cmt.parentBranch] = &node.Node{Name: ""}
	}
	if _, ok := g.filesOnBranch[cmt.branch]; !ok {
		g.filesOnBranch[cmt.branch] = &node.Node{Name: ""}
		pfiles := g.filesOnBranch[cmt.parentBranch].GetFiles("")
		for _, f := range pfiles {
			g.filesOnBranch[cmt.branch].AddFile(f)
		}
	}
	newfiles := make([]*GitFile, 0)
	node := g.filesOnBranch[cmt.branch]
	// Phase 1 - expand deletes/renames/copies of directories to individual commands
	//    May ignore files, eg. delete of a file which doesn't exist (or an empty dir)
	for i := range cmt.files {
		gf := cmt.files[i]
		if gf.actionInvalid {
			g.logger.Debugf("IgnoringInvalidAction: Path: %s %s", gf.name, gf.action)
			continue
		}
		if gf.action == modify {
			newfiles = append(newfiles, gf)
		} else if gf.action == delete {
			if node.FindFile(gf.name) { // Single file found
				newfiles = append(newfiles, gf)
				continue
			}
			filesDeleted := 0
			files := node.GetFiles(gf.name)
			if len(files) > 0 {
				g.logger.Debugf("DirDelete: Path:%s", gf.name)
				for _, df := range files {
					if !hasPrefix(df, gf.name) {
						g.logger.Errorf("Unexpected path found: %s: %s", gf.name, df)
						continue
					}
					g.logger.Debugf("DirFileDelete: %s Path:%s", cmt.ref(), df)
					filesDeleted += 1
					newfiles = append(newfiles, newGitFile(&GitFile{name: df, action: delete, logger: g.logger}))
				}
			}
			// Now we search to see if we are deleting the targets of any renames (previously) in current commit
			deleteLogged := false
			for _, dupGf := range newfiles {
				if dupGf.action == rename && hasPrefix(dupGf.name, string(gf.name)) {
					if !deleteLogged && len(files) == 0 {
						g.logger.Debugf("DirDelete: Path:%s", gf.name)
					}
					g.logger.Debugf("DeleteOverridesRename: %s Src: %s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
					dupGf.actionInvalid = true
					filesDeleted += 1
				}
			}
			if filesDeleted == 0 {
				g.logger.Debugf("DeleteIgnored: %s Path:%s", cmt.ref(), gf.name)
				g.blobFileMatcher.removeGitFile(gf)
			}
		} else if gf.action == rename {
			if node.FindFile(gf.srcName) { // Single file rename
				newfiles = append(newfiles, gf)
				continue
			}
			files := node.GetFiles(gf.srcName)
			if len(files) > 0 { // Turn dir rename into multiple single file renames
				g.logger.Debugf("DirRename: Src:%s Dst:%s", gf.srcName, gf.name)
				// First we look for files in current commit - because a single file rename can be followed by a dir rename which overrides it
				// src/A -> src/B followed by src -> targ, means turn it into src/A -> targ/B
				srcDoubles := make([]string, 0)
				for _, dupGf := range newfiles {
					if dupGf.action == rename && hasPrefix(dupGf.name, string(gf.srcName)) {
						dest := fmt.Sprintf("%s%s", gf.name, dupGf.name[len(gf.srcName):])
						dupGf.name = dest // Don't append gf to newfiles because we adjust dupGF to be the correct rename
						g.logger.Debugf("RenameOverride: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						srcDoubles = append(srcDoubles, dupGf.srcName)
					}
				}
				for _, rf := range files {
					if !hasPrefix(rf, string(gf.srcName)) {
						g.logger.Errorf("Unexpected src found: %s: %s", string(gf.srcName), rf)
						continue
					}
					dest := fmt.Sprintf("%s%s", gf.name, rf[len(gf.srcName):])
					foundDouble := false
					for _, double := range srcDoubles {
						if double == rf {
							foundDouble = true
							break
						}
					}
					if foundDouble {
						g.logger.Debugf("DirFileRenameIgnoredAsDouble: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
					} else {
						g.logger.Debugf("DirFileRename: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
						newfiles = append(newfiles, newGitFile(&GitFile{name: dest, srcName: rf, action: rename, logger: g.logger}))
					}
				}
			} else {
				// Handle the rare case where a directory rename is followed by individual file renames (so a double rename!)
				doubleRename := false
				var dupGf *GitFile
				for _, dupGf = range newfiles {
					if dupGf.name == gf.srcName {
						if dupGf.srcName == dupGf.name {
							g.logger.Debugf("DoubleRenameIgnored: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						} else {
							doubleRename = true
							dupGf.name = gf.name // Don't append gf to newfiles because we adjust dupGF to be the correct rename
							g.logger.Debugf("DoubleRename: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						}
						break
					}
				}
				if !doubleRename {
					g.logger.Debugf("RenameIgnored: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
					g.blobFileMatcher.removeGitFile(gf)
				}
			}
		} else if gf.action == copy {
			if node.FindFile(gf.name) {
				newfiles = append(newfiles, gf)
				continue
			}
			files := node.GetFiles(gf.name)
			if len(files) > 0 {
				g.logger.Debugf("DirCopy: Src:%s Dst:%s", gf.srcName, gf.name)
				for _, rf := range files {
					if !hasPrefix(rf, string(gf.srcName)) {
						g.logger.Errorf("Unexpected src found: %s: %s", string(gf.srcName), rf)
						continue
					}
					dest := fmt.Sprintf("%s%s", gf.name, rf[len(gf.srcName):])
					g.logger.Debugf("DirFileCopy: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
					newfiles = append(newfiles, newGitFile(&GitFile{name: dest, srcName: rf, action: copy, logger: g.logger}))
				}
			} else {
				g.logger.Debugf("CopyIgnored: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
			}
		} else {
			g.logger.Errorf("Unexpected GFAction: GitFile: %s ID %d, %s, %s", cmt.ref(), gf.ID, gf.name, gf.action.String())
		}
	}
	// Phase 2 - remove actions which do not make sense, e.g. delete of a renamed file or rename/copy of a deleted file
	cmt.files = newfiles
	newfiles = make([]*GitFile, 0)
	for i := range cmt.files {
		valid := true
		gf := cmt.files[i]
		if gf.action == modify {
			valid = true
		} else if gf.action == delete {
			dupGF := cmt.findGitFileRename(string(gf.name))
			if dupGF != nil && dupGF.action == rename {
				g.logger.Warnf("DeleteOfRenamedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
			if !g.filesOnBranch[cmt.branch].FindFile(gf.name) {
				g.logger.Warnf("DeleteOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
		} else if gf.action == rename {
			// Search for all possible duplicates - there may be more than one!
			for _, dupGF := range cmt.files {
				if dupGF.name == gf.srcName && dupGF.ID != gf.ID {
					if !g.filesOnBranch[cmt.branch].FindFile(gf.srcName) {
						g.logger.Warnf("RenameOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
						valid = false
						break
					} else if dupGF.action == modify {
						// Look for case where there is a modify for the source of a file being renamed:
						//   - if so mark this rename as a pseudo one - so that delete of source won't happen
						valid = true
						gf.isPseudoRename = true
						g.logger.Warnf("PseudoRename - RenameOfModifiedFile: GitFile: %s ID %d, %s", cmt.ref(), gf.ID, gf.name)
					} else if dupGF.action == rename {
						g.logger.Warnf("DoubleRename: GitFile: %s ID %d, ID2 %d, %s", cmt.ref(), gf.ID, dupGF.ID, gf.name)
					}
				}
				if dupGF.srcName == gf.srcName && dupGF.ID != gf.ID && dupGF.action == rename && !dupGF.isDoubleRename {
					gf.isDoubleRename = true
					g.logger.Warnf("DoubleRename2: GitFile: %s ID %d, ID2 %d, %s", cmt.ref(), gf.ID, dupGF.ID, gf.name)
				}
			}
			// Look for double rename - a->b and b->c
		} else if gf.action == copy {
			dupGF := cmt.findGitFile(string(gf.srcName))
			if dupGF != nil && dupGF.action == delete {
				g.logger.Warnf("CopyOfDeletedFile ignored: GitFile: %s ID %d, %s -> %s", cmt.ref(), dupGF.ID, gf.srcName, gf.name)
				valid = false
			}
			if !g.filesOnBranch[cmt.branch].FindFile(gf.srcName) {
				g.logger.Warnf("CopyOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
		}
		if valid && !gf.actionInvalid {
			newfiles = append(newfiles, gf)
		}
	}
	// Phase 3 - update our list of files for validation of future commits
	cmt.files = newfiles
	for i := range cmt.files {
		gf := cmt.files[i]
		if gf.action == modify || gf.action == copy {
			node.AddFile(gf.name)
		} else if gf.action == delete {
			node.DeleteFile(gf.name)
		} else if gf.action == rename {
			node.AddFile(gf.name)
			node.DeleteFile(gf.srcName)
		}
	}
	for i := range cmt.files {
		cmt.files[i].setDepotPaths(g.opts, g.branchNameMapper, &g.depotFileRevs, cmt)
		cmt.files[i].updateFileDetails()
	}
}

// Validate that all collected GitFiles make sense - remove any that don't!
// Note that we need to process things in order - later actions can override earlier ones
func (g *GitP4Transfer) ValidateCommit(cmt *GitCommit) {
	if cmt == nil {
		return
	}
	g.setBranch(cmt)
	g.logger.Debugf("CommitSummary: Mark:%d Files:%d Size:%d/%s",
		cmt.commit.Mark, len(cmt.files), cmt.commitSize, Humanize(cmt.commitSize))
	// If a new branch, then copy files from parent
	if _, ok := g.filesOnBranch[cmt.parentBranch]; !ok {
		g.filesOnBranch[cmt.parentBranch] = &node.Node{Name: ""}
	}
	if _, ok := g.filesOnBranch[cmt.branch]; !ok {
		g.filesOnBranch[cmt.branch] = &node.Node{Name: ""}
		pfiles := g.filesOnBranch[cmt.parentBranch].GetFiles("")
		for _, f := range pfiles {
			g.filesOnBranch[cmt.branch].AddFile(f)
		}
	}
	newfiles := make([]*GitFile, 0)
	node := g.filesOnBranch[cmt.branch]
	// Phase 1 - expand deletes/renames/copies of directories to individual commands
	//    May ignore files, eg. delete of a file which doesn't exist (or an empty dir)
	for i := range cmt.files {
		gf := cmt.files[i]
		if gf.actionInvalid {
			g.logger.Debugf("IgnoringInvalidAction: Path: %s %s", gf.name, gf.action)
			continue
		}
		if gf.action == modify {
			newfiles = append(newfiles, gf)
		} else if gf.action == delete {
			if node.FindFile(gf.name) { // Single file found
				newfiles = append(newfiles, gf)
				continue
			}
			filesDeleted := 0
			files := node.GetFiles(gf.name)
			if len(files) > 0 {
				g.logger.Debugf("DirDelete: Path:%s", gf.name)
				for _, df := range files {
					if !hasPrefix(df, gf.name) {
						g.logger.Errorf("Unexpected path found: %s: %s", gf.name, df)
						continue
					}
					g.logger.Debugf("DirFileDelete: %s Path:%s", cmt.ref(), df)
					filesDeleted += 1
					newfiles = append(newfiles, newGitFile(&GitFile{name: df, action: delete, logger: g.logger}))
				}
			}
			// Now we search to see if we are deleting the targets of any renames (previously) in current commit
			deleteLogged := false
			for _, dupGf := range newfiles {
				if dupGf.action == rename && hasPrefix(dupGf.name, string(gf.name)) {
					if !deleteLogged && len(files) == 0 {
						g.logger.Debugf("DirDelete: Path:%s", gf.name)
					}
					g.logger.Debugf("DeleteOverridesRename: %s Src: %s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
					dupGf.actionInvalid = true
					filesDeleted += 1
				}
			}
			if filesDeleted == 0 {
				g.logger.Debugf("DeleteIgnored: %s Path:%s", cmt.ref(), gf.name)
				g.blobFileMatcher.removeGitFile(gf)
			}
		} else if gf.action == rename {
			if node.FindFile(gf.srcName) { // Single file rename
				newfiles = append(newfiles, gf)
				continue
			}
			files := node.GetFiles(gf.srcName)
			if len(files) > 0 { // Turn dir rename into multiple single file renames
				g.logger.Debugf("DirRename: Src:%s Dst:%s", gf.srcName, gf.name)
				// First we look for files in current commit - because a single file rename can be followed by a dir rename which overrides it
				// src/A -> src/B followed by src -> targ, means turn it into src/A -> targ/B
				srcDoubles := make([]string, 0)
				for _, dupGf := range newfiles {
					if dupGf.action == rename && hasPrefix(dupGf.name, string(gf.srcName)) {
						dest := fmt.Sprintf("%s%s", gf.name, dupGf.name[len(gf.srcName):])
						dupGf.name = dest // Don't append gf to newfiles because we adjust dupGF to be the correct rename
						g.logger.Debugf("RenameOverride: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						srcDoubles = append(srcDoubles, dupGf.srcName)
					}
				}
				for _, rf := range files {
					if !hasPrefix(rf, string(gf.srcName)) {
						g.logger.Errorf("Unexpected src found: %s: %s", string(gf.srcName), rf)
						continue
					}
					dest := fmt.Sprintf("%s%s", gf.name, rf[len(gf.srcName):])
					foundDouble := false
					for _, double := range srcDoubles {
						if double == rf {
							foundDouble = true
							break
						}
					}
					if foundDouble {
						g.logger.Debugf("DirFileRenameIgnoredAsDouble: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
					} else {
						g.logger.Debugf("DirFileRename: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
						newfiles = append(newfiles, newGitFile(&GitFile{name: dest, srcName: rf, action: rename, logger: g.logger}))
					}
				}
			} else {
				// Handle the rare case where a directory rename is followed by individual file renames (so a double rename!)
				doubleRename := false
				var dupGf *GitFile
				for _, dupGf = range newfiles {
					if dupGf.name == gf.srcName {
						if dupGf.srcName == dupGf.name {
							g.logger.Debugf("DoubleRenameIgnored: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						} else {
							doubleRename = true
							dupGf.name = gf.name // Don't append gf to newfiles because we adjust dupGF to be the correct rename
							g.logger.Debugf("DoubleRename: %s Src:%s Dst:%s", cmt.ref(), dupGf.srcName, dupGf.name)
						}
						break
					}
				}
				if !doubleRename {
					g.logger.Debugf("RenameIgnored: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
					g.blobFileMatcher.removeGitFile(gf)
				}
			}
		} else if gf.action == copy {
			if node.FindFile(gf.name) {
				newfiles = append(newfiles, gf)
				continue
			}
			files := node.GetFiles(gf.name)
			if len(files) > 0 {
				g.logger.Debugf("DirCopy: Src:%s Dst:%s", gf.srcName, gf.name)
				for _, rf := range files {
					if !hasPrefix(rf, string(gf.srcName)) {
						g.logger.Errorf("Unexpected src found: %s: %s", string(gf.srcName), rf)
						continue
					}
					dest := fmt.Sprintf("%s%s", gf.name, rf[len(gf.srcName):])
					g.logger.Debugf("DirFileCopy: %s Src:%s Dst:%s", cmt.ref(), rf, dest)
					newfiles = append(newfiles, newGitFile(&GitFile{name: dest, srcName: rf, action: copy, logger: g.logger}))
				}
			} else {
				g.logger.Debugf("CopyIgnored: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
			}
		} else {
			g.logger.Errorf("Unexpected GFAction: GitFile: %s ID %d, %s, %s", cmt.ref(), gf.ID, gf.name, gf.action.String())
		}
	}
	// Phase 2 - remove actions which do not make sense, e.g. delete of a renamed file or rename/copy of a deleted file
	cmt.files = newfiles
	newfiles = make([]*GitFile, 0)
	for i := range cmt.files {
		valid := true
		gf := cmt.files[i]
		if gf.action == modify {
			valid = true
		} else if gf.action == delete {
			dupGF := cmt.findGitFileRename(string(gf.name))
			if dupGF != nil && dupGF.action == rename {
				g.logger.Warnf("DeleteOfRenamedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
			if !g.filesOnBranch[cmt.branch].FindFile(gf.name) {
				g.logger.Warnf("DeleteOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
		} else if gf.action == rename {
			// Search for all possible duplicates - there may be more than one!
			for _, dupGF := range cmt.files {
				if dupGF.name == gf.srcName && dupGF.ID != gf.ID {
					if !g.filesOnBranch[cmt.branch].FindFile(gf.srcName) {
						g.logger.Warnf("RenameOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
						valid = false
						break
					} else if dupGF.action == modify {
						// Look for case where there is a modify for the source of a file being renamed:
						//   - if so mark this rename as a pseudo one - so that delete of source won't happen
						valid = true
						gf.isPseudoRename = true
						g.logger.Warnf("PseudoRename - RenameOfModifiedFile: GitFile: %s ID %d, %s", cmt.ref(), gf.ID, gf.name)
					} else if dupGF.action == rename {
						g.logger.Warnf("DoubleRename: GitFile: %s ID %d, ID2 %d, %s", cmt.ref(), gf.ID, dupGF.ID, gf.name)
					}
				}
				if dupGF.srcName == gf.srcName && dupGF.ID != gf.ID && dupGF.action == rename && !dupGF.isDoubleRename {
					gf.isDoubleRename = true
					g.logger.Warnf("DoubleRename2: GitFile: %s ID %d, ID2 %d, %s", cmt.ref(), gf.ID, dupGF.ID, gf.name)
				}
			}
			// Look for double rename - a->b and b->c
		} else if gf.action == copy {
			dupGF := cmt.findGitFile(string(gf.srcName))
			if dupGF != nil && dupGF.action == delete {
				g.logger.Warnf("CopyOfDeletedFile ignored: GitFile: %s ID %d, %s -> %s", cmt.ref(), dupGF.ID, gf.srcName, gf.name)
				valid = false
			}
			if !g.filesOnBranch[cmt.branch].FindFile(gf.srcName) {
				g.logger.Warnf("CopyOfDeletedFile ignored: GitFile: %s ID %d, %s", cmt.ref(), dupGF.ID, gf.name)
				valid = false
			}
		}
		if valid && !gf.actionInvalid {
			newfiles = append(newfiles, gf)
		}
	}
	// Phase 3 - update our list of files for validation of future commits
	cmt.files = newfiles
	for i := range cmt.files {
		gf := cmt.files[i]
		if gf.action == modify || gf.action == copy {
			node.AddFile(gf.name)
		} else if gf.action == delete {
			node.DeleteFile(gf.name)
		} else if gf.action == rename {
			node.AddFile(gf.name)
			node.DeleteFile(gf.srcName)
		}
	}
}

// Process a previously validated commit
func (g *GitP4Transfer) processCommit(cmt *GitCommit) {
	if cmt == nil {
		return
	}
	for i := range cmt.files {
		g.updateDepotRevs(g.opts, cmt.files[i], cmt.commit.Mark)
	}
	if g.graph != nil { // Optional Graphviz structure to be output
		cmt.gNode = g.graph.Node(fmt.Sprintf("Commit: %d %s", cmt.commit.Mark, cmt.branch))
		g.createGraphEdges(cmt)
	}
	g.gitChan <- *cmt
}

// GitParse - returns channel which contains commits with associated files.
func (g *GitP4Transfer) GitParse(pool *pond.WorkerPool) chan GitCommit {
	var buf io.Reader
	var file *os.File
	var err error

	if g.testInput != "" {
		buf = strings.NewReader(g.testInput)
	} else {
		file, err = os.Open(g.opts.gitImportFile) // Note deferred close in go routine below.
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", g.opts.gitImportFile, err)
			os.Exit(1)
		}
		buf = bufio.NewReader(file)
	}

	g.gitChan = make(chan GitCommit, 50)
	var currCommit *GitCommit
	var commitSize = 0
	commitCount := 0

	// Create an unbuffered (blocking) pool with a fixed
	// number of workers
	weCreatedPool := false
	if pool == nil {
		weCreatedPool = true
		pondSize := runtime.NumCPU()
		pool = pond.New(pondSize, 0, pond.MinWorkers(10))
	}

	if g.opts.graphFile != "" { // Optional Graphviz structure to be output
		g.graph = dot.NewGraph(dot.Directed)
	}

	f := libfastimport.NewFrontend(buf, nil, nil)
	go func() {
		defer file.Close()
		defer close(g.gitChan)
		if weCreatedPool {
			defer pool.StopAndWait()
		}
	CmdLoop:
		for {
			cmd, err := f.ReadCmd()
			if err != nil {
				if err == io.EOF {
					break
				}
				g.logger.Errorf("ERROR: Failed to read cmd: %v", err)
				continue
			}
			switch ctype := cmd.(type) {
			case libfastimport.CmdBlob:
				blob := cmd.(libfastimport.CmdBlob)
				g.logger.Debugf("Blob: Mark:%d OriginalOID:%s Size:%s", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
				b := newGitBlob(&blob)
				g.blobFileMatcher.addBlob(b)
				commitSize += len(blob.Data)
				b.SaveBlob(pool, g.opts.archiveRoot, g.opts.dummyArchives, g.blobFileMatcher)

			case libfastimport.CmdReset:
				reset := cmd.(libfastimport.CmdReset)
				g.logger.Debugf("Reset: - %+v", reset)
				reset.RefName = strings.ReplaceAll(reset.RefName, " ", "_") // For consistency with Commits even though unused

			case libfastimport.CmdCommit:
				g.validateCommit(currCommit)
				g.processCommit(currCommit)
				commit := cmd.(libfastimport.CmdCommit)
				g.logger.Debugf("Commit: %+v", commit)
				commit.Ref = strings.ReplaceAll(commit.Ref, " ", "_")
				currCommit = newGitCommit(&commit, commitSize)
				commitSize = 0
				g.commits[commit.Mark] = currCommit
				if g.opts.debugCommit != 0 && g.opts.debugCommit == commit.Mark {
					g.logger.Debugf("Commit breakpoint: %d", commit.Mark)
				}

			case libfastimport.CmdCommitEnd:
				commit := cmd.(libfastimport.CmdCommitEnd)
				g.logger.Debugf("CommitEnd: %+v", commit)
				commitCount += 1
				if g.opts.maxCommits > 0 && commitCount >= g.opts.maxCommits {
					g.logger.Infof("Processed %d commits", commitCount)
					break CmdLoop
				}

			case libfastimport.FileModify:
				f := cmd.(libfastimport.FileModify)
				g.logger.Debugf("FileModify: %s %+v", currCommit.ref(), f)
				oid, err := getOID(f.DataRef)
				if err != nil {
					g.logger.Errorf("Failed to get oid: %+v", f)
				}
				var gf *GitFile
				b := g.blobFileMatcher.getBlob(oid)
				if b != nil {
					if len(b.gitFileIDs) == 0 { // First ref to this blob
						gf = newGitFile(&GitFile{name: string(f.Path), action: modify,
							blob: b, fileType: b.fileType, compressed: b.compressed,
							logger: g.logger})
					} else { // Duplicate ref
						gf = newGitFile(&GitFile{name: string(f.Path), action: modify,
							blob: b, fileType: b.fileType, compressed: b.compressed,
							duplicateArchive: true, logger: g.logger})
					}
				} else {
					g.logger.Errorf("Failed to find blob: %d", oid)
				}
				// Search for renames (or deletes) of same file in current commit and note if found.
				dupGF := currCommit.findGitFile(gf.name)
				if dupGF != nil {
					if dupGF.action == rename {
						dupGF.isDirtyRename = true
						dupGF.blob = gf.blob
						dupGF.compressed = gf.compressed
						dupGF.duplicateArchive = gf.duplicateArchive
						dupGF.fileType = gf.fileType
						g.blobFileMatcher.addGitFile(dupGF)
						g.logger.Debugf("DirtyRenameFound: %s %s, GitFile: ID %d, %s, blobID %d, filetype: %s",
							currCommit.ref(), dupGF.name, dupGF.ID, dupGF.name, dupGF.blob.blob.Mark, dupGF.blob.fileType)
					} else if dupGF.action == delete {
						// Having a modify with a delete doesn't make sense - we discard the delete!
						g.logger.Warnf("ModifyOfDeletedFile: %s GitFile: ID %d, %s, blobID %d, filetype: %s",
							currCommit.ref(), gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
						dupGF.actionInvalid = true
						g.blobFileMatcher.addGitFile(gf)
						g.logger.Debugf("GitFile: ID %d, %s, blobID %d, filetype: %s", gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
						currCommit.files = append(currCommit.files, gf)
					}
				} else {
					g.blobFileMatcher.addGitFile(gf)
					g.logger.Debugf("GitFile: %s ID %d, %s, blobID %d, filetype: %s",
						currCommit.ref(), gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
					currCommit.files = append(currCommit.files, gf)
				}

			case libfastimport.FileDelete:
				f := cmd.(libfastimport.FileDelete)
				g.logger.Debugf("FileDelete: %s Path:%s", currCommit.ref(), f.Path)
				gf := newGitFile(&GitFile{name: string(f.Path), action: delete, logger: g.logger})
				currCommit.files = append(currCommit.files, gf)

			case libfastimport.FileCopy:
				f := cmd.(libfastimport.FileCopy)
				g.logger.Debugf("FileCopy: %s Src:%s Dst:%s", currCommit.ref(), f.Src, f.Dst)
				gf := newGitFile(&GitFile{name: string(f.Dst), srcName: string(f.Src), action: copy, logger: g.logger})
				currCommit.files = append(currCommit.files, gf)

			case libfastimport.FileRename:
				f := cmd.(libfastimport.FileRename)
				g.logger.Debugf("FileRename: %s Src:%s Dst:%s", currCommit.ref(), f.Src, f.Dst)
				gf := newGitFile(&GitFile{name: string(f.Dst), srcName: string(f.Src), action: rename, logger: g.logger})
				currCommit.files = append(currCommit.files, gf)

			case libfastimport.CmdTag:
				t := cmd.(libfastimport.CmdTag)
				g.logger.Debugf("CmdTag: %+v", t)
				t.RefName = strings.ReplaceAll(t.RefName, " ", "_")

			default:
				g.logger.Errorf("Not handled: Found ctype %v cmd %+v", ctype, cmd)
				g.logger.Errorf("Cmd type %T", cmd)
			}
		}
		g.validateCommit(currCommit)
		g.processCommit(currCommit)
		// Render/close Graphviz if required
		if g.opts.graphFile == "" {
			return
		}
		f, err := os.OpenFile(g.opts.graphFile, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			g.logger.Error(err)
		}
		defer f.Close()

		f.Write([]byte(g.graph.String()))
	}()

	return g.gitChan
}

func main() {
	// Tracing code
	// ft, err := os.Create("trace.out")
	// if err != nil {
	// 	panic(err)
	// }
	// defer ft.Close()
	// err = trace.Start(ft)
	// if err != nil {
	// 	panic(err)
	// }
	// defer trace.Stop()
	// End of trace code
	// var err error

	// Turn on profiling
	// defer profile.Start(profile.MemProfile).Stop()
	// go func() {
	// 	http.ListenAndServe(":8080", nil)
	// }()

	var (
		configFile = kingpin.Flag(
			"config",
			"Config file for gitp4transfer - allows for branch renaming etc.",
		).Default("gitp4transfer.yaml").Short('c').String()
		gitimport = kingpin.Arg(
			"gitimport",
			"Git fast-export file to process.",
		).String()
		importDepot = kingpin.Flag(
			"import.depot",
			"Depot into which to import (overrides config).",
		).Default(config.DefaultDepot).Short('d').String()
		importPath = kingpin.Flag(
			"import.path",
			"(Optional) path component under import.depot (overrides config).",
		).String()
		defaultBranch = kingpin.Flag(
			"default.branch",
			"Name of default git branch (overrides config).",
		).Default(config.DefaultBranch).Short('b').String()
		caseInsensitive = kingpin.Flag(
			"case.insensitive",
			"Create checkpoint case-insensitive mode (for Linux) and lowercase archive files. If not set, then OS default applies.",
		).Bool()
		dummyArchives = kingpin.Flag(
			"dummy",
			"Create dummy (small) archive files - for quick analysis of large repos.",
		).Bool()
		dump = kingpin.Flag(
			"dump",
			"Dump git file, saving the contained archive contents.",
		).Bool()
		dumpArchives = kingpin.Flag(
			"dump.archives",
			"Saving the contained archive contents if --dump is specified.",
		).Short('a').Bool()
		maxCommits = kingpin.Flag(
			"max.commits",
			"Max no of commits to process (default 0 means all).",
		).Default("0").Short('m').Int()
		dryrun = kingpin.Flag(
			"dryrun",
			"Don't actually create archive files.",
		).Bool()
		archive = kingpin.Flag(
			"archive.root",
			"Archive root dir under which to store extracted archives.",
		).String()
		outputGraph = kingpin.Flag(
			"graphfile",
			"Graphviz dot file to output git commit/file structure to.",
		).String()
		outputJournal = kingpin.Flag(
			"journal",
			"P4D journal file to write (assuming --dump not specified).",
		).Default("jnl.0").String()
		debug = kingpin.Flag(
			"debug",
			"Enable debugging level.",
		).Default("0").Int()
		parallelThreads = kingpin.Flag(
			"parallel.threads",
			"How many parallel threads to use (default 0 means no of CPUs).",
		).Default("0").Int()
		debugCommit = kingpin.Flag(
			"debug.commit",
			"For debugging - to allow breakpoints to be set - only valid if debug > 0.",
		).Default("0").Int()
	)
	kingpin.UsageTemplate(kingpin.CompactUsageTemplate).Version(version.Print("gitp4transfer")).Author("Robert Cowham")
	kingpin.CommandLine.Help = "Parses one or more git fast-export files to create a Perforce Helix Core import\n"
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *debug > 0 {
		logger.Level = logrus.DebugLevel
	}
	cfg, err := config.LoadConfigFile(*configFile)
	if err != nil {
		logger.Errorf("error loading config file: %v", err)
		os.Exit(-1)
	}
	if *defaultBranch != config.DefaultBranch {
		cfg.DefaultBranch = *defaultBranch
	}
	if *importDepot != config.DefaultDepot {
		cfg.ImportDepot = *importDepot
	}
	if *importPath != "" {
		cfg.ImportPath = *importPath
	}
	startTime := time.Now()
	logger.Infof("%v", version.Print("gitp4transfer"))
	logger.Infof("Starting %s, gitimport: %v", startTime, *gitimport)

	opts := &GitParserOptions{
		config:          cfg,
		gitImportFile:   *gitimport,
		archiveRoot:     *archive,
		dryRun:          *dryrun,
		dummyArchives:   *dummyArchives,
		caseInsensitive: *caseInsensitive,
		maxCommits:      *maxCommits,
		graphFile:       *outputGraph,
		debugCommit:     *debugCommit,
	}
	logger.Infof("Options: %+v", opts)
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Errorf("error loading config: %v", err)
		os.Exit(-1)
	}
	if *dump {
		g.DumpGit(*dumpArchives)
		return
	}

	var pool *pond.WorkerPool
	pondSize := runtime.NumCPU()
	if *parallelThreads == 0 {
		pool = pond.New(pondSize, 0, pond.MinWorkers(10))
	} else {
		pondSize = *parallelThreads
		if pondSize > runtime.NumCPU() {
			pondSize = runtime.NumCPU()
		}
		pool = pond.New(pondSize, pondSize)
	}
	logger.Infof("Parallel threads: %d", pondSize)

	commitChan := g.GitParse(pool)

	j := journal.Journal{}
	f, err := os.Create(*outputJournal)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	j.SetWriter(f)
	j.WriteHeader(opts.config.ImportDepot, opts.caseInsensitive)

	for c := range commitChan {
		j.WriteChange(c.commit.Mark, c.user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			if !*dryrun {
				f.CreateArchiveFile(pool, opts.caseInsensitive, opts.archiveRoot, g.blobFileMatcher, c.commit.Mark)
			} else if f.blob != nil && f.blob.hasData && !f.blob.dataRemoved {
				f.blob.blob.Data = "" // Allow contents to be GC'ed
				f.blob.dataRemoved = true
			}
			f.WriteJournal(&j, &c)
		}
	}

	defer pool.StopAndWait()
	// close(filesChan)

}
