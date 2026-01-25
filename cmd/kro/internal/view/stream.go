// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package view

import (
	"fmt"
	"io"

	"github.com/kubernetes-sigs/kro/cmd/kro/version"
)

// Stream provides basic output operations wrapping an io.Writer.
type Stream struct {
	Writer io.Writer
}

// NewStream creates a Stream writing to the provided io.Writer.
func NewStream(w io.Writer) *Stream {
	return &Stream{
		Writer: w,
	}
}

// Println writes arguments to the stream with a newline.
func (s *Stream) Println(args ...any) {
	fmt.Fprintln(s.Writer, args...)
}

// Printf writes formatted output to the stream.
func (s *Stream) Printf(fmtStr string, args ...any) {
	fmt.Fprintf(s.Writer, fmtStr, args...)
}

// PrintVersion writes version information to the stream.
func (s *Stream) PrintVersion() {
	version.Fprint(s.Writer)
}
