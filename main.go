package main

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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alitto/pond"
	"github.com/emicklei/dot"
	"github.com/h2non/filetype"
	journal "github.com/rcowham/gitp4transfer/journal"
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
	gitImportFile string
	archiveRoot   string
	dryRun        bool
	dummyArchives bool
	importDepot   string
	importPath    string // After depot and branch
	defaultBranch string
	graphFile     string
	maxCommits    int
}

type GitAction int

const (
	unknown GitAction = iota
	modify
	delete
	copy
	rename
	renamed // Source of rename so deleted
)

func (a GitAction) String() string {
	return [...]string{"Unknown", "Modify", "Delete", "Copy", "Rename"}[a]
}

// Node - tree structure to record directory contents for directory renames, deletes and copies
type Node struct {
	name     string
	path     string
	isFile   bool
	children []*Node
}

func (n *Node) addSubFile(fullPath string, subPath string) {
	parts := strings.Split(subPath, "/")
	if len(parts) == 1 {
		for _, c := range n.children {
			if c.name == parts[0] {
				return // file already registered
			}
		}
		n.children = append(n.children, &Node{name: parts[0], isFile: true, path: fullPath})
	} else {
		for _, c := range n.children {
			if c.name == parts[0] {
				c.addSubFile(fullPath, strings.Join(parts[1:], "/"))
				return
			}
		}
		n.children = append(n.children, &Node{name: parts[0]})
		n.children[len(n.children)-1].addSubFile(fullPath, strings.Join(parts[1:], "/"))
	}
}

func (n *Node) addFile(path string) {
	n.addSubFile(path, path)
}

func (n *Node) getChildFiles() []string {
	files := make([]string, 0)
	for _, c := range n.children {
		if c.isFile {
			files = append(files, c.path)
		} else {
			files = append(files, c.getChildFiles()...)
		}
	}
	return files
}

// Return a list of all files in a directory
func (n *Node) getFiles(dirName string) []string {
	parts := strings.Split(dirName, "/")
	files := make([]string, 0)
	if len(parts) == 1 {
		if n.name == parts[0] {
			files = append(files, n.getChildFiles()...)
			return files
		}
		for _, c := range n.children {
			if c.name == parts[0] {
				if c.isFile {
					files = append(files, c.path)
				} else {
					files = append(files, c.getChildFiles()...)
				}
			}
		}
		return files
	} else {
		for _, c := range n.children {
			if c.name == parts[0] {
				return c.getFiles(strings.Join(parts[1:], "/"))
			}
		}
	}
	return files
}

// Returns true if it finds a single file with specified name
func (n *Node) findFile(fileName string) bool {
	parts := strings.Split(fileName, "/")
	dir := ""
	if len(parts) > 1 {
		dir = strings.Join(parts[:len(parts)-1], "/")
	}
	files := n.getFiles(dir)
	for _, f := range files {
		if f == fileName {
			return true
		}
	}
	return false
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
	gf.blob.gitFileIDs = append(gf.blob.gitFileIDs, gf.ID)
}

func (m *BlobFileMatcher) getBlob(blobID int) *GitBlob {
	if b, ok := m.blobMap[blobID]; ok {
		return b
	} else {
		m.logger.Errorf("Failed to find blob: %d", blobID)
		return nil
	}
}

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
}

// GitFile - A git file record - modify/delete/copy/move
type GitFile struct {
	name             string // Git filename (target for rename/copy)
	ID               int
	size             int
	p4               *P4File
	srcName          string // Name of git source file for rename/copy
	duplicateArchive bool
	action           GitAction
	targ             string // For use with copy/move
	isBranch         bool
	isMerge          bool
	isDirtyRename    bool // Rename where content changed
	fileType         journal.FileType
	compressed       bool
	blob             *GitBlob
	logger           *logrus.Logger
	commit           *GitCommit
}

