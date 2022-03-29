package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rcowham/gitp4transfer/journal"
	libfastimport "github.com/rcowham/go-libgitfastimport"

	"github.com/rcowham/p4training/version"
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

type GitParserOptions struct {
	gitImportFile string
	extractFiles  bool
	createJournal bool
	importDepot   string
	importPath    string // After depot
}

type GitAction int

const (
	modify GitAction = iota
	delete
	copy
	rename
)

// Performs simple hash
func getBlobIDPath(blobID int) (dir string, name string) {
	n := fmt.Sprintf("%08d", blobID)
	d := path.Join(n[0:2], n[2:5], n[5:8])
	n = path.Join(d, n)
	return d, n
}

func writeBlob(rootDir string, blobID int, data *string) {
	dir, name := getBlobIDPath(blobID)
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
	name      string
	depotFile string
	action    GitAction
	targ      string // For use with copy/move
	fileType  string
	blob      *libfastimport.CmdBlob
}

// GitCommit - A git commit
type GitCommit struct {
	commit     *libfastimport.CmdCommit
	commitSize int // Size of all files in this commit - useful for memory sizing
	files      []GitFile
}

func (gc *GitCommit) writeCommit(j *journal.Journal) {
	j.WriteChange(gc.commit.Mark, gc.commit.Msg, int(gc.commit.Author.Time.Unix()))
}

type CommitMap map[int]*GitCommit
type FileMap map[int]*GitFile

// GitP4Transfer - Transfer via git fast-export file
type GitP4Transfer struct {
	exportFile string
	logger     *logrus.Logger
	gitChan    chan GitCommit
	opts       GitParserOptions
	testInput  string // For testing only
}

func NewGitP4Transfer(logger *logrus.Logger) *GitP4Transfer {
	return &GitP4Transfer{logger: logger}
}

func (gf *GitFile) setDepotPath(opts GitParserOptions) {
	if len(opts.importPath) == 0 {
		gf.depotFile = fmt.Sprintf("//%s/%s", opts.importDepot, gf.name)
	} else {
		gf.depotFile = fmt.Sprintf("//%s/%s/%s", opts.importDepot, opts.importPath, gf.name)
	}
}

func getOID(dataref string) (int, error) {
	if !strings.HasPrefix(dataref, ":") {
		return 0, errors.New("Invalid dataref")
	}
	return strconv.Atoi(dataref[1:])
}

