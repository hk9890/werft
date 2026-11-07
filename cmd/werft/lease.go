package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

// leasePrefix starts the name of every lease domain. Live tests change it,
// so their leases carry the prefix of a test domain.
var leasePrefix = "werft-"

// clock is the time leases are made, extended and reaped at. Live tests
// move it forward, so a lease expires without a wait of hours.
var clock = time.Now

// leaseEnv is what the lease commands run with: the real SSH, keys and
// global lock.
func (c *cli) leaseEnv(h virt.Hypervisor, dir state.Dir) lease.Env {
	return lease.Env{
		Ctx: context.Background(), H: h, Dir: dir, Prefix: leasePrefix,
		Shell:  func(config string) guest.Shell { return guest.OpenSSH{Config: config} },
		NewKey: guestEnsureKey,
		Global: func() (func(), error) {
			lock, err := dir.LockGlobal()
			if err != nil {
				return nil, err
			}
			return func() { _ = lock.Release() }, nil
		},
		Now: clock, Sleep: c.sleep, Out: c.stderr,
	}
}

// reapFirst is the reap pass before a command that reads leases. A reap
// that fails does not stop the command: it warns, and the next command or
// werft reap tries again.
func (c *cli) reapFirst(e lease.Env) {
	if _, err := lease.Reap(e); err != nil {
		c.progress("warning: %v; werft reap tries again", err)
	}
}

