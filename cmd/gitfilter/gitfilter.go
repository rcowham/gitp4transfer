package main

// Filters git fast-import files - removing contents of blobs (converting them to unique integers) - but
// otherwise keeping structure the same.

import (
	"bufio"
	"errors"
	"fmt"
	"io"               // profiling only
	_ "net/http/pprof" // profiling only
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	node "github.com/rcowham/gitp4transfer/node"
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

// append source onto target
func appendfile(src, dst string) error {
	const BUFFERSIZE = 1024 * 1024
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file.", src)
	}
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer destination.Close()

	buf := make([]byte, BUFFERSIZE)
	for {
		n, err := source.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		if _, err := destination.Write(buf[:n]); err != nil {
			return err
		}
	}
	return err
}

type GitFilterOptions struct {
	gitImportFile string
	gitExportFile string
	renameRefs    bool
	filterCommits bool
	pathFilter    string // Regex to filter output only to specified files
	maxCommits    int    // Max number of commits to process
}

// GitFilter - filter a git fast-export file
type GitFilter struct {
	logger        *logrus.Logger
	opts          GitFilterOptions
	filesOnBranch map[string]*node.Node
	blobsFound    map[int]int
	testInput     string // For testing only
}

func NewGitFilter(logger *logrus.Logger) *GitFilter {
	return &GitFilter{logger: logger}
}

type MyWriteCloser struct {
	f *os.File
	*bufio.Writer
}

func (mwc *MyWriteCloser) Close() error {
	if err := mwc.Flush(); err != nil {
		return err
	}
	return mwc.f.Close()
}

func getOID(dataref string) (int, error) {
	if !strings.HasPrefix(dataref, ":") {
		return 0, errors.New("invalid dataref")
	}
	return strconv.Atoi(dataref[1:])
}

// FilterGitCommit - A git commit
type FilterGitCommit struct {
	commit       *libfastimport.CmdCommit
	fileCount    int
	mergeCount   int
	branch       string
	parentBranch string
	mergeBranch  []string
	filtered     bool // True => has been filtered out
}

type CommitMap map[int]*FilterGitCommit

// Work out which commits can be filtered - parses the input file once
func (g *GitFilter) filterCommits(options GitFilterOptions, rePathFilter *regexp.Regexp) *CommitMap {
	var inbuf io.Reader
	var infile *os.File
	var err error
	commitMap := make(CommitMap, 0)

	infile, err = os.Open(options.gitImportFile) // Note deferred close below.
	if err != nil {
		fmt.Printf("ERROR: Failed to open file '%s': %v\n", options.gitImportFile, err)
		os.Exit(1)
	}
	inbuf = bufio.NewReader(infile)
	defer infile.Close()

	frontend := libfastimport.NewFrontend(inbuf, nil, nil)
	commitCount := 0
	currFileCount := 0
	var currCommit *FilterGitCommit
CmdLoop:
	for {
		cmd, err := frontend.ReadCmd()
		if err != nil {
			if err != io.EOF {
				g.logger.Errorf("ERROR: Failed to read cmd: %v", err)
			}
			break
		}
		switch cmd.(type) {

		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			currCommit = &FilterGitCommit{commit: &commit, mergeBranch: make([]string, 0)}

		case libfastimport.CmdCommitEnd:
			commitCount += 1
			if g.opts.maxCommits > 0 && commitCount >= g.opts.maxCommits {
				break CmdLoop
			}
			currCommit.fileCount = currFileCount
			commitMap[currCommit.commit.Mark] = currCommit
			if currCommit.commit.From != "" {
				currCommit.branch = strings.Replace(currCommit.commit.Ref, "refs/heads/", "", 1)
				if intVar, err := strconv.Atoi(currCommit.commit.From[1:]); err == nil {
					parent := commitMap[intVar]
					if currCommit.branch == "" {
						currCommit.branch = parent.branch
					}
					currCommit.parentBranch = parent.parentBranch
					if currCommit.parentBranch == "" {
						currCommit.parentBranch = parent.branch
					}
				}
			} else {
				currCommit.branch = "main"
			}
			if len(currCommit.commit.Merge) > 0 {
				for _, merge := range currCommit.commit.Merge {
					if intVar, err := strconv.Atoi(merge[1:]); err == nil {
						mergeCmt := commitMap[intVar]
						mergeCmt.mergeCount += 1
						currCommit.mergeBranch = append(currCommit.mergeBranch, mergeCmt.branch)
					}
				}
			}
			currFileCount = 0

		case libfastimport.FileModify:
			fm := cmd.(libfastimport.FileModify)
			if rePathFilter.MatchString(string(fm.Path)) {
				currFileCount += 1
			}

		case libfastimport.FileDelete:
			fdel := cmd.(libfastimport.FileDelete)
			if rePathFilter.MatchString(string(fdel.Path)) {
				currFileCount += 1
			}

		case libfastimport.FileCopy:
			fc := cmd.(libfastimport.FileCopy)
			if rePathFilter.MatchString(string(fc.Src)) || rePathFilter.MatchString(string(fc.Dst)) {
				currFileCount += 1
			}

		case libfastimport.FileRename:
			fr := cmd.(libfastimport.FileRename)
			if rePathFilter.MatchString(string(fr.Src)) || rePathFilter.MatchString(string(fr.Dst)) {
				currFileCount += 1
			}

		default:
		}
	}
	return &commitMap
}

