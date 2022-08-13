package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/h2non/filetype"
	journal "github.com/rcowham/gitp4transfer/journal"
	libfastimport "github.com/rcowham/go-libgitfastimport"

	"github.com/perforce/p4prometheus/version"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
)

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

const defaultBranch = "main"

type GitParserOptions struct {
	gitImportFile string
	extractFiles  bool
	archiveRoot   string
	createJournal bool
	dryRun        bool
	importDepot   string
	importPath    string // After depot and branch
	defaultBranch string
}

type GitAction int

const (
	unknown GitAction = iota
	modify
	delete
	copy
	rename
)

// Node - tree structure to record directory contents for directory renames
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

func writeBlob(rootDir string, blobID int, data *string) {
	dir, name := getBlobIDPath(rootDir, blobID)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create %s: %v", dir, err)
	}
	f, err := os.Create(name)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprint(f, *data)
	if err != nil {
		panic(err)
	}
}

// GitFile - A git file record - modify/delete/copy/move
type GitFile struct {
	name         string // Git filename (target for rename/copy)
	size         int
	depotFile    string // Full depot path
	rev          int    // Depot rev
	lbrRev       int    // Lbr rev - usually same as Depot rev
	lbrFile      string // Lbr file - usually same as Depot file
	srcName      string // Name of git source file for rename/copy
	srcDepotFile string //   "
	srcRev       int    //   "
	archiveFile  string
	action       GitAction
	p4action     journal.FileAction
	targ         string // For use with copy/move
	isBranch     bool
	fileType     journal.FileType
	compressed   bool
	blob         *libfastimport.CmdBlob
}

// GitCommit - A git commit
type GitCommit struct {
	commit     *libfastimport.CmdCommit
	branch     string // branch name
	commitSize int    // Size of all files in this commit - useful for memory sizing
	files      []GitFile
}

// HasPrefix tests whether the string s begins with prefix.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

func newGitCommit(commit *libfastimport.CmdCommit, commitSize int) *GitCommit {
	gc := &GitCommit{commit: commit, commitSize: commitSize, files: make([]GitFile, 0)}
	gc.branch = strings.Replace(commit.Ref, "refs/heads/", "", 1)
	if hasPrefix(gc.branch, "refs/tags") || hasPrefix(gc.branch, "refs/remote") {
		gc.branch = ""
	}
	return gc
}

func (gc *GitCommit) writeCommit(j *journal.Journal) {
	j.WriteChange(gc.commit.Mark, gc.commit.Msg, int(gc.commit.Author.Time.Unix()))
}

type CommitMap map[int]*GitCommit
type FileMap map[int]*GitFile
type RevChange struct { // Struct to remember revs and changes per depotFile
	rev     int
	chgNo   int
	lbrRev  int    // Normally same as chgNo but not for renames/copies
	lbrFile string // Normally same as depotFile byt not for renames/copies
	action  GitAction
}

// GitP4Transfer - Transfer via git fast-export file
type GitP4Transfer struct {
	exportFile     string
	logger         *logrus.Logger
	gitChan        chan GitCommit
	opts           GitParserOptions
	depotFileRevs  map[string]*RevChange       // Map depotFile to latest rev/chg
	depotFileTypes map[string]journal.FileType // Map depotFile#rev to filetype (for renames/branching)
	commits        map[int]*GitCommit
	testInput      string // For testing only
}

func NewGitP4Transfer(logger *logrus.Logger) *GitP4Transfer {
	return &GitP4Transfer{logger: logger,
		depotFileRevs:  make(map[string]*RevChange),
		depotFileTypes: make(map[string]journal.FileType),
		commits:        make(map[int]*GitCommit)}
}

func (gf *GitFile) getDepotPath(opts GitParserOptions, branch string, name string) string {
	if len(opts.importPath) == 0 {
		return fmt.Sprintf("//%s/%s/%s", opts.importDepot, branch, name)
	} else {
		return fmt.Sprintf("//%s/%s/%s/%s", opts.importDepot, opts.importPath, branch, name)
	}
}

func (gf *GitFile) setDepotPaths(opts GitParserOptions, branch string, prevBranch string) {
	gf.depotFile = gf.getDepotPath(opts, branch, gf.name)
	if gf.srcName != "" {
		gf.srcDepotFile = gf.getDepotPath(opts, branch, gf.srcName)
	} else if prevBranch != "" {
		gf.srcName = gf.name
		gf.isBranch = true
		gf.srcDepotFile = gf.getDepotPath(opts, prevBranch, gf.srcName)
	}
}

