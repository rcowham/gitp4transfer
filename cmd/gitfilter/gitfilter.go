package main

// Filters git fast-import files - removing contents of blobs (converting them to unique integers) - but
// otherwise keeping structure the same.

import (
	"bufio"
	"fmt"
	"io"               // profiling only
	_ "net/http/pprof" // profiling only
	"os"
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

type GitFilterOptions struct {
	gitImportFile string
	gitExportFile string
	renameRefs    bool
}

// GitFilter - Transfer via git fast-export file
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
	outfile, err := os.Create(options.gitExportFile)
	if err != nil {
		panic(err) // Handle error
	}

	mwc := &MyWriteCloser{outfile, bufio.NewWriter(outfile)}
	defer mwc.Close()
	defer infile.Close()

	g.opts = options

	frontend := libfastimport.NewFrontend(inbuf, nil, nil)
	backend := libfastimport.NewBackend(mwc, nil, nil)
	for {
		cmd, err := frontend.ReadCmd()
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
			blob.Data = fmt.Sprintf("%d\n", blob.Mark) // Filter out blob contents
			backend.Do(blob)

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
			backend.Do(commit)

		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			g.logger.Debugf("CommitEnd: %+v", commit)
			backend.Do(cmd)

		case libfastimport.FileModify:
			fm := cmd.(libfastimport.FileModify)
			g.logger.Debugf("FileModify: %+v", fm)
			backend.Do(cmd)

		case libfastimport.FileDelete:
			fdel := cmd.(libfastimport.FileDelete)
			g.logger.Debugf("FileDelete: Path:%s", fdel.Path)
			backend.Do(fdel)

		case libfastimport.FileCopy:
			fc := cmd.(libfastimport.FileCopy)
			g.logger.Debugf("FileCopy: Src:%s Dst:%s", fc.Src, fc.Dst)
			backend.Do(fc)

		case libfastimport.FileRename:
			fr := cmd.(libfastimport.FileRename)
			g.logger.Debugf("FileRename: Src:%s Dst:%s", fr.Src, fr.Dst)
			backend.Do(fr)

		case libfastimport.CmdTag:
			t := cmd.(libfastimport.CmdTag)
			g.logger.Debugf("CmdTag: %+v", t)
			backend.Do(t)

		default:
			g.logger.Errorf("Not handled: Found cmd %+v", cmd)
			g.logger.Errorf("Cmd type %T", cmd)
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
	}

	g.RunGitFilter(opts)

}
