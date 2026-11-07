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
	"github.com/hk9890/werft/internal/lease"
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
		Global: globalLock(dir),
		Now:    time.Now, Out: c.stderr,
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
	prune := fs.Bool("prune", false, "delete the old stock versions that no lease uses")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "prune without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	if err := c.parseFlags(fs, "images", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return failure.New(failure.Usage, "images takes no arguments")
	}
	if yes && !*prune {
		return failure.New(failure.Usage, "--yes needs --prune")
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	if *prune {
		return c.pruneImages(dir, yes)
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

// pruneImages runs the reap pass, then deletes the old stock versions that
// no lease uses, after the confirmation. The reap pass first frees the
// versions of expired leases.
func (c *cli) pruneImages(dir state.Dir, yes bool) error {
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	c.reapFirst(c.leaseEnv(h, dir))
	e := image.PruneEnv{
		H: h, Catalog: dir.Images(),
		Leased: func() (map[string]bool, error) { return lease.Stocks(h) },
		Global: globalLock(dir),
		Out:    c.stderr,
	}
	planned, err := image.Prunable(e)
	if err != nil {
		return err
	}
	removed := []image.Record{}
	if len(planned) > 0 {
		names := make([]string, len(planned))
		for i, r := range planned {
			names[i] = r.Name
		}
		if err := c.confirm(yes, fmt.Sprintf("Prune deletes the disk, NVRAM and record of %s. Delete them?", strings.Join(names, ", "))); err != nil {
			return err
		}
		if removed, err = image.Prune(e, planned); err != nil {
			return err
		}
	}
	c.output(map[string][]image.Record{"removed": removed}, func(w io.Writer) {
		if len(removed) == 0 {
			_, _ = fmt.Fprintln(w, "nothing to prune: each old stock version is the newest of its image and tier, or a lease uses it")
			return
		}
		for _, r := range removed {
			_, _ = fmt.Fprintf(w, "%s removed\n", r.Name)
		}
	})
	return nil
}