// Sets compression option and binary/text
func (gf *GitFile) updateFileDetails() {
	switch gf.action {
	case delete:
		gf.p4action = journal.Delete
		return
	case rename:
		gf.p4action = journal.Rename
		return
	case modify:
		gf.p4action = journal.Edit
	}
	// Compression defaults to false
	l := len(gf.blob.Data)
	if l > 261 {
		l = 261
	}
	head := []byte(gf.blob.Data[:l])
	if filetype.IsImage(head) || filetype.IsVideo(head) || filetype.IsArchive(head) || filetype.IsAudio(head) {
		gf.fileType = journal.UBinary
		return
	}
	if filetype.IsDocument(head) {
		gf.fileType = journal.Binary
		kind, _ := filetype.Match(head)
		switch kind.Extension {
		case "docx", "pptx", "xlsx":
			return // no compression
		}
		gf.compressed = true
		return
	}
	gf.fileType = journal.CText
	gf.compressed = true
}

func getOID(dataref string) (int, error) {
	if !strings.HasPrefix(dataref, ":") {
		return 0, errors.New("invalid dataref")
	}
	return strconv.Atoi(dataref[1:])
}

// WriteFile will write a data file using standard path: <depotRoot>/<path>,d/1.<changeNo>[.gz]
func (gf *GitFile) WriteFile(depotRoot string, changeNo int) error {
	if gf.action == delete || gf.action == rename {
		return nil
	}
	depotFile := strings.ReplaceAll(gf.depotFile[2:], "@", "%40")
	rootDir := fmt.Sprintf("%s/%s,d", depotRoot, depotFile)
	err := os.MkdirAll(rootDir, 0755)
	if err != nil {
		panic(err)
	}
	if gf.compressed {
		gf.compressed = true
		fname := fmt.Sprintf("%s/1.%d.gz", rootDir, changeNo)
		f, err := os.Create(fname)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		zw := gzip.NewWriter(f)
		defer zw.Close()
		_, err = zw.Write([]byte(gf.blob.Data))
		if err != nil {
			panic(err)
		}
	} else {
		fname := fmt.Sprintf("%s/1.%d", rootDir, changeNo)
		f, err := os.Create(fname)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		fmt.Fprint(f, gf.blob.Data)
		if err != nil {
			panic(err)
		}
	}
	return nil
}

// WriteFile will write a data file using standard path: <depotRoot>/<path>,d/1.<changeNo>[.gz]
func (gf *GitFile) WriteJournal(j *journal.Journal, c *GitCommit) {
	dt := int(c.commit.Author.Time.Unix())
	chgNo := c.commit.Mark
	if gf.action == modify {
		if gf.isBranch {
			// we write rev for newly branched depot file, with link to old version
			j.WriteRev(gf.depotFile, gf.rev, journal.Add, gf.fileType, chgNo, gf.lbrFile, gf.lbrRev, dt)
			j.WriteInteg(gf.depotFile, gf.srcDepotFile, 0, gf.srcRev, 0, gf.rev, journal.BranchFrom, journal.DirtyBranchInto, c.commit.Mark)
		} else {
			j.WriteRev(gf.depotFile, gf.rev, gf.p4action, gf.fileType, chgNo, gf.lbrFile, gf.lbrRev, dt)
		}
	} else if gf.action == delete {
		j.WriteRev(gf.depotFile, gf.rev, gf.p4action, gf.fileType, chgNo, gf.lbrFile, gf.lbrRev, dt)
	} else if gf.action == rename {
		j.WriteRev(gf.srcDepotFile, gf.srcRev, journal.Delete, gf.fileType, chgNo, gf.lbrFile, gf.lbrRev, dt)
		j.WriteRev(gf.depotFile, gf.rev, journal.Add, gf.fileType, chgNo, gf.lbrFile, gf.lbrRev, dt)
		// TODO - don't use 0 for startfromRev, startToRev
		j.WriteInteg(gf.depotFile, gf.srcDepotFile, 0, gf.srcRev, 0, gf.rev, journal.BranchFrom, journal.BranchInto, c.commit.Mark)
	}
}

