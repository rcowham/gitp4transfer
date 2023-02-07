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
	pathFilter    string // Regex to filter output only to specified files
	maxCommits    int    // Max number of commits to process
}

// GitFilter - filter a git fast-export file
type GitFilter struct {
	logger    *logrus.Logger
	opts      GitFilterOptions
	testInput string // For testing only
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

// // FilterGitCommit - A git commit
// type FilterGitCommit struct {
// 	commit *libfastimport.CmdCommit
// }

// RunGitFilter - returns channel which contains commits with associated files.
func (g *GitFilter) RunGitFilter(options GitFilterOptions) {
	var inbuf io.Reader
	var infile *os.File
	// var outfile *os.File
	var err error

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

	blobsFound := map[int]int{}

	// var currCommit *FilterGitCommit

	frontend := libfastimport.NewFrontend(inbuf, nil, nil)
	backend := libfastimport.NewBackend(mwc, nil, nil)
	commitCount := 0
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
			reset := cmd.(libfastimport.CmdReset)
			g.logger.Debugf("Reset: - %+v", reset)
			if options.renameRefs {
				reset.RefName = strings.ReplaceAll(reset.RefName, " ", "_")
			}
			backend.Do(reset)

		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Debugf("Commit:  %+v", commit)
			if commit.Msg[len(commit.Msg)-1] != '\n' {
				commit.Msg += "\n"
			}
			if options.renameRefs {
				commit.Ref = strings.ReplaceAll(commit.Ref, " ", "_")
			}
			// if currCommit == nil {
			// 	currCommit = &FilterGitCommit{commit: &commit}
			// }
			// if !filteringPaths {
			backend.Do(commit)
			// }

		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Debugf("CommitEnd: %+v", commit)
			backend.Do(cmd)
			commitCount += 1
			if g.opts.maxCommits > 0 && commitCount >= g.opts.maxCommits {
				g.logger.Infof("Processed %d commits", commitCount)
				break CmdLoop
			}

		case libfastimport.FileModify:
			fm := cmd.(libfastimport.FileModify)
			if filteringPaths {
				if rePathFilter.MatchString(string(fm.Path)) {
					g.logger.Debugf("FileModify: %+v", fm)
					backend.Do(cmd)
					if fm.DataRef != "" {
						oid, err := getOID(fm.DataRef)
						if err == nil {
							blobsFound[oid] = 1
						} else {
							g.logger.Errorf("Failed to extract Dataref: %+v", fm)
						}
					}
				} else {
					g.logger.Debugf("Filtered FileModify: %+v", fm)
				}
			} else {
				g.logger.Debugf("FileModify: %+v", fm)
				backend.Do(cmd)
			}

		case libfastimport.FileDelete:
			fdel := cmd.(libfastimport.FileDelete)
			if filteringPaths {
				if rePathFilter.MatchString(string(fdel.Path)) {
					g.logger.Debugf("FileDelete: Path:%s", fdel.Path)
					backend.Do(fdel)
				} else {
					g.logger.Debugf("Filtered FileDelete: Path:%s", fdel.Path)
				}
			} else {
				g.logger.Debugf("FileDelete: Path:%s", fdel.Path)
				backend.Do(fdel)
			}

		case libfastimport.FileCopy:
			fc := cmd.(libfastimport.FileCopy)
			if filteringPaths {
				if rePathFilter.MatchString(string(fc.Src)) || rePathFilter.MatchString(string(fc.Dst)) {
					g.logger.Debugf("FileCopy: Src:%s Dst:%s", fc.Src, fc.Dst)
					backend.Do(fc)
				} else {
					g.logger.Debugf("Filtered FileCopy: Src:%s Dst:%s", fc.Src, fc.Dst)
				}
			} else {
				g.logger.Debugf("FileCopy: Src:%s Dst:%s", fc.Src, fc.Dst)
				backend.Do(fc)
			}

		case libfastimport.FileRename:
			fr := cmd.(libfastimport.FileRename)
			if filteringPaths {
				if rePathFilter.MatchString(string(fr.Src)) || rePathFilter.MatchString(string(fr.Dst)) {
					g.logger.Debugf("FileRename: Src:%s Dst:%s", fr.Src, fr.Dst)
					backend.Do(fr)
				} else {
					g.logger.Debugf("Filtered FileRename: Src:%s Dst:%s", fr.Src, fr.Dst)
				}
			} else {
				g.logger.Debugf("FileRename: Src:%s Dst:%s", fr.Src, fr.Dst)
				backend.Do(fr)
			}

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
		keys := make([]int, 0, len(blobsFound))
		for k := range blobsFound {
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
	kingpin.CommandLine.Help = "Parses one or more git fast-export files to filter blobl contents and write a new one\n"
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *debug > 0 {
		logger.Level = logrus.DebugLevel
	}
	startTime := time.Now()
	logger.Infof("%v", version.Print("gitfilter"))
	logger.Infof("Starting %s, gitimport: %v", startTime, *gitimport)

	g := NewGitFilter(logger)
	opts := GitFilterOptions{
		gitImportFile: *gitimport,
		gitExportFile: *gitexport,
		renameRefs:    *renameRefs,
		maxCommits:    *maxCommits,
		pathFilter:    *pathFilter,
	}

	g.RunGitFilter(opts)
	if opts.pathFilter != "" {
		err := appendfile(fmt.Sprintf("%s_", opts.gitExportFile), opts.gitExportFile)
		if err != nil {
			logger.Errorf("Failed to write %s: %v", opts.gitExportFile, err)
		}
	}
	logger.Infof("Output file: %s", opts.gitExportFile)

}
