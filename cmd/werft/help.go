package main

import (
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hk9890/werft/internal/failure"
)

func (c *cli) cmdHelp(args []string) error {
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	if err := c.parseFlags(fs, "help", args); err != nil {
		return err
	}
	switch fs.NArg() {
	case 0:
		_, _ = fmt.Fprint(c.stdout, usage())
		return nil
	case 1:
		cmd, ok := lookup(fs.Arg(0))
		if !ok {
			return failure.New(failure.Usage, "unknown command %q; run 'werft help'", fs.Arg(0))
		}
		return cmd.run(c, []string{"--help"})
	}
	return failure.New(failure.Usage, "help takes at most one command")
}

func usage() string {
	var b strings.Builder
	b.WriteString("werft - disposable test VMs from stock images\n\n")
	b.WriteString("usage: werft [--json] <command> [arguments]\n\ncommands:\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "  %-24s %s\n", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.summary)
	}
	b.WriteString("\nglobal flags:\n")
	b.WriteString("  --json                   print one JSON document on stdout; on error\n")
	b.WriteString("                           {\"error\": {\"kind\": ..., \"message\": ...}}\n")
	b.WriteString("\nexit codes:\n")
	byCode := map[int]failure.Kind{0: "ok"}
	for kind, code := range exitCodes {
		byCode[code] = kind
	}
	for _, code := range slices.Sorted(maps.Keys(byCode)) {
		fmt.Fprintf(&b, "  %-3d %s\n", code, byCode[code])
	}
	return b.String()
}

func commandUsage(cmd command, fs *flag.FlagSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: werft %s\n\n%s\n", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.summary)
	var flags strings.Builder
	fs.SetOutput(&flags)
	fs.PrintDefaults()
	if flags.Len() > 0 {
		b.WriteString("\nflags:\n")
		b.WriteString(flags.String())
	}
	return b.String()
}
