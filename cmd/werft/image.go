package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/state"
)

func (c *cli) cmdSeal(args []string) error {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "reset the build VM to the label without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	if err := c.parseFlags(fs, "seal", args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return failure.New(failure.Usage, "seal takes a build VM name and a label")
	}
	name, label := fs.Arg(0), fs.Arg(1)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	h, done, err := c.lockedHypervisor(name)
	if err != nil {
		return err
	}
	defer done()
	s, err := image.Plan(h, name, label)
	if err != nil {
		return err
	}
	if err := c.confirm(yes, fmt.Sprintf("Seal resets %s to '%s'. Everything written since is lost. Seal it?", name, label)); err != nil {
		return err
	}
	shell, key, cleanup, err := rootShell()
	if err != nil {
		return err
	}
	defer cleanup()
	u, err := user.Current()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec, err := s.Run(image.Env{
		Ctx: ctx, H: h, Shell: shell, Key: key,
		Pool: cfg.Pool, PoolPath: cfg.PoolPath(), Catalog: dir.Images(),
		User: u.Username, WerftKey: publicKey(dir.SSHKey() + ".pub"),
		Global: func() (func(), error) {
			lock, err := dir.LockGlobal()
			if err != nil {
				return nil, err
			}
			return func() { _ = lock.Release() }, nil
		},
		Now: time.Now, Out: c.stderr,
	})
	if err != nil {
		return err
	}
	c.output(rec, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s sealed: image %s, tier %s, version %s\n", rec.Name, rec.Image, rec.Tier, rec.Version)
	})
	return nil
}

// publicKey is the type and base64 of the public key at path, without the
// comment; empty when there is no key.
func publicKey(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return ""
	}
	return fields[0] + " " + fields[1]
}

func (c *cli) cmdImages(args []string) error {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	if err := c.parseFlags(fs, "images", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return failure.New(failure.Usage, "images takes no arguments")
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	records, err := image.Load(dir.Images())
	if err != nil {
		return err
	}
	if records == nil {
		records = []image.Record{}
	}
	c.output(records, func(w io.Writer) {
		if len(records) == 0 {
			_, _ = fmt.Fprintln(w, "no stock images; werft seal <build VM> provision-base makes one")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tIMAGE\tTIER\tVERSION\tDATE\tUSER")
		for _, r := range records {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Image, r.Tier, r.Version, r.Date.Local().Format("2006-01-02 15:04"), r.User)
		}
		_ = tw.Flush()
	})
	return nil
}
