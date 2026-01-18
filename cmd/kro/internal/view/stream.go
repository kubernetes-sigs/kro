package view

import (
	"fmt"
	"io"

	"github.com/kubernetes-sigs/kro/cmd/kro/version"
)

type Stream struct {
	Writer io.Writer
}

func NewStream(w io.Writer) *Stream {
	return &Stream{
		Writer: w,
	}
}

func (s *Stream) Println(args ...any) {
	fmt.Fprintln(s.Writer, args...)
}

func (s *Stream) Printf(fmtStr string, args ...any) {
	fmt.Fprintf(s.Writer, fmtStr, args...)
}

func (s *Stream) PrintVersion() {
	version.Fprint(s.Writer)
}
