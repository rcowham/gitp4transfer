package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	libfastimport "github.com/rcowham/go-libgitfastimport"

	"github.com/rcowham/p4training/version"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
)

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
	var err error
	var (
		gitimport = kingpin.Arg(
			"gitimport",
			"Git fast-export files to process.").String()
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

	file, err := os.Open(*gitimport)
	if err != nil {
		fmt.Printf("ERROR: Failed to open file '%s': %v\n", *gitimport, err)
		os.Exit(1)
	}

	buf := bufio.NewReader(file)

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
			fmt.Printf("Blob: Mark:%d OriginalOID:%s\n", blob.Mark, blob.OriginalOID)
		case libfastimport.CmdReset:
			reset := cmd.(libfastimport.CmdReset)
			fmt.Printf("Reset: - %+v\n", reset)
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			fmt.Printf("Commit:  %+v\n", commit)
		case libfastimport.CmdCommitEnd:
			commit := cmd.(libfastimport.CmdCommitEnd)
			fmt.Printf("CommitEnd:  %+v\n", commit)
		case libfastimport.FileModify:
			f := cmd.(libfastimport.FileModify)
			fmt.Printf("FileModify:  %+v\n", f)
		case libfastimport.FileDelete:
			f := cmd.(libfastimport.FileDelete)
			fmt.Printf("FileModify: Path:%s\n", f.Path)
		case libfastimport.FileCopy:
			f := cmd.(libfastimport.FileCopy)
			fmt.Printf("FileCopy: Src:%s Dst:%s\n", f.Src, f.Dst)
		case libfastimport.FileRename:
			f := cmd.(libfastimport.FileRename)
			fmt.Printf("FileRename: Src:%s Dst:%s\n", f.Src, f.Dst)
		default:
			fmt.Printf("Not handled\n")
			fmt.Printf("Found cmd %v\n", cmd)
			fmt.Printf("Cmd type %T\n", cmd)
		}
	}

}
