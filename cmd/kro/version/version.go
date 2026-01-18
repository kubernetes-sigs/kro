package version

import (
	_ "embed"
	"fmt"
	"io"
	"runtime"
)

var (
	Version string = "dev"
)

func Print() {
	fmt.Printf("kro version %s\n", Version)
	fmt.Printf("%s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func Fprint(w io.Writer) {
	fmt.Fprintf(w, "kro version %s\n", Version)
	fmt.Fprintf(w, "%s/%s\n", runtime.GOOS, runtime.GOARCH)
}