// RunGetCommits - for small files - returns list of all commits and files.
// Mainly for testing.
func (g *GitP4Transfer) RunGetCommits(options GitParserOptions) (CommitMap, FileMap) {
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
	gitFiles := make(map[int]*GitFile, 0)
	var currCommit *GitCommit

	f := libfastimport.NewFrontend(buf, nil, nil)
	for {
		cmd, err := f.ReadCmd()
		if err != nil {
			if err != io.EOF {
				fmt.Printf("ERROR: Failed to read cmd: %v\n", err)
			}
			break
		}
		switch cmd.(type) {
		case libfastimport.CmdBlob:
			blob := cmd.(libfastimport.CmdBlob)
			g.logger.Debugf("Blob: Mark:%d OriginalOID:%s Size:%s\n", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
			gitFiles[blob.Mark] = &GitFile{blob: &blob}
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Debugf("Reset: - %+v\n", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Debugf("Commit:  %+v\n", commit)
			currCommit = newGitCommit(&commit, 0)
			commits[commit.Mark] = currCommit
		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Debugf("CommitEnd:  %+v\n", commit)
		case libfastimport.FileModify:
			f := cmd.(libfastimport.FileModify)
			g.logger.Debugf("FileModify:  %+v\n", f)
			oid, err := getOID(f.DataRef)
			if err != nil {
				g.logger.Errorf("Failed to get oid: %+v", f)
			}
			gf, ok := gitFiles[oid]
			if ok {
				gf.name = f.Path.String()
				gf.updateFileDetails()
				currCommit.files = append(currCommit.files, *gf)
			}
		case libfastimport.FileDelete:
			f := cmd.(libfastimport.FileDelete)
			g.logger.Debugf("FileModify: Path:%s\n", f.Path)
		case libfastimport.FileCopy:
			f := cmd.(libfastimport.FileCopy)
			g.logger.Debugf("FileCopy: Src:%s Dst:%s\n", f.Src, f.Dst)
		case libfastimport.FileRename:
			f := cmd.(libfastimport.FileRename)
			g.logger.Debugf("FileRename: Src:%s Dst:%s\n", f.Src, f.Dst)
		case libfastimport.CmdTag:
			t := cmd.(libfastimport.CmdTag)
			g.logger.Debugf("CmdTag: %+v\n", t)
		default:
			g.logger.Debugf("Not handled\n")
			g.logger.Debugf("Found cmd %+v\n", cmd)
			g.logger.Debugf("Cmd type %T\n", cmd)
		}
	}
	return commits, gitFiles
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
			// We write the blobs as we go to avoid using up too much memory
			if saveFiles {
				writeBlob(g.opts.archiveRoot, blob.Mark, &blob.Data)
			}
			blob.Data = ""
			files[blob.Mark] = &GitFile{blob: &blob, size: size}
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Infof("Reset: - %+v", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Infof("Commit:  %+v", commit)
			currCommit = newGitCommit(&commit, commitSize)
			commitSize = 0
			commits[commit.Mark] = currCommit
		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Infof("CommitEnd:  %+v", commit)
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
				gf.name = f.Path.String()
				_, archName := getBlobIDPath(g.opts.archiveRoot, gf.blob.Mark)
				gf.archiveFile = archName
				g.logger.Infof("Path:%s Size:%s Archive:%s", gf.name, Humanize(gf.size), gf.archiveFile)
				currCommit.files = append(currCommit.files, *gf)
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
	if gf.srcDepotFile == "" {
		return false
	}
	if f, ok := g.depotFileRevs[gf.srcDepotFile]; ok {
		return f.action == delete
	}
	return false
}

// Maintain a list of latest revision counters indexed by depotFile
func (g *GitP4Transfer) updateDepotRevs(gf *GitFile, chgNo int) {
	prevAction := unknown
	if _, ok := g.depotFileRevs[gf.depotFile]; !ok {
		g.depotFileRevs[gf.depotFile] = &RevChange{rev: 0, chgNo: chgNo, lbrRev: chgNo,
			lbrFile: gf.depotFile, action: gf.action}
	}
	if gf.action == delete {
		gf.fileType = g.getDepotFileTypes(gf.depotFile, g.depotFileRevs[gf.depotFile].rev)
	}
	g.depotFileRevs[gf.depotFile].rev += 1
	if g.depotFileRevs[gf.depotFile].rev > 1 {
		prevAction = g.depotFileRevs[gf.depotFile].action
	}
	g.depotFileRevs[gf.depotFile].action = gf.action
	g.depotFileRevs[gf.depotFile].lbrRev = chgNo
	g.depotFileRevs[gf.depotFile].lbrFile = gf.depotFile
	gf.lbrRev = chgNo
	gf.lbrFile = gf.depotFile
	isRename := (gf.action == rename)
	gf.rev = g.depotFileRevs[gf.depotFile].rev
	if gf.action == modify {
		// modify defaults to edit, except when first rev or previously deleted
		if gf.rev == 1 || prevAction == delete {
			gf.p4action = journal.Add
		}
	}
	if gf.srcName == "" {
		g.updateDepotFileTypes(gf)
	} else {
		gf.p4action = journal.Add
		if _, ok := g.depotFileRevs[gf.srcDepotFile]; !ok {
			// A copy or branch without a source file just becomes new file added on branch
			g.logger.Debugf("Copy/branch becomes add: %s/%s", gf.depotFile, gf.srcDepotFile)
			gf.srcDepotFile = ""
			gf.srcName = ""
			gf.isBranch = false
			g.updateDepotFileTypes(gf)
			return
		}
		if isRename { // Rename means old file is being deleted
			g.depotFileRevs[gf.srcDepotFile].rev += 1
			g.depotFileRevs[gf.srcDepotFile].action = delete
			gf.srcRev = g.depotFileRevs[gf.srcDepotFile].rev
			gf.lbrFile = g.depotFileRevs[gf.srcDepotFile].lbrFile
			gf.lbrRev = g.depotFileRevs[gf.srcDepotFile].lbrRev
			gf.fileType = g.getDepotFileTypes(gf.srcDepotFile, gf.srcRev-1)
			g.depotFileRevs[gf.depotFile].lbrRev = gf.lbrRev
			g.depotFileRevs[gf.depotFile].lbrFile = gf.lbrFile
			g.updateDepotFileTypes(gf)
		} else { // Copy/branch
			gf.srcRev = g.depotFileRevs[gf.srcDepotFile].rev
			gf.fileType = g.getDepotFileTypes(gf.srcDepotFile, gf.srcRev)
			if len(gf.blob.Data) == 0 { // Copied but changed
				gf.lbrRev = g.depotFileRevs[gf.srcDepotFile].lbrRev
				gf.lbrFile = g.depotFileRevs[gf.srcDepotFile].lbrFile
			} else {
				g.depotFileRevs[gf.depotFile].lbrRev = gf.lbrRev
				g.depotFileRevs[gf.depotFile].lbrFile = gf.lbrFile
			}
			g.updateDepotFileTypes(gf)
		}
	}
}

// Maintain a list of latest revision counters indexed by depotFile/rev
func (g *GitP4Transfer) updateDepotFileTypes(gf *GitFile) {
	k := fmt.Sprintf("%s#%d", gf.depotFile, gf.rev)
	g.depotFileTypes[k] = gf.fileType
}

// Retrieve required filetype
func (g *GitP4Transfer) getDepotFileTypes(depotFile string, rev int) journal.FileType {
	k := fmt.Sprintf("%s#%d", depotFile, rev)
	if _, ok := g.depotFileTypes[k]; !ok {
		return 0
	}
	return g.depotFileTypes[k]
}

func (g *GitP4Transfer) setBranch(currCommit *GitCommit) string {
	// Sets the branch for the current commit, using its parent if not otherwise specified
	prevBranch := ""
	if currCommit == nil {
		return prevBranch
	}
	if currCommit.commit.From != "" {
		if intVar, err := strconv.Atoi(currCommit.commit.From[1:]); err == nil {
			parent := g.commits[intVar]
			if currCommit.branch == "" {
				currCommit.branch = parent.branch
			}
			if currCommit.branch != parent.branch {
				prevBranch = parent.branch
			}
		}
	} else if currCommit.branch == "" {
		currCommit.branch = g.opts.defaultBranch
	}
	return prevBranch
}

func (g *GitP4Transfer) processCommit(currCommit *GitCommit) {
	if currCommit != nil { // Process previous commit
		prevBranch := g.setBranch(currCommit)
		for i := range currCommit.files {
			currCommit.files[i].setDepotPaths(g.opts, currCommit.branch, prevBranch)
			currCommit.files[i].updateFileDetails()
			g.updateDepotRevs(&currCommit.files[i], currCommit.commit.Mark)
		}
		g.gitChan <- *currCommit
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

	g.gitChan = make(chan GitCommit, 100)
	blobFiles := make(map[int]*GitFile, 0) // Index by blob ID (mark)
	var currCommit *GitCommit

	missingRenameLogged := false
	node := &Node{name: ""}

	f := libfastimport.NewFrontend(buf, nil, nil)
	go func() {
		defer file.Close()
		defer close(g.gitChan)
		for {
			cmd, err := f.ReadCmd()
			if err != nil {
				if err != io.EOF {
					g.logger.Errorf("ERROR: Failed to read cmd: %v", err)
				}
				break
			}
			switch cmd.(type) {
			case libfastimport.CmdBlob:
				blob := cmd.(libfastimport.CmdBlob)
				g.logger.Debugf("Blob: Mark:%d OriginalOID:%s Size:%s", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
				blobFiles[blob.Mark] = &GitFile{blob: &blob, action: modify}
			case libfastimport.CmdReset:
				reset := cmd.(libfastimport.CmdReset)
				g.logger.Debugf("Reset: - %+v", reset)
			case libfastimport.CmdCommit:
				g.processCommit(currCommit)
				commit := cmd.(libfastimport.CmdCommit)
				g.logger.Debugf("Commit:  %+v", commit)
				currCommit = newGitCommit(&commit, 0)
				g.commits[commit.Mark] = currCommit
			case libfastimport.CmdCommitEnd:
				commit := cmd.(libfastimport.CmdCommitEnd)
				g.logger.Debugf("CommitEnd: %+v", commit)
			case libfastimport.FileModify:
				f := cmd.(libfastimport.FileModify)
				g.logger.Debugf("FileModify: %+v", f)
				oid, err := getOID(f.DataRef)
				if err != nil {
					g.logger.Errorf("Failed to get oid: %+v", f)
				}
				gf, ok := blobFiles[oid]
				if ok {
					gf.name = f.Path.String()
					currCommit.files = append(currCommit.files, *gf)
					node.addFile(gf.name)
				} else {
					g.logger.Errorf("Failed to find blob: %+v", f)
				}
			case libfastimport.FileDelete:
				f := cmd.(libfastimport.FileDelete)
				g.logger.Debugf("FileDelete: Path:%s", f.Path)
				gf := &GitFile{name: f.Path.String(), action: delete}
				currCommit.files = append(currCommit.files, *gf)
			case libfastimport.FileCopy:
				f := cmd.(libfastimport.FileCopy)
				g.logger.Debugf("FileCopy: Src:%s Dst:%s", f.Src, f.Dst)
				gf := &GitFile{name: f.Src.String(), targ: f.Dst.String(), action: copy}
				currCommit.files = append(currCommit.files, *gf)
			case libfastimport.FileRename:
				f := cmd.(libfastimport.FileRename)
				g.logger.Debugf("FileRename: Src:%s Dst:%s", f.Src, f.Dst)
				// Look for renames of dirs vs files
				if node.findFile(f.Src.String()) {
					gf := &GitFile{name: f.Dst.String(), srcName: f.Src.String(), action: rename}
					currCommit.files = append(currCommit.files, *gf)
					node.addFile(gf.name)
				} else {
					files := node.getFiles(f.Src.String())
					if len(files) > 0 {
						g.logger.Debugf("DirRename: Src:%s Dst:%s", f.Src, f.Dst)
						for _, rf := range files {
							if !hasPrefix(rf, f.Src.String()) {
								g.logger.Errorf("Unexpected src found: %s: %s", f.Src.String(), rf)
								continue
							}
							dest := fmt.Sprintf("%s%s", f.Dst.String(), rf[len(f.Src.String()):])
							g.logger.Debugf("DirFileRename: Src:%s Dst:%s", rf, dest)
							gf := &GitFile{name: dest, srcName: rf, action: rename}
							// Have to set up depot paths to be able to look up deleted files.
							prevBranch := g.setBranch(currCommit)
							gf.setDepotPaths(g.opts, currCommit.branch, prevBranch)
							if g.isSrcDeletedFile(gf) {
								g.logger.Debugf("DirFileRenameIgnoreDeleted: Src:%s", rf)
							} else {
								currCommit.files = append(currCommit.files, *gf)
								node.addFile(gf.name)
							}
						}
					} else {
						g.logger.Errorf("FileRenameMissing: Src:%s Dst:%s", f.Src.String(), f.Dst.String())
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
		dump = kingpin.Flag(
			"dump",
			"Dump git file, saving the contained archive contents.",
		).Bool()
		dumpArchives = kingpin.Flag(
			"dump.archives",
			"Saving the contained archive contents if --dump is specified.",
		).Short('a').Bool()
		dryrun = kingpin.Flag(
			"dryrun",
			"Don't actually create archive files.",
		).Bool()
		archive = kingpin.Flag(
			"archive.root",
			"Archive root dir under which to store extracted archives.",
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
		j.WriteChange(c.commit.Mark, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			if !*dryrun {
				f.WriteFile(opts.archiveRoot, c.commit.Mark)
			}
			f.WriteJournal(&j, &c)
		}
	}

}