// Information for a P4File
type P4File struct {
	depotFile          string // Full depot path
	rev                int    // Depot rev
	lbrRev             int    // Lbr rev - usually same as Depot rev
	lbrFile            string // Lbr file - usually same as Depot file
	srcDepotFile       string //   "
	srcRev             int    //   "
	branchDepotFile    string // For branched files
	branchDepotRev     int
	branchSrcDepotFile string // The source file for a merged rename
	branchSrcDepotRev  int
	archiveFile        string
	p4action           journal.FileAction
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

func (gc *GitCommit) removeGitFile(ID int) {
	i := 0
	var gf *GitFile
	for i, gf = range gc.files {
		if gf.ID == ID {
			break
		}
	}
	if i >= len(gc.files) {
		return
	}
	gc.files = append(gc.files[:i], gc.files[i+1:]...)
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

// GitP4Transfer - Transfer via git fast-export file
type GitP4Transfer struct {
	logger          *logrus.Logger
	gitChan         chan GitCommit
	opts            GitParserOptions
	depotFileRevs   map[string]*RevChange       // Map depotFile to latest rev/chg
	depotFileTypes  map[string]journal.FileType // Map depotFile#rev to filetype (for renames/branching)
	blobFileMatcher *BlobFileMatcher            // Map between gitfile ID and record
	commits         map[int]*GitCommit
	testInput       string     // For testing only
	graph           *dot.Graph // If outputting a graph
}

func NewGitP4Transfer(logger *logrus.Logger) *GitP4Transfer {
	return &GitP4Transfer{logger: logger,
		depotFileRevs:   make(map[string]*RevChange),
		blobFileMatcher: newBlobFileMatcher(logger),
		depotFileTypes:  make(map[string]journal.FileType),
		commits:         make(map[int]*GitCommit)}
}

func (gf *GitFile) getDepotPath(opts GitParserOptions, branch string, name string) string {
	if len(opts.importPath) == 0 {
		return fmt.Sprintf("//%s/%s/%s", opts.importDepot, branch, name)
	} else {
		return fmt.Sprintf("//%s/%s/%s/%s", opts.importDepot, opts.importPath, branch, name)
	}
}

func (gf *GitFile) setDepotPaths(opts GitParserOptions, gc *GitCommit) {
	gf.commit = gc
	gf.p4.depotFile = gf.getDepotPath(opts, gc.branch, gf.name)
	if gf.srcName != "" {
		gf.p4.srcDepotFile = gf.getDepotPath(opts, gc.branch, gf.srcName)
	} else if gc.prevBranch != "" {
		gf.srcName = gf.name
		gf.isBranch = true
		gf.p4.srcDepotFile = gf.getDepotPath(opts, gc.prevBranch, gf.srcName)
	}
	if gc.mergeBranch != "" && gc.mergeBranch != gc.branch {
		gf.isMerge = true
		if gf.srcName == "" {
			gf.srcName = gf.name
			gf.p4.srcDepotFile = gf.getDepotPath(opts, gc.mergeBranch, gf.srcName)
		}
	}
}

// Sets compression option and binary/text
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
	b.setCompressionDetails()
	matcher.logger.Debugf("BlobFileType: %d: %s", b.blob.Mark, b.fileType)
	// Do the work in pool worker threads for concurrency, especially with compression
	rootDir := path.Join(archiveRoot, b.blobDirPath)
	if b.compressed {
		fname := path.Join(rootDir, fmt.Sprintf("%s.gz", b.blobFileName))
		matcher.logger.Debugf("SavingBlob: %s", fname)
		data := b.blob.Data
		if dummyFlag {
			data = fmt.Sprintf("%d", b.blob.Mark)
		}
		pool.Submit(func(rootDir string, fname string, data string) func() {
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
				_, err = zw.Write([]byte(data))
				if err != nil {
					panic(err)
				}
			}
		}(rootDir, fname, data))
	} else {
		fname := path.Join(rootDir, b.blobFileName)
		matcher.logger.Debugf("SavingBlob: %s", fname)
		data := b.blob.Data
		if dummyFlag {
			data = fmt.Sprintf("%d", b.blob.Mark)
		}
		pool.Submit(func(rootDir string, fname string, data string) func() {
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
				fmt.Fprint(f, data)
				if err != nil {
					panic(err)
				}
			}
		}(rootDir, fname, data))
	}
	b.saved = true
	b.blob.Data = "" // Allow contents to be GC'ed
	b.dataRemoved = true
	return nil
}

