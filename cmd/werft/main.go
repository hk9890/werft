// Command werft hands out disposable test VMs built from stock images.
package main

import (
	"fmt"
	"os"

	"github.com/hk9890/werft/internal/build"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("werft %s (%s, %s)\n", build.Version, build.Commit, build.Date)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: werft version")
	os.Exit(2)
}
