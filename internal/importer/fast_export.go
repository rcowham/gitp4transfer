package importer

import (
	"errors"
	"io"

	libfastimport "github.com/rcowham/go-libgitfastimport"
)

// ErrStop stops processing a source without treating it as a failure.
var ErrStop = errors.New("stop source processing")

// CommandSource produces commands from an import source.
type CommandSource interface {
	Next() (libfastimport.Cmd, error)
}

// FastExportSource reads records from a git fast-export stream.
type FastExportSource struct {
	frontend *libfastimport.Frontend
}

// NewFastExportSource creates a source backed by a git fast-export stream.
func NewFastExportSource(input io.Reader) *FastExportSource {
	return &FastExportSource{frontend: libfastimport.NewFrontend(input, nil, nil)}
}

// Next returns the next command in the stream, or io.EOF at end of input.
func (s *FastExportSource) Next() (libfastimport.Cmd, error) {
	return s.frontend.ReadCmd()
}

// Process invokes handle for each command until end of source or ErrStop.
func Process(source CommandSource, handle func(libfastimport.Cmd) error) error {
	for {
		cmd, err := source.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := handle(cmd); err != nil {
			if errors.Is(err, ErrStop) {
				return nil
			}
			return err
		}
	}
}
