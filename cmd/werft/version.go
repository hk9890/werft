package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/hk9890/werft/internal/build"
	"github.com/hk9890/werft/internal/failure"
)

func (c *cli) cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := c.parseFlags(fs, "version", args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return failure.New(failure.Usage, "version takes no arguments")
	}
	c.output(map[string]string{"version": build.Version, "commit": build.Commit, "date": build.Date}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "werft %s (%s, %s)\n", build.Version, build.Commit, build.Date)
	})
	return nil
}