func (c *cli) cmdLease(args []string) error {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	tier := fs.String("tier", image.Plain, "the tier of the stock image: plain or full; with --stock, the tier of that version")
	owner := fs.String("owner", "", "the owner werft leases shows (default $WERFT_OWNER, else $USER)")
	view := fs.Bool("view", false, "also open a virt-viewer window")
	stock := fs.String("stock", "", "the stock version, such as ubuntu-26-04-plain-20261102 (default the newest)")
	memory := fs.Uint64("memory", 0, "the memory of the VM in MiB, at least 256 (default that of the stock image)")
	cpus := fs.Uint("cpus", 0, "the vCPUs of the VM (default those of the stock image)")
	lifetime := fs.Duration("for", lease.Lifetime, "the lifetime of the lease, from 15m to 24h")
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
	if *tier != image.Plain && *tier != image.Full {
		return failure.New(failure.Usage, "--tier is plain or full, not %q", *tier)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["memory"] && (*memory < lease.MinMemory>>20 || *memory > math.MaxUint64>>20) {
		return failure.New(failure.Usage, "--memory is a size in MiB, at least %d, not %d", lease.MinMemory>>20, *memory)
	}
	if set["cpus"] && *cpus == 0 {
		return failure.New(failure.Usage, "--cpus is at least 1")
	}
	if *lifetime < lease.MinLifetime || *lifetime > lease.MaxLifetime {
		return failure.New(failure.Usage, "--for is a duration from 15m to 24h, such as 2h, not %v", *lifetime)
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	if *stock != "" && !set["tier"] {
		*tier = ""
	}
	rec, err := lease.Resolve(dir.Images(), img, *tier, *stock)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
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
	env := c.leaseEnv(h, dir)
	env.Ctx = ctx
	env.MemoryBudget = cfg.MemoryBudgetMiB << 20
	c.reapFirst(env)
	if *view {
		env.View = func(domain string) {
			if err := c.openViewer(domain); err != nil {
				c.progress("warning: no viewer window: %v; open one on the desktop: %s", err, steps.ViewerCommand(domain))
			}
		}
	}
	l, err := lease.Start(env, rec, lease.Options{Owner: leaseOwner(*owner), Memory: *memory << 20, CPUs: *cpus, Lifetime: *lifetime})
	if err != nil {
		return err
	}
	c.output(l, func(w io.Writer) { writeLease(w, l) })
	return nil
}

// writeLease is the text output of werft lease: every field of the JSON
// document, then the ssh command alone on the last line.
func writeLease(w io.Writer, l lease.Lease) {
	const when = "2006-01-02 15:04"
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range [][2]string{
		{"lease", l.ID}, {"domain", l.Domain}, {"image", l.Image}, {"tier", l.Tier}, {"stock", l.Stock}, {"owner", l.Owner},
		{"user", l.User}, {"address", l.Address}, {"key", l.Key}, {"ssh_config", l.SSHConfig},
		{"created", l.Created.Local().Format(when)}, {"expiry", l.Expiry.Local().Format(when)},
	} {
		_, _ = fmt.Fprintf(tw, "%s:\t%s\n", row[0], row[1])
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w, l.SSH)
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
	e, done, err := c.leaseHost()
	if err != nil {
		return err
	}
	defer done()
	c.reapFirst(e)
	domain, err := lease.Release(e.H, e.Dir, e.Prefix, id)
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

func (c *cli) cmdLeases(args []string) error {
	fs := flag.NewFlagSet("leases", flag.ContinueOnError)
	if err := c.parseFlags(fs, "leases", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return failure.New(failure.Usage, "leases takes no arguments")
	}
	e, done, err := c.leaseHost()
	if err != nil {
		return err
	}
	defer done()
	c.reapFirst(e)
	leases, err := lease.List(e)
	if err != nil {
		return err
	}
	c.output(leases, func(w io.Writer) {
		if len(leases) == 0 {
			_, _ = fmt.Fprintln(w, "no leases; werft lease <image> makes one")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tOWNER\tIMAGE\tTIER\tADDRESS\tEXPIRY")
		for _, l := range leases {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, l.Owner, l.Image, l.Tier, l.Address, l.Expiry.Local().Format("2006-01-02 15:04"))
		}
		_ = tw.Flush()
	})
	return nil
}

func (c *cli) cmdExtend(args []string) error {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	by := fs.Duration("for", lease.Lifetime, "the time to add to the expiry, such as 1h or 30m")
	if err := c.parseFlags(fs, "extend", args); err != nil {
		return err
	}
	// The flags may also follow the lease id: werft extend abc123 --for 1h.
	var id string
	if fs.NArg() > 0 {
		id = fs.Arg(0)
		if err := c.parseFlags(fs, "extend", fs.Args()[1:]); err != nil {
			return err
		}
	}
	if id == "" || fs.NArg() != 0 {
		return failure.New(failure.Usage, "extend takes one lease id")
	}
	e, done, err := c.leaseHost()
	if err != nil {
		return err
	}
	defer done()
	c.reapFirst(e)
	l, err := lease.Extend(e, id, *by)
	if err != nil {
		return err
	}
	c.output(l, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "lease %s now expires at %s\n", l.ID, l.Expiry.Local().Format("2006-01-02 15:04"))
	})
	return nil
}

func (c *cli) cmdReap(args []string) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	if err := c.parseFlags(fs, "reap", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return failure.New(failure.Usage, "reap takes no arguments")
	}
	e, done, err := c.leaseHost()
	if err != nil {
		return err
	}
	defer done()
	r, err := lease.Reap(e)
	if err != nil {
		return err
	}
	c.output(r, func(w io.Writer) {
		n := len(r.Expired) + len(r.OrphanDomains) + len(r.OrphanDirectories)
		if n == 0 {
			_, _ = fmt.Fprintln(w, "nothing to reap")
			return
		}
		_, _ = fmt.Fprintf(w, "reaped %d expired leases, %d orphan domains, %d orphan lease directories\n",
			len(r.Expired), len(r.OrphanDomains), len(r.OrphanDirectories))
	})
	return nil
}

// leaseHost opens the hypervisor for a lease command; done closes it.
func (c *cli) leaseHost() (e lease.Env, done func(), err error) {
	dir, err := state.Root()
	if err != nil {
		return lease.Env{}, nil, err
	}
	h, err := c.hypervisor()
	if err != nil {
		return lease.Env{}, nil, err
	}
	return c.leaseEnv(h, dir), func() { _ = h.Close() }, nil
}
