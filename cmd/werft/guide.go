package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"

	"github.com/hk9890/werft/internal/failure"
)

// guide tells an agent how to lease a VM, use it and give it back. It is
// embedded, so it matches the installed binary.
//
//go:embed guide.txt
var guide string

func (c *cli) cmdGuide(args []string) error {
	fs := flag.NewFlagSet("guide", flag.ContinueOnError)
	if err := c.parseFlags(fs, "guide", args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return failure.New(failure.Usage, "guide takes no arguments")
	}
	c.output(map[string]string{"guide": guide}, func(w io.Writer) { _, _ = fmt.Fprint(w, guide) })
	return nil
}
