package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
)

// leasePrefix starts the name of every lease domain. Live tests change it,
// so their leases carry the prefix of a test domain.
var leasePrefix = "werft-"

func (c *cli) cmdLease(args []string) error {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	tier := fs.String("tier", image.Plain, "the tier of the stock image: plain or full")
	owner := fs.String("owner", "", "the owner werft leases shows (default $WERFT_OWNER, else $USER)")
	view := fs.Bool("view", false, "also open a virt-viewer window")
	if err := c.parseFlags(fs, "lease", args); err != nil {
		return err
	}
	// The flags may also follow the image: werft lease ubuntu-26-04 --tier full.
	var img string
	if fs.NArg() > 0 {
		img = fs.Arg(0)
		if err := c.parseFlags(fs, "lease", fs.Args()[1:]); err != nil {
			return err
		}
	}
	if img == "" || fs.NArg() != 0 {
		return failure.New(failure.Usage, "lease takes one image name; werft images lists them")
	}
	if *tier != image.Plain && *tier != "full" {
		return failure.New(failure.Usage, "--tier is plain or full, not %q", *tier)
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	rec, err := lease.Resolve(dir.Images(), img, *tier)
	if err != nil {
		return err
	}
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := lease.Env{
		Ctx: ctx, H: h, Dir: dir, Prefix: leasePrefix,
		Shell:  func(config string) guest.Shell { return guest.OpenSSH{Config: config} },
		NewKey: guestEnsureKey,
		Global: func() (func(), error) {
			lock, err := dir.LockGlobal()
			if err != nil {
				return nil, err
			}
			return func() { _ = lock.Release() }, nil
		},
		Now: time.Now, Sleep: c.sleep, Out: c.stderr,
	}
	if *view {
		env.View = func(domain string) {
			if err := c.openViewer(domain); err != nil {
				c.progress("warning: no viewer window: %v; open one on the desktop: %s", err, steps.ViewerCommand(domain))
			}
		}
	}
	l, err := lease.Start(env, rec, leaseOwner(*owner))
	if err != nil {
		return err
	}
	c.output(l, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "lease %s: %s from %s, user %s at %s, until %s\n%s\n",
			l.ID, l.Domain, l.Stock, l.User, l.Address, l.Expiry.Local().Format("2006-01-02 15:04"), l.SSH)
	})
	return nil
}

// leaseOwner is flag, else $WERFT_OWNER, else $USER. The owner is a label:
// release does not check it.
func leaseOwner(flag string) string {
	for _, v := range []string{flag, os.Getenv("WERFT_OWNER"), os.Getenv("USER")} {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *cli) cmdRelease(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	if err := c.parseFlags(fs, "release", args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return failure.New(failure.Usage, "release takes one lease id")
	}
	id := fs.Arg(0)
	dir, err := state.Root()
	if err != nil {
		return err
	}
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	domain, err := lease.Release(h, dir, leasePrefix, id)
	if err != nil {
		return err
	}
	c.output(map[string]string{"id": id, "domain": domain}, func(w io.Writer) {
		if domain == "" {
			_, _ = fmt.Fprintf(w, "lease %s is gone already\n", id)
			return
		}
		_, _ = fmt.Fprintf(w, "lease %s released: %s is destroyed\n", id, domain)
	})
	return nil
}
