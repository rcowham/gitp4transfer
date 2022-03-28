package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
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
	commit *libfastimport.CmdCommit
	files  []GitFile
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
func (gf *GitFile) WriteFile(depotRoot string, compressed bool, changeNo string) error {

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
	commits, files := g.RunGetCommits(opts)

	for _, v := range commits {
		g.logger.Infof("Found commit: id %d, files: %d", v.commit.Mark, len(v.files))
	}
	for _, v := range files {
		g.logger.Infof("Found file: %s, %d", v.name, v.blob.Mark)
	}
}