// WriteFile will write a data file using standard path: <depotRoot>/<path>,d/1.<changeNo>[.gz]
func (gf *GitFile) WriteFile(depotRoot string, compressed bool, changeNo int) error {
	rootDir := fmt.Sprintf("%s/%s,d", depotRoot, gf.depotFile[2:])
	err := os.MkdirAll(rootDir, 0755)
	if err != nil {
		panic(err)
	}
	if compressed {
		// TODO - actually compress!
		fname := fmt.Sprintf("%s/1.%d.gz", rootDir, changeNo)
		f, err := os.Create(fname)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		fmt.Fprint(f, gf.blob.Data)
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

// RunGetCommits - for small files - returns list of all commits and files.
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
	files := make(map[int]*GitFile, 0)
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
			files[blob.Mark] = &GitFile{blob: &blob}
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Debugf("Reset: - %+v\n", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Debugf("Commit:  %+v\n", commit)
			currCommit = &GitCommit{commit: &commit, files: make([]GitFile, 0)}
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
			gf, ok := files[oid]
			if ok {
				gf.name = f.Path.String()
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
	return commits, files
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
	var currCommit *GitCommit
	var commitSize = 0

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
			g.logger.Infof("Blob: Mark:%d OriginalOID:%s Size:%s\n", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
			commitSize += len(blob.Data)
			// We write the blobs as we go to avoid using up too much memory
			if saveFiles {
				writeBlob("", blob.Mark, &blob.Data)
			}
			blob.Data = ""
			files[blob.Mark] = &GitFile{blob: &blob}
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Infof("Reset: - %+v\n", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Infof("Commit:  %+v\n", commit)
			currCommit = &GitCommit{commit: &commit, commitSize: commitSize, files: make([]GitFile, 0)}
			commitSize = 0
			commits[commit.Mark] = currCommit
		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Infof("CommitEnd:  %+v\n", commit)
		case libfastimport.FileModify:
			f := cmd.(libfastimport.FileModify)
			g.logger.Infof("FileModify:  %+v\n", f)
			oid, err := getOID(f.DataRef)
			if err != nil {
				g.logger.Errorf("Failed to get oid: %+v", f)
			}
			gf, ok := files[oid]
			if ok {
				gf.name = f.Path.String()
				currCommit.files = append(currCommit.files, *gf)
			}
		case libfastimport.FileDelete:
			f := cmd.(libfastimport.FileDelete)
			g.logger.Infof("FileModify: Path:%s\n", f.Path)
		case libfastimport.FileCopy:
			f := cmd.(libfastimport.FileCopy)
			g.logger.Infof("FileCopy: Src:%s Dst:%s\n", f.Src, f.Dst)
		case libfastimport.FileRename:
			f := cmd.(libfastimport.FileRename)
			g.logger.Infof("FileRename: Src:%s Dst:%s\n", f.Src, f.Dst)
		case libfastimport.CmdTag:
			t := cmd.(libfastimport.CmdTag)
			g.logger.Infof("CmdTag: %+v\n", t)
		default:
			g.logger.Errorf("Not handled - found cmd %+v\n", cmd)
			g.logger.Infof("Cmd type %T\n", cmd)
		}
	}
}

// GitParse - returns channel which contains commits with associated files.
func (g *GitP4Transfer) GitParse(options GitParserOptions) chan GitCommit {
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

	g.gitChan = make(chan GitCommit, 100)
	files := make(map[int]*GitFile, 0)
	var currCommit *GitCommit

	f := libfastimport.NewFrontend(buf, nil, nil)
	go func() {
		defer close(g.gitChan)
		for {
			cmd, err := f.ReadCmd()
			if err != nil {
				if err != io.EOF {
					g.logger.Errorf("ERROR: Failed to read cmd: %v\n", err)
				}
				break
			}
			switch cmd.(type) {
			case libfastimport.CmdBlob:
				blob := cmd.(libfastimport.CmdBlob)
				g.logger.Debugf("Blob: Mark:%d OriginalOID:%s Size:%s\n", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
				files[blob.Mark] = &GitFile{blob: &blob, action: modify}
				files[blob.Mark].setDepotPath(g.opts)
			case libfastimport.CmdReset:
				reset := cmd.(libfastimport.CmdReset)
				g.logger.Debugf("Reset: - %+v\n", reset)
			case libfastimport.CmdCommit:
				if currCommit != nil {
					g.gitChan <- *currCommit
				}
				commit := cmd.(libfastimport.CmdCommit)
				g.logger.Debugf("Commit:  %+v\n", commit)
				currCommit = &GitCommit{commit: &commit, files: make([]GitFile, 0)}
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
				gf, ok := files[oid]
				if ok {
					gf.name = f.Path.String()
					gf.setDepotPath(g.opts)
					currCommit.files = append(currCommit.files, *gf)
				}
			case libfastimport.FileDelete:
				f := cmd.(libfastimport.FileDelete)
				g.logger.Debugf("FileModify: Path:%s\n", f.Path)
				gf := &GitFile{name: f.Path.String(), action: delete}
				gf.setDepotPath(g.opts)
				currCommit.files = append(currCommit.files, *gf)
			case libfastimport.FileCopy:
				f := cmd.(libfastimport.FileCopy)
				g.logger.Debugf("FileCopy: Src:%s Dst:%s\n", f.Src, f.Dst)
				gf := &GitFile{name: f.Src.String(), targ: f.Dst.String(), action: copy}
				gf.setDepotPath(g.opts)
				currCommit.files = append(currCommit.files, *gf)
			case libfastimport.FileRename:
				f := cmd.(libfastimport.FileRename)
				g.logger.Debugf("FileRename: Src:%s Dst:%s\n", f.Src, f.Dst)
				gf := &GitFile{name: f.Src.String(), targ: f.Dst.String(), action: rename}
				gf.setDepotPath(g.opts)
				currCommit.files = append(currCommit.files, *gf)
			case libfastimport.CmdTag:
				t := cmd.(libfastimport.CmdTag)
				g.logger.Debugf("CmdTag: %+v\n", t)
			default:
				g.logger.Errorf("Not handled: Found cmd %+v\n", cmd)
				g.logger.Errorf("Cmd type %T\n", cmd)
			}
		}
		if currCommit != nil {
			g.gitChan <- *currCommit
		}

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
		dump = kingpin.Flag(
			"dump",
			"Dump git file, saving the contained archive contents.",
		).Bool()
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
	logger.Infof("%v", version.Print("log2sql"))
	logger.Infof("Starting %s, gitimport: %v", startTime, *gitimport)

	g := NewGitP4Transfer(logger)
	opts := GitParserOptions{
		gitImportFile: *gitimport,
	}

	if *dump {
		g.DumpGit(opts, true)
	} else {
		commits, files := g.RunGetCommits(opts)
		for _, v := range commits {
			g.logger.Infof("Found commit: id %d, files: %d", v.commit.Mark, len(v.files))
		}
		for _, v := range files {
			g.logger.Infof("Found file: %s, %d", v.name, v.blob.Mark)
		}
	}
}