func (gf *GitFile) CreateArchiveFile(depotRoot string, matcher *BlobFileMatcher, changeNo int) {
	if gf.action == delete || (gf.action == rename && !gf.isDirtyRename) || !gf.blob.hasData {
		return
	}
	depotFile := strings.ReplaceAll(gf.p4.depotFile[2:], "@", "%40")
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
	if !gf.duplicateArchive {
		err = os.Rename(bname, fname)
		if err != nil {
			gf.logger.Errorf("Failed to Rename: %v", err)
		}
		return
	}
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
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, gf.p4.srcRev-1, gf.p4.srcRev, gf.p4.rev-1, gf.p4.rev, journal.BranchFrom, journal.DirtyBranchInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
		}
	} else if gf.action == delete {
		if gf.isMerge {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, gf.p4.srcRev-1, gf.p4.srcRev, gf.p4.rev-1, gf.p4.rev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, gf.p4.p4action, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
		}
	} else if gf.action == rename {
		if gf.isBranch { // Rename of a branched file - create integ records from parent
			j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteInteg(gf.p4.srcDepotFile, gf.p4.branchDepotFile, 0, gf.p4.srcRev, 0, gf.p4.branchDepotRev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
			j.WriteInteg(gf.p4.depotFile, gf.p4.branchDepotFile, 0, gf.p4.srcRev, 0, gf.p4.branchDepotRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
		} else if gf.isMerge { // Merging a rename
			j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteInteg(gf.p4.srcDepotFile, gf.p4.branchSrcDepotFile, 0, gf.p4.branchSrcDepotRev, 0, gf.p4.srcRev, journal.DeleteFrom, journal.DeleteInto, c.commit.Mark)
			j.WriteInteg(gf.p4.depotFile, gf.p4.branchDepotFile, 0, gf.p4.srcRev, 0, gf.p4.branchDepotRev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.p4.srcDepotFile, gf.p4.srcRev, journal.Delete, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			j.WriteRev(gf.p4.depotFile, gf.p4.rev, journal.Add, gf.fileType, chgNo, gf.p4.lbrFile, gf.p4.lbrRev, dt)
			// TODO - don't use 0 for startfromRev, startToRev
			j.WriteInteg(gf.p4.depotFile, gf.p4.srcDepotFile, 0, gf.p4.srcRev, 0, gf.p4.rev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
		}
	} else {
		gf.logger.Errorf("Unexpected action: %s", gf.action.String())
	}
}

// DumpGit - incrementally parse the git file, collecting stats and optionally saving archives as we go
// Useful for parsing very large git fast-export files without loading too much into memory!
func (g *GitP4Transfer) DumpGit(options GitParserOptions, saveFiles bool) {
	var buf io.Reader

	if g.testInput != "" {
		buf = strings.NewReader(g.testInput)
	} else {
		file, err := os.Open(options.gitImportFile)
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", options.gitImportFile, err)
			os.Exit(1)
		}
		defer file.Close()
		buf = bufio.NewReader(file)
	}
	g.opts = options

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
		switch cmd.(type) {
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
			g.logger.Errorf("Not handled - found cmd %+v", cmd)
			g.logger.Infof("Cmd type %T", cmd)
		}
	}
	for ext, size := range extSizes {
		g.logger.Infof("Ext %s: %s", ext, Humanize(size))
	}
}

// Is current head rev a deleted rev?
func (g *GitP4Transfer) isSrcDeletedFile(gf *GitFile) bool {
	if gf.p4.srcDepotFile == "" {
		return false
	}
	if f, ok := g.depotFileRevs[gf.p4.srcDepotFile]; ok {
		return f.action == delete
	}
	return false
}

// Is current head rev a deleted rev?
func (g *GitP4Transfer) isRenamedFile(gf *GitFile) bool {
	if gf.p4.depotFile == "" {
		return false
	}
	if f, ok := g.depotFileRevs[gf.p4.depotFile]; ok {
		return f.action == renamed
	}
	return false
}

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
		g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
		g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
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
		if gf.action == delete {
			// A delete without a source file just becomes delete
			g.logger.Warnf("Integ of delete becomes delete: '%s' '%s'", gf.p4.depotFile, gf.p4.srcDepotFile)
			gf.p4.srcDepotFile = ""
			gf.srcName = ""
			gf.isMerge = false
		} else if gf.action == rename {
			g.logger.Debugf("Rename of branched file: '%s' <- '%s'", gf.p4.depotFile, gf.p4.srcDepotFile)
			// Create a record for the source of the rename referring to its branched source
			depotPathOrig := gf.getDepotPath(opts, gf.commit.parentBranch, gf.srcName)
			if _, ok := g.depotFileRevs[depotPathOrig]; !ok {
				g.logger.Errorf("Failed to find original file: '%s'", depotPathOrig)
			} else {
				gf.isBranch = true
				lbrFileOrig := g.depotFileRevs[depotPathOrig].lbrFile
				lbrRevOrig := g.depotFileRevs[depotPathOrig].lbrRev
				g.depotFileRevs[gf.p4.srcDepotFile] = &RevChange{rev: 1, chgNo: chgNo, lbrRev: lbrRevOrig,
					lbrFile: lbrFileOrig, action: delete}
				gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
				if !gf.isDirtyRename {
					gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
					gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
				}
				gf.p4.branchDepotFile = depotPathOrig
				gf.p4.branchDepotRev = g.depotFileRevs[depotPathOrig].rev
				gf.fileType = g.getDepotFileTypes(depotPathOrig, g.depotFileRevs[depotPathOrig].rev)
				g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
				g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
				g.recordDepotFileType(gf)
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
	if gf.action == delete { // merge of delete
		gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
		gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
		gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
		g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
		g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
	} else if gf.action == rename { // Rename means old file is being deleted
		// If it is a merge of a rename then link to file on the branch
		handled := false
		if gf.isMerge {
			targOrigDepotPath := gf.getDepotPath(opts, gf.commit.mergeBranch, gf.name)
			srcOrigDepotPath := gf.getDepotPath(opts, gf.commit.mergeBranch, gf.srcName)
			if _, ok := g.depotFileRevs[targOrigDepotPath]; !ok {
				targOrigDepotPath = ""
			}
			if _, ok := g.depotFileRevs[srcOrigDepotPath]; !ok {
				srcOrigDepotPath = ""
			}
			if targOrigDepotPath != "" && srcOrigDepotPath != "" {
				handled = true
				g.depotFileRevs[gf.p4.srcDepotFile].rev += 1
				gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
				gf.p4.branchDepotFile = targOrigDepotPath
				gf.p4.branchDepotRev = g.depotFileRevs[targOrigDepotPath].rev
				gf.p4.branchSrcDepotFile = srcOrigDepotPath
				gf.p4.branchSrcDepotRev = g.depotFileRevs[srcOrigDepotPath].rev
				gf.fileType = g.getDepotFileTypes(targOrigDepotPath, g.depotFileRevs[targOrigDepotPath].rev)
				g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
				g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
				g.recordDepotFileType(gf)
			}
		}
		if !handled {
			g.depotFileRevs[gf.p4.srcDepotFile].rev += 1
			g.depotFileRevs[gf.p4.srcDepotFile].action = delete
			gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
			gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
			gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
			gf.fileType = g.getDepotFileTypes(gf.p4.srcDepotFile, gf.p4.srcRev-1)
			g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
			g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
			g.recordDepotFileType(gf)
		}
	} else { // Copy/branch
		gf.p4.srcRev = g.depotFileRevs[gf.p4.srcDepotFile].rev
		srcExists := g.depotFileTypeExists(gf.p4.srcDepotFile, gf.p4.srcRev)
		if !srcExists {
			g.logger.Debugf("UDR4: ")
			gf.isMerge = false
			gf.p4.srcDepotFile = ""
			gf.srcName = ""
		} else {
			gf.fileType = g.getDepotFileTypes(gf.p4.srcDepotFile, gf.p4.srcRev)
			if !gf.blob.hasData || gf.isMerge { // Copied but changed
				g.logger.Debugf("UDR5: ")
				gf.p4.lbrRev = g.depotFileRevs[gf.p4.srcDepotFile].lbrRev
				gf.p4.lbrFile = g.depotFileRevs[gf.p4.srcDepotFile].lbrFile
				g.depotFileRevs[gf.p4.depotFile].lbrRev = gf.p4.lbrRev
				g.depotFileRevs[gf.p4.depotFile].lbrFile = gf.p4.lbrFile
			} else {
				g.logger.Debugf("UDR6: ")
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
		currCommit.branch = g.opts.defaultBranch
	}
	if len(currCommit.commit.Merge) == 1 {
		firstMerge := currCommit.commit.Merge[0]
		if intVar, err := strconv.Atoi(firstMerge[1:]); err == nil {
			mergeFrom := g.commits[intVar]
			if mergeFrom.branch != "" {
				currCommit.mergeBranch = mergeFrom.branch
			} else {
				g.logger.Errorf("Merge Commit mark %d has no branch", intVar)
			}
		}
	} else if len(currCommit.commit.Merge) > 1 {
		// Potential for more than one merge, but we just log an error for now
		g.logger.Errorf("Commit mark %d has %d merges", currCommit.commit.Mark, len(currCommit.commit.Merge))
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

func (g *GitP4Transfer) processCommit(cmt *GitCommit) {
	if cmt != nil { // Process previous commit
		g.setBranch(cmt)
		g.logger.Debugf("CommitSummary: Mark:%d Files:%d Size:%d/%s",
			cmt.commit.Mark, len(cmt.files), cmt.commitSize, Humanize(cmt.commitSize))
		for i := range cmt.files {
			cmt.files[i].setDepotPaths(g.opts, cmt)
			cmt.files[i].updateFileDetails()
			g.updateDepotRevs(g.opts, cmt.files[i], cmt.commit.Mark)
		}
		if g.graph != nil { // Optional Graphviz structure to be output
			cmt.gNode = g.graph.Node(fmt.Sprintf("Commit: %d %s", cmt.commit.Mark, cmt.branch))
			g.createGraphEdges(cmt)
		}
		g.gitChan <- *cmt
	}
}

// GitParse - returns channel which contains commits with associated files.
func (g *GitP4Transfer) GitParse(options GitParserOptions) chan GitCommit {
	var buf io.Reader
	var file *os.File
	var err error

	if g.testInput != "" {
		buf = strings.NewReader(g.testInput)
	} else {
		file, err = os.Open(options.gitImportFile) // Note deferred close in go routine below.
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", options.gitImportFile, err)
			os.Exit(1)
		}
		buf = bufio.NewReader(file)
	}

	g.opts = options

	g.gitChan = make(chan GitCommit, 50)
	var currCommit *GitCommit
	var commitSize = 0
	commitCount := 0

	missingRenameLogged := false
	missingDeleteLogged := false
	node := &Node{name: ""} // Track directories and file trees

	// Create an unbuffered (blocking) pool with a fixed
	// number of workers
	pondSize := runtime.NumCPU()
	pool := pond.New(pondSize, 0, pond.MinWorkers(10))

	if g.opts.graphFile != "" { // Optional Graphviz structure to be output
		g.graph = dot.NewGraph(dot.Directed)
	}

	f := libfastimport.NewFrontend(buf, nil, nil)
	go func() {
		defer file.Close()
		defer close(g.gitChan)
		defer pool.StopAndWait()
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
			switch cmd.(type) {
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

			case libfastimport.CmdCommit:
				g.processCommit(currCommit)
				commit := cmd.(libfastimport.CmdCommit)
				g.logger.Debugf("Commit: %+v", commit)
				currCommit = newGitCommit(&commit, commitSize)
				commitSize = 0
				g.commits[commit.Mark] = currCommit
				if commit.Mark > 157570 {
					g.logger.Debugf("CommitOver: %+v", commit)
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
					if len(b.gitFileIDs) == 0 {
						gf = newGitFile(&GitFile{name: string(f.Path), action: modify,
							blob: b, fileType: b.fileType, compressed: b.compressed,
							logger: g.logger})
					} else {
						gf = newGitFile(&GitFile{name: string(f.Path), action: modify,
							blob: b, fileType: b.fileType, compressed: b.compressed,
							duplicateArchive: true, logger: g.logger})
					}
				} else {
					g.logger.Errorf("Failed to find blob: %d", oid)
				}
				// Search for renames (or deletes) of same file in current commit mark if so.
				dupGF := currCommit.findGitFile(gf.name)
				if dupGF != nil {
					if dupGF.action == rename {
						dupGF.isDirtyRename = true
						dupGF.blob = gf.blob
						dupGF.compressed = gf.compressed
						dupGF.duplicateArchive = gf.duplicateArchive
						dupGF.fileType = gf.fileType
						g.blobFileMatcher.addGitFile(dupGF)
						g.logger.Debugf("DirtyRenameFound: %s, GitFile: ID %d, %s, blobID %d, filetype: %s",
							dupGF.name, dupGF.ID, dupGF.name, dupGF.blob.blob.Mark, dupGF.blob.fileType)
					} else if dupGF.action == delete {
						// Having a modify with a delete doesn't make sense - we discard the delete!
						g.logger.Warnf("ModifyOfDeletedFile: GitFile: ID %d, %s, blobID %d, filetype: %s", gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
						currCommit.removeGitFile(gf.ID)
						g.blobFileMatcher.addGitFile(gf)
						g.logger.Debugf("GitFile: ID %d, %s, blobID %d, filetype: %s", gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
						currCommit.files = append(currCommit.files, gf)
					}
				} else {
					// Only record gitfiles when not duplicates
					g.blobFileMatcher.addGitFile(gf)
					g.logger.Debugf("GitFile: ID %d, %s, blobID %d, filetype: %s", gf.ID, gf.name, gf.blob.blob.Mark, gf.blob.fileType)
					currCommit.files = append(currCommit.files, gf)
				}
				node.addFile(gf.name)

			case libfastimport.FileDelete:
				f := cmd.(libfastimport.FileDelete)
				g.logger.Debugf("FileDelete: %s Path:%s", currCommit.ref(), f.Path)
				// Look for delete of dirs vs files
				if node.findFile(string(f.Path)) {
					dupGF := currCommit.findGitFileRename(string(f.Path))
					if dupGF != nil && dupGF.action == rename {
						g.logger.Warnf("DeleteOfRenamedFile ignored: GitFile: ID %d, %s", dupGF.ID, f.Path)
						currCommit.removeGitFile(dupGF.ID)
					} else {
						gf := newGitFile(&GitFile{name: string(f.Path), action: delete, logger: g.logger})
						currCommit.files = append(currCommit.files, gf)
						node.addFile(gf.name)
					}
				} else {
					files := node.getFiles(string(f.Path))
					if len(files) > 0 {
						g.logger.Debugf("DirDelete: Path:%s", f.Path)
						for _, df := range files {
							if !hasPrefix(df, string(f.Path)) {
								g.logger.Errorf("Unexpected path found: %s: %s", string(f.Path), df)
								continue
							}
							g.logger.Debugf("DirFileDelete: %s Path:%s", currCommit.ref(), df)
							dupGF := currCommit.findGitFileRename(df)
							if dupGF != nil && dupGF.action == rename {
								g.logger.Warnf("DeleteOfRenamedFile ignored: GitFile: ID %d, %s", dupGF.ID, df)
								currCommit.removeGitFile(dupGF.ID)
							} else {
								gf := newGitFile(&GitFile{name: df, action: delete, logger: g.logger})
								// Have to set up depot paths to be able to look up deleted files.
								g.setBranch(currCommit)
								gf.setDepotPaths(g.opts, currCommit)
								if g.isSrcDeletedFile(gf) {
									g.logger.Debugf("DirFileDeleteIgnoreDeleted: Path:%s", df)
								} else {
									currCommit.files = append(currCommit.files, gf)
									node.addFile(gf.name)
								}
							}
						}
					} else {
						g.logger.Errorf("FileDeleteMissing: Path:%s", string(f.Path))
						if g.logger.IsLevelEnabled(logrus.DebugLevel) && !missingDeleteLogged {
							missingDeleteLogged = true // only do it once
							nodeFiles := node.getFiles("")
							g.logger.Debugf("nodeFiles:")
							for _, s := range nodeFiles {
								g.logger.Debug(s)
							}
						}
					}
				}

			case libfastimport.FileCopy:
				f := cmd.(libfastimport.FileCopy)
				g.logger.Debugf("FileCopy: %s Src:%s Dst:%s", currCommit.ref(), f.Src, f.Dst)
				gf := newGitFile(&GitFile{name: string(f.Src), targ: string(f.Dst), action: copy, logger: g.logger})
				currCommit.files = append(currCommit.files, gf)

			case libfastimport.FileRename:
				f := cmd.(libfastimport.FileRename)
				g.logger.Debugf("FileRename: %s Src:%s Dst:%s", currCommit.ref(), f.Src, f.Dst)
				// Look for renames of dirs vs files
				if node.findFile(string(f.Src)) {
					gf := newGitFile(&GitFile{name: string(f.Dst), srcName: string(f.Src), action: rename, logger: g.logger})
					currCommit.files = append(currCommit.files, gf)
					node.addFile(gf.name)
				} else {
					files := node.getFiles(string(f.Src))
					if len(files) > 0 {
						g.logger.Debugf("DirRename: Src:%s Dst:%s", f.Src, f.Dst)
						for _, rf := range files {
							if !hasPrefix(rf, string(f.Src)) {
								g.logger.Errorf("Unexpected src found: %s: %s", string(f.Src), rf)
								continue
							}
							dest := fmt.Sprintf("%s%s", string(f.Dst), rf[len(string(f.Src)):])
							g.logger.Debugf("DirFileRename: %s Src:%s Dst:%s", currCommit.ref(), rf, dest)
							gf := newGitFile(&GitFile{name: dest, srcName: rf, action: rename, logger: g.logger})
							// Have to set up depot paths to be able to look up deleted files.
							g.setBranch(currCommit)
							gf.setDepotPaths(g.opts, currCommit)
							if g.isSrcDeletedFile(gf) {
								g.logger.Debugf("DirFileRenameIgnoreDeleted: Src:%s", rf)
							} else {
								currCommit.files = append(currCommit.files, gf)
								node.addFile(gf.name)
							}
						}
					} else {
						g.logger.Errorf("FileRenameMissing: Src:%s Dst:%s", string(f.Src), string(f.Dst))
						if g.logger.IsLevelEnabled(logrus.DebugLevel) && !missingRenameLogged {
							missingRenameLogged = true // only do it once
							nodeFiles := node.getFiles("")
							g.logger.Debugf("nodeFiles:")
							for _, s := range nodeFiles {
								g.logger.Debug(s)
							}
						}
					}
				}

			case libfastimport.CmdTag:
				t := cmd.(libfastimport.CmdTag)
				g.logger.Debugf("CmdTag: %+v", t)

			default:
				g.logger.Errorf("Not handled: Found cmd %+v", cmd)
				g.logger.Errorf("Cmd type %T", cmd)
			}
		}
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
		gitimport = kingpin.Arg(
			"gitimport",
			"Git fast-export file to process.",
		).String()
		importDepot = kingpin.Flag(
			"import.depot",
			"Git fast-export file to process.",
		).Default("import").Short('d').String()
		defaultBranch = kingpin.Flag(
			"default.branch",
			"Name of default git branch.",
		).Default("main").Short('b').String()
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
			"Max no of commits to process.",
		).Short('m').Int()
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
		).Int()
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
	startTime := time.Now()
	logger.Infof("%v", version.Print("gitp4transfer"))
	logger.Infof("Starting %s, gitimport: %v", startTime, *gitimport)

	g := NewGitP4Transfer(logger)
	opts := GitParserOptions{
		gitImportFile: *gitimport,
		importDepot:   *importDepot,
		archiveRoot:   *archive,
		defaultBranch: *defaultBranch,
		dryRun:        *dryrun,
		dummyArchives: *dummyArchives,
		maxCommits:    *maxCommits,
		graphFile:     *outputGraph,
	}

	if *dump {
		g.DumpGit(opts, *dumpArchives)
		return
	}

	commitChan := g.GitParse(opts)

	j := journal.Journal{}
	f, err := os.Create(*outputJournal)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	j.SetWriter(f)
	j.WriteHeader()

	for c := range commitChan {
		j.WriteChange(c.commit.Mark, c.user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			if !*dryrun {
				f.CreateArchiveFile(opts.archiveRoot, g.blobFileMatcher, c.commit.Mark)
			} else if f.blob != nil && f.blob.hasData && !f.blob.dataRemoved {
				f.blob.blob.Data = "" // Allow contents to be GC'ed
				f.blob.dataRemoved = true
			}
			f.WriteJournal(&j, &c)
		}
	}

	// close(filesChan)

}