// Finds the first parent who hasn't been filtered
func (g *GitFilter) findUnfilteredParent(commitMap *CommitMap, from string) string {
	var mark int
	var err error

	if from == "" {
		return from
	}
	for {
		if mark, err = strconv.Atoi(from[1:]); err == nil {
			if parent, ok := (*commitMap)[mark]; ok {
				if !parent.filtered {
					return from
				}
				from = parent.commit.From
			} else {
				g.logger.Errorf("ERROR: Failed to find parent from: %s", from)
			}
		} else {
			g.logger.Errorf("ERROR: Failed to extract int from: %s", from)
			return from
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

type GitAction int

const (
	unknown GitAction = iota
	modify
	delete
	copy
	rename
)

type FileAction struct {
	name    string
	srcName string // For copy
	action  GitAction
	mode    libfastimport.Mode
	dataRef string
}

// MyCommit - A git commit
type MyCommit struct {
	commit       *libfastimport.CmdCommit
	branch       string
	parentBranch string
	mergeBranch  []string
	files        []FileAction
}

func (c *MyCommit) ref() string {
	result := strings.Split(c.commit.Msg, " ")
	chg := "unknown"
	if len(result) >= 2 {
		chg = result[1]
	}
	return fmt.Sprintf("%d branch:%s chg:%s merge:%v", c.commit.Mark, c.branch, chg, c.mergeBranch)
}

// Validate the commit, expanding directory actions, and identifying/filtering out others that don't make sense
func (g *GitFilter) validateCommit(cmt *MyCommit) {
	if cmt == nil {
		return
	}
	// If a new branch, then copy files from parent
	if _, ok := g.filesOnBranch[cmt.parentBranch]; !ok {
		g.filesOnBranch[cmt.parentBranch] = &node.Node{Name: ""}
	}
	if _, ok := g.filesOnBranch[cmt.branch]; !ok {
		g.filesOnBranch[cmt.branch] = &node.Node{Name: ""}
		pfiles := g.filesOnBranch[cmt.parentBranch].GetFiles("")
		g.logger.Infof("Creating new branch %s with %d files from parent %s", cmt.branch, len(pfiles), cmt.parentBranch)
		for _, f := range pfiles {
			g.filesOnBranch[cmt.branch].AddFile(f)
		}
	}
	newfiles := make([]FileAction, 0)
	node := g.filesOnBranch[cmt.branch]
	// Phase 1 - expand deletes/renames/copies of directories to individual commands
	//    May ignore files, eg. delete of a file which doesn't exist (or an empty dir)
	for i := range cmt.files {
		gf := cmt.files[i]
		if gf.action == modify {
			newfiles = append(newfiles, gf)
		} else if gf.action == delete {
			if node.FindFile(gf.name) { // Single file found which we can delete
				newfiles = append(newfiles, gf)
				continue
			}
			files := node.GetFiles(gf.name)
			if len(files) > 0 {
				g.logger.Debugf("DirDelete: Path:%s", gf.name)
				for _, df := range files {
					if !hasPrefix(df, gf.name) {
						g.logger.Errorf("Unexpected path found: %s: %s", gf.name, df)
						continue
					}
					g.logger.Debugf("DirFileDelete: %s Path:%s", cmt.ref(), df)
					newfiles = append(newfiles, FileAction{name: df, action: delete})
				}
			} else {
				g.logger.Debugf("DeleteIgnored: %s Path:%s", cmt.ref(), gf.name)
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
						newfiles = append(newfiles, FileAction{name: dest, srcName: rf, action: rename})
					}
				}
			} else {
				// Handle the rare case where a directory rename is followed by individual file renames (so a double rename!)
				doubleRename := false
				var dupGf FileAction
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
					newfiles = append(newfiles, FileAction{name: dest, srcName: rf, action: copy})
				}
			} else {
				g.logger.Debugf("CopyIgnored: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
			}
		} else {
			g.logger.Errorf("Unexpected GFAction: GitFile: %s %v", cmt.ref(), gf.name, gf.action)
		}
	}
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

func (g *GitFilter) processCommit(cmt *MyCommit, backend *libfastimport.Backend, filteringPaths bool, rePathFilter regexp.Regexp) {
	if cmt == nil {
		return
	}
	for i := range cmt.files {
		gf := cmt.files[i]
		switch gf.action {
		case modify:
			if filteringPaths {
				if rePathFilter.MatchString(string(gf.name)) {
					g.logger.Infof("FileModify: %s %+v", cmt.ref(), gf)
					cmd := libfastimport.FileModify{Path: libfastimport.Path(gf.name), Mode: gf.mode, DataRef: gf.dataRef}
					backend.Do(cmd)
					if gf.dataRef != "" {
						oid, err := getOID(gf.dataRef)
						if err == nil {
							g.blobsFound[oid] = 1
						} else {
							g.logger.Errorf("Failed to extract Dataref: %+v", gf)
						}
					}
				}
			} else {
				cmd := libfastimport.FileModify{Path: libfastimport.Path(gf.name), Mode: gf.mode, DataRef: gf.dataRef}
				backend.Do(cmd)
			}
		case delete:
			if filteringPaths {
				if rePathFilter.MatchString(string(gf.name)) {
					g.logger.Infof("FileDelete: %s %+v", cmt.ref(), gf)
					cmd := libfastimport.FileDelete{Path: libfastimport.Path(gf.name)}
					backend.Do(cmd)
					if gf.dataRef != "" {
						oid, err := getOID(gf.dataRef)
						if err == nil {
							g.blobsFound[oid] = 1
						} else {
							g.logger.Errorf("Failed to extract Dataref: %+v", gf)
						}
					}
				}
			} else {
				cmd := libfastimport.FileDelete{Path: libfastimport.Path(gf.name)}
				backend.Do(cmd)
			}
		case copy:
			if filteringPaths {
				if rePathFilter.MatchString(gf.name) || rePathFilter.MatchString(gf.srcName) {
					g.logger.Infof("FileCopy: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
					cmd := libfastimport.FileCopy{Src: libfastimport.Path(gf.srcName), Dst: libfastimport.Path(gf.name)}
					backend.Do(cmd)
				}
			} else {
				cmd := libfastimport.FileCopy{Src: libfastimport.Path(gf.srcName), Dst: libfastimport.Path(gf.name)}
				backend.Do(cmd)
			}
		case rename:
			if filteringPaths {
				if rePathFilter.MatchString(gf.name) || rePathFilter.MatchString(gf.srcName) {
					g.logger.Infof("FileRename: %s Src:%s Dst:%s", cmt.ref(), gf.srcName, gf.name)
					cmd := libfastimport.FileRename{Src: libfastimport.Path(gf.srcName), Dst: libfastimport.Path(gf.name)}
					backend.Do(cmd)
				}
			} else {
				cmd := libfastimport.FileRename{Src: libfastimport.Path(gf.srcName), Dst: libfastimport.Path(gf.name)}
				backend.Do(cmd)
			}
		}
	}
}

// RunGitFilter - filters git export file
func (g *GitFilter) RunGitFilter(options GitFilterOptions) {
	var inbuf io.Reader
	var infile *os.File
	var err error
	var commitMap *CommitMap

	if g.testInput != "" {
		inbuf = strings.NewReader(g.testInput)
	} else {
		infile, err = os.Open(options.gitImportFile) // Note deferred close below.
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", options.gitImportFile, err)
			os.Exit(1)
		}
		inbuf = bufio.NewReader(infile)
	}

	g.opts = options
	var rePathFilter *regexp.Regexp
	filteringPaths := false
	if g.opts.pathFilter != "" {
		rePathFilter = regexp.MustCompile(g.opts.pathFilter)
		filteringPaths = true
	}

	if filteringPaths {
		commitMap = g.filterCommits(options, rePathFilter)
	}

	outpath := options.gitExportFile
	if filteringPaths {
		outpath = fmt.Sprintf("%s_", outpath)
	}
	outfile, err := os.Create(outpath)
	if err != nil {
		panic(err) // Handle error
	}
	mwc := &MyWriteCloser{outfile, bufio.NewWriter(outfile)}
	defer mwc.Close()
	defer infile.Close()

	g.blobsFound = map[int]int{}

	var currCommit *MyCommit

	g.filesOnBranch = make(map[string]*node.Node) // Records current state of git tree per branch
	frontend := libfastimport.NewFrontend(inbuf, nil, nil)
	backend := libfastimport.NewBackend(mwc, nil, nil)
	commitCount := 0
	commitFiltered := false
	var currReset libfastimport.CmdReset
CmdLoop:
	for {
		cmd, err := frontend.ReadCmd()
		if err != nil {
			if err != io.EOF {
				g.logger.Errorf("ERROR: Failed to read cmd: %v", err)
			}
			break
		}
		switch ctype := cmd.(type) {
		case libfastimport.CmdBlob:
			blob := cmd.(libfastimport.CmdBlob)
			if !filteringPaths {
				g.logger.Debugf("Blob: Mark:%d OriginalOID:%s Size:%s", blob.Mark, blob.OriginalOID, Humanize(len(blob.Data)))
				blob.Data = fmt.Sprintf("%d\n", blob.Mark) // Filter out blob contents
				backend.Do(blob)
			}

		case libfastimport.CmdReset:
			// Only output if commit not filtered
			currReset := cmd.(libfastimport.CmdReset)
			if options.renameRefs {
				currReset.RefName = strings.ReplaceAll(currReset.RefName, " ", "_")
			}

		case libfastimport.CmdCommit:
			g.validateCommit(currCommit)
			g.processCommit(currCommit, backend, filteringPaths, *rePathFilter)
			commit := cmd.(libfastimport.CmdCommit)
			if commit.Msg[len(commit.Msg)-1] != '\n' {
				commit.Msg += "\n"
			}
			if options.renameRefs {
				commit.Ref = strings.ReplaceAll(commit.Ref, " ", "_")
			}
			currCommit = &MyCommit{commit: &commit, files: make([]FileAction, 0), mergeBranch: make([]string, 0)}
			commitFiltered = false
			if g.opts.filterCommits {
				if cmt, ok := (*commitMap)[commit.Mark]; ok {
					currCommit.branch = cmt.branch
					currCommit.mergeBranch = cmt.mergeBranch
					if cmt.fileCount > 0 || cmt.mergeCount > 0 || cmt.branch != cmt.parentBranch {
						g.logger.Debugf("Reset: - %+v", currReset)
						backend.Do(currReset)
						// Reset From to ignore any filtered parents
						commit.From = g.findUnfilteredParent(commitMap, commit.From)
						backend.Do(commit)
					} else {
						commitFiltered = true
						cmt.filtered = true
						g.logger.Debugf("FilteredCommit:  %+v", commit)
					}
				} else {
					g.logger.Errorf("Couldn't find Commit: %d", commit.Mark)
				}
			} else {
				g.logger.Debugf("Reset: - %+v", currReset)
				backend.Do(currReset)
				backend.Do(commit)
			}
			if !commitFiltered {
				g.logger.Debugf("Commit:  %+v", commit)
			}

		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			if !commitFiltered {
				g.logger.Debugf("FilteredCommitEnd: %+v", commit)
				backend.Do(cmd)
			} else {
				g.logger.Debugf("CommitEnd: %+v", commit)
			}
			commitCount += 1
			if g.opts.maxCommits > 0 && commitCount >= g.opts.maxCommits {
				g.logger.Infof("Processed %d commits", commitCount)
				break CmdLoop
			}

		case libfastimport.FileModify:
			fm := cmd.(libfastimport.FileModify)
			currCommit.files = append(currCommit.files, FileAction{action: modify, name: fm.Path.String(), mode: fm.Mode, dataRef: fm.DataRef})

		case libfastimport.FileDelete:
			fdel := cmd.(libfastimport.FileDelete)
			currCommit.files = append(currCommit.files, FileAction{action: delete, name: fdel.Path.String()})

		case libfastimport.FileCopy:
			fc := cmd.(libfastimport.FileCopy)
			currCommit.files = append(currCommit.files, FileAction{action: copy, name: fc.Dst.String(), srcName: fc.Src.String()})

		case libfastimport.FileRename:
			fr := cmd.(libfastimport.FileRename)
			currCommit.files = append(currCommit.files, FileAction{action: rename, name: fr.Dst.String(), srcName: fr.Src.String()})

		case libfastimport.CmdTag:
			t := cmd.(libfastimport.CmdTag)
			g.logger.Debugf("CmdTag: %+v", t)
			if options.renameRefs {
				t.RefName = strings.ReplaceAll(t.RefName, " ", "_")
			}
			backend.Do(t)

		default:
			g.logger.Errorf("Not handled - found ctype %s cmd %+v", ctype, cmd)
			g.logger.Errorf("Cmd type %T", cmd)
		}
	}
	// Save referenced blobs in a new file which will be concatenated with the original
	if filteringPaths {
		outfile, err := os.Create(options.gitExportFile)
		if err != nil {
			panic(err) // Handle error
		}
		mwcBlob := &MyWriteCloser{outfile, bufio.NewWriter(outfile)}
		defer mwcBlob.Close()
		keys := make([]int, 0, len(g.blobsFound))
		for k := range g.blobsFound {
			keys = append(keys, k)
		}
		backendBlob := libfastimport.NewBackend(mwcBlob, nil, nil)
		sort.Ints(keys)
		var blob libfastimport.CmdBlob
		for _, k := range keys {
			blob.Mark = k
			blob.Data = fmt.Sprintf("%d\n", blob.Mark)
			backendBlob.Do(blob)
		}
	}
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
		gitexport = kingpin.Arg(
			"gitexport",
			"Git fast-import file to write.",
		).String()
		renameRefs = kingpin.Flag(
			"rename",
			"Enable debugging level.",
		).Short('r').Bool()
		filterCommits = kingpin.Flag(
			"filter.commits",
			"Filter out empty commits (if --path.filter defined).",
		).Short('f').Bool()
		maxCommits = kingpin.Flag(
			"max.commits",
			"Max no of commits to process.",
		).Short('m').Int()
		pathFilter = kingpin.Flag(
			"path.filter",
			"Regex git path to filter output by.",
		).String()
		debug = kingpin.Flag(
			"debug",
			"Enable debugging level.",
		).Int()
	)
	kingpin.UsageTemplate(kingpin.CompactUsageTemplate).Version(version.Print("GitFilter")).Author("Robert Cowham")
	kingpin.CommandLine.Help = "Parses one or more git fast-export files to filter blob contents and write a new one\n"
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *debug > 0 {
		logger.Level = logrus.DebugLevel
	}
	startTime := time.Now()
	logger.Infof("%v", version.Print("gitfilter"))
	if *filterCommits && *pathFilter == "" {
		logger.Fatalf("Please only specify -f/--filter.commits if also specifying --path.filter value")
	}

	logger.Infof("Starting %s, gitimport: %v", startTime, *gitimport)

	g := NewGitFilter(logger)
	opts := GitFilterOptions{
		gitImportFile: *gitimport,
		gitExportFile: *gitexport,
		renameRefs:    *renameRefs,
		filterCommits: *filterCommits,
		maxCommits:    *maxCommits,
		pathFilter:    *pathFilter,
	}
	logger.Infof("Options: %+v", opts)

	g.RunGitFilter(opts)
	if opts.pathFilter != "" {
		err := appendfile(fmt.Sprintf("%s_", opts.gitExportFile), opts.gitExportFile)
		if err != nil {
			logger.Errorf("Failed to write %s: %v", opts.gitExportFile, err)
		}
	}
	logger.Infof("Output file: %s", opts.gitExportFile)

}
