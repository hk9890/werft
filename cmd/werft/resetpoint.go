package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

// lockedHypervisor connects to libvirt and takes the domain lock of name.
// close releases both.
func (c *cli) lockedHypervisor(name string) (virt.Hypervisor, func(), error) {
	dir, err := state.Root()
	if err != nil {
		return nil, nil, err
	}
	lock, err := dir.LockDomain(name)
	if err != nil {
		return nil, nil, err
	}
	h, err := c.hypervisor()
	if err != nil {
		_ = lock.Release()
		return nil, nil, err
	}
	return h, func() { _ = h.Close(); _ = lock.Release() }, nil
}

// nameAndLabel reads <name> [label]; the label defaults to clean-install,
// as in vm.
func nameAndLabel(fs *flag.FlagSet, command string) (name, label string, err error) {
	switch fs.NArg() {
	case 1:
		return fs.Arg(0), resetpoint.CleanInstall, nil
	case 2:
		return fs.Arg(0), fs.Arg(1), nil
	}
	return "", "", failure.New(failure.Usage, "%s takes a VM name and an optional label", command)
}

func (c *cli) cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	replace := fs.Bool("replace", false, "delete a reset point with the same label first")
	if err := c.parseFlags(fs, "snapshot", args); err != nil {
		return err
	}
	name, label, err := nameAndLabel(fs, "snapshot")
	if err != nil {
		return err
	}
	h, done, err := c.lockedHypervisor(name)
	if err != nil {
		return err
	}
	defer done()
	replaced, _ := resetpoint.Has(h, name, label)
	if err := resetpoint.Take(h, name, label, *replace); err != nil {
		return err
	}
	c.output(map[string]any{"domain": name, "reset_point": label, "replaced": replaced}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s reset point '%s' created\n  reset with: werft reset %s %s\n", name, label, name, label)
	})
	return nil
}

func (c *cli) cmdSnapshots(args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	if err := c.parseFlags(fs, "snapshots", args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return failure.New(failure.Usage, "snapshots takes one VM name")
	}
	name := fs.Arg(0)
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	native, err := h.Snapshots(name)
	if failure.Is(err, failure.NotFound) {
		return failure.New(failure.NotFound, "unknown VM %s; werft list shows the VMs", name)
	}
	if err != nil {
		return err
	}
	legacy, err := resetpoint.Legacy(h, name)
	if err != nil {
		return err
	}
	c.output(map[string]any{"domain": name, "reset_points": nonNil(native), "legacy": nonNil(legacy)}, func(w io.Writer) {
		if len(native)+len(legacy) == 0 {
			_, _ = fmt.Fprintf(w, "%s has no reset points\n", name)
		}
		for _, label := range native {
			_, _ = fmt.Fprintln(w, label)
		}
		if len(legacy) > 0 {
			_, _ = fmt.Fprintf(w, "reset points of vm, which werft migrate %s moves to werft:\n  %s\n", name, strings.Join(legacy, "\n  "))
		}
	})
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (c *cli) cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "reset without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	if err := c.parseFlags(fs, "reset", args); err != nil {
		return err
	}
	name, label, err := nameAndLabel(fs, "reset")
	if err != nil {
		return err
	}
	h, done, err := c.lockedHypervisor(name)
	if err != nil {
		return err
	}
	defer done()
	if err := resetpoint.CheckRevert(h, name, label); err != nil {
		return err
	}
	if err := c.confirm(yes, fmt.Sprintf("Reset %s to '%s'? Everything written since is lost.", name, label)); err != nil {
		return err
	}
	c.progress("Resetting %s to '%s'", name, label)
	if err := resetpoint.Revert(h, name, label); err != nil {
		return err
	}
	c.output(map[string]string{"domain": name, "reset_point": label}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s is back at '%s'\n", name, label)
	})
	return nil
}

func (c *cli) cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "migrate without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	if err := c.parseFlags(fs, "migrate", args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return failure.New(failure.Usage, "migrate takes one build VM name")
	}
	name := fs.Arg(0)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	h, done, err := c.lockedHypervisor(name)
	if err != nil {
		return err
	}
	defer done()
	m, err := resetpoint.PlanMigration(h, name, cfg.Pool)
	if err != nil {
		return err
	}
	if err := c.confirm(yes, migrationQuestion(m)); err != nil {
		return err
	}
	if len(m.Export) > 0 {
		if err := virt.EnsurePool(h, cfg.Pool, cfg.PoolPath()); err != nil {
			return err
		}
	}
	if err := m.Run(h, c.progress); err != nil {
		return err
	}
	c.output(map[string]any{"domain": name, "kept": m.Keep, "export_pool": m.ExportPool, "exported": nonNil(m.Export), "deleted": nonNil(m.Delete)},
		func(w io.Writer) {
			if m.Keep == "" {
				_, _ = fmt.Fprintf(w, "%s takes native reset points now; take one with werft prepare %s or werft snapshot %s\n", name, name, name)
				return
			}
			_, _ = fmt.Fprintf(w, "%s has the native reset point '%s'; vm no longer manages it\n", name, m.Keep)
		})
	return nil
}

func migrationQuestion(m *resetpoint.Migration) string {
	if m.Resume {
		return fmt.Sprintf("Finish the migration of %s: take '%s' as a native reset point?", m.Domain, m.Keep)
	}
	var b strings.Builder
	if m.Keep == "" {
		fmt.Fprintf(&b, "Migrate %s to native reset points? It keeps its current state, and has no reset point after.", m.Domain)
	} else {
		fmt.Fprintf(&b, "Migrate %s to native reset points? It goes back to '%s', and everything written since is lost.", m.Domain, m.Keep)
	}
	if len(m.Export) > 0 {
		fmt.Fprintf(&b, " '%s' moves to %s in pool %s.", resetpoint.CleanInstall, strings.Join(m.Export, " and "), m.ExportPool)
	}
	if len(m.Delete) > 0 {
		fmt.Fprintf(&b, " Deleted: %s.", strings.Join(m.Delete, ", "))
	}
	b.WriteString(" vm can no longer manage it.")
	return b.String()
}
