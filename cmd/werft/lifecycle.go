package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/lifecycle"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

func (c *cli) hypervisor() (virt.Hypervisor, error) {
	h, err := c.dial()
	if err != nil {
		return nil, failure.Wrap(failure.Failed, err, "libvirt")
	}
	return h, nil
}

func (c *cli) cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if err := c.parseFlags(fs, "list", args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return failure.New(failure.Usage, "list takes no arguments")
	}
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	entries, err := lifecycle.List(h)
	if err != nil {
		return err
	}
	c.output(entries, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tUSED\tSIZE\tRESET\tLUKS\tADDRESS")
		for _, e := range entries {
			luks := "-"
			if e.LUKS {
				luks = "yes"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				e.Name, e.State, bytesText(e.Used), bytesText(e.Size), e.ResetPoints, luks, orDash(e.Address))
		}
		_ = tw.Flush()
	})
	return nil
}

// bytesText is n in binary units, as virsh vol-info shows it, or "-" when
// it is unknown.
func bytesText(p *uint64) string {
	if p == nil {
		return "-"
	}
	n := *p
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *cli) cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	view := fs.Bool("view", false, "also open a virt-viewer window: Shift+F11 fullscreen, Shift+F12 release cursor")
	if err := c.parseFlags(fs, "start", args); err != nil {
		return err
	}
	if err := flagsBeforeNames(fs, "start", "<name>"); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return failure.New(failure.Usage, "start takes one VM name")
	}
	name := fs.Arg(0)
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	started, err := lifecycle.Start(lifecycle.Env{
		Ctx: ctx, H: h,
		DiskPassword: func() (secret.Spec, error) { return startDiskPasswordSpec(name) },
		Secrets:      c.secretStore(), Terminal: prompter{c}, TTY: c.tty,
		Lock: lockDomain, Sleep: c.sleep, Out: c.stderr,
	}, name)
	stop()
	if err != nil {
		return err
	}
	viewerOpen := false
	if *view {
		if err := c.openViewer(name); err != nil {
			c.progress("warning: no viewer window: %v; open one on the desktop: %s", err, steps.ViewerCommand(name))
		} else {
			viewerOpen = true
			c.progress("virt-viewer attached: Shift+F11 fullscreen, Shift+F12 release cursor")
		}
	}
	if started.LUKS && !started.AlreadyRunning && !started.Typed {
		if viewerOpen {
			c.progress("The disk of %s asks for its password at the console; werft does not type it. Type it in the open viewer window", name)
		} else {
			c.progress("The disk of %s asks for its password at the console; werft does not type it. Type it in the viewer: werft start --view %s", name, name)
		}
	}
	c.progress("Waiting up to %v for an address", lifecycle.AddressTimeout)
	addr := lifecycle.WaitAddress(h, name, started.Leases, c.sleep)
	if addr == "" {
		c.progress("No address yet; werft list shows it once the guest is up")
	}
	if strings.HasPrefix(name, leasePrefix) {
		c.readdressLease(h, name, addr, started.AlreadyRunning)
	}
	c.output(map[string]any{"domain": name, "state": "running", "already_running": started.AlreadyRunning, "address": addr},
		func(w io.Writer) {
			if addr != "" {
				_, _ = fmt.Fprintf(w, "address: %s   ssh %s\n", addr, addr)
			}
		})
	return nil
}

// readdressLease brings the ssh_config of a started lease up to date. The VM
// did start, so a failure here is only a warning.
func (c *cli) readdressLease(h virt.Hypervisor, name, addr string, running bool) {
	dir, err := state.Root()
	switch {
	case err != nil:
	case running:
		err = lease.ReaddressRunning(c.leaseEnv(h, dir), name)
	default:
		err = lease.Readdress(c.leaseEnv(h, dir), name, addr)
	}
	if err != nil {
		c.progress("warning: the ssh_config of %s can hold an old address: %v", name, err)
	}
}

func (c *cli) cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	var force bool
	fs.BoolVar(&force, "force", false, "force the VM off at once instead of an ACPI shutdown")
	fs.BoolVar(&force, "f", false, "short for --force")
	if err := c.parseFlags(fs, "stop", args); err != nil {
		return err
	}
	if err := flagsBeforeNames(fs, "stop", "<name>"); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return failure.New(failure.Usage, "stop takes one VM name")
	}
	name := fs.Arg(0)
	h, err := c.hypervisor()
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()

	stopped, err := lifecycle.Stop(h, name, force, c.sleep, c.stderr)
	if err != nil {
		return err
	}
	result := stopResults[stopped]
	c.output(map[string]string{"domain": name, "state": "shut off", "result": result.json}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, result.text+"\n", name)
	})
	return nil
}

var stopResults = map[lifecycle.Stopped]struct{ json, text string }{
	lifecycle.AlreadyShutOff: {"already_shut_off", "%s is already shut off"},
	lifecycle.ShutDown:       {"shut_down", "%s is shut off"},
	lifecycle.ForcedOff:      {"forced_off", "%s forced off"},
}

// progress writes a line for the person who watches: to stderr, so stdout
// keeps only the result.
func (c *cli) progress(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stderr, format+"\n", args...)
}

// startDiskPasswordSpec says where the disk password of the LUKS domain
// name comes from: guest_password of the recipe of its build VM, as seal
// reads it; -full maps to its build VM.
func startDiskPasswordSpec(name string) (secret.Spec, error) {
	cfg, err := loadConfig()
	if err != nil {
		return secret.Spec{}, err
	}
	buildVM, _ := strings.CutSuffix(name, resetpoint.FullSuffix)
	return diskPasswordSpec(cfg, "", buildVM, name)
}

// lockDomain takes the domain lock of name.
func lockDomain(name string) (func(), error) {
	dir, err := state.Root()
	if err != nil {
		return nil, err
	}
	lock, err := dir.LockDomain(name)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}
