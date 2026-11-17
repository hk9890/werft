package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
)

// cli is one invocation: its streams, whether stdin is a terminal, and the
// global flags.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	tty    bool
	json   bool
	// readSecret reads a line from the terminal without echo.
	readSecret func() (string, error)
	dial       func() (virt.Hypervisor, error)
	// openViewer opens a detached viewer window on a domain.
	openViewer func(domain string) error
	sleep      func(time.Duration)
	// secrets is the keyring and pass; nil means those of the host.
	secrets secret.Store
}

type command struct {
	name    string
	args    string
	summary string
	run     func(c *cli, args []string) error
}

// commands is filled in init, because the help command lists them.
var commands []command

func init() {
	commands = []command{
		{name: "completion", args: "<shell>", summary: "print the zsh completion script: source <(werft completion zsh)", run: (*cli).cmdCompletion},
		{name: "extend", args: "<lease-id> [--for duration]", summary: "move the expiry of a lease by --for (default 4h)", run: (*cli).cmdExtend},
		{name: "guide", summary: "print how an agent leases a VM, uses it and gives it back", run: (*cli).cmdGuide},
		{name: "help", args: "[command]", summary: "show the usage of werft or of one command", run: (*cli).cmdHelp},
		{name: "images", args: "[--prune] [--yes]", summary: "list the stock images: image, tier, version, date; --prune deletes the old versions no lease uses", run: (*cli).cmdImages},
		{name: "lease", args: "<image> [--tier plain|full] [--stock name] [--memory mib] [--cpus n] [--for duration] [--owner o] [--view]", summary: "lease a disposable VM from a stock image, the newest by default; returns when SSH answers", run: (*cli).cmdLease},
		{name: "leases", summary: "list the leases: id, owner, image, tier, state, address, expiry; releases expired ones first", run: (*cli).cmdLeases},
		{name: "list", summary: "list all VMs: state, disk use, reset points, LUKS marker, address", run: (*cli).cmdList},
		{name: "migrate", args: "[--yes] <name>", summary: "move a build VM from the reset points of vm to native ones: keep provision-base (clean on -full), export clean-install", run: (*cli).cmdMigrate},
		{name: "prepare", args: "[--yes] [--recipe file] <name>", summary: "make a stock install the base provision starts from: werft key and your own key, hostname, provision-base", run: (*cli).cmdPrepare},
		{name: "provision", args: "[--yes] [--recipe file] [--key mount] <name>", summary: "run the recipe on <name> from provision-base and keep the result as <name>-full", run: (*cli).cmdProvision},
		{name: "reap", summary: "release expired leases, lease VMs without a lease directory, and lease directories without a VM", run: (*cli).cmdReap},
		{name: "release", args: "[--owner o] [lease-id]", summary: "destroy a lease: its VM, disk and key; --owner destroys every lease of that owner; a second release succeeds", run: (*cli).cmdRelease},
		{name: "reset", args: "[--yes] <name> [label]", summary: "roll a shut-off VM back to a reset point (default label: clean-install)", run: (*cli).cmdReset},
		{name: "seal", args: "[--yes] [--recipe file] <build-vm> <label>", summary: "make a new stock image version from a reset point of a build VM; provision-base gives the plain tier, clean on <x>-full the full tier", run: (*cli).cmdSeal},
		{name: "snapshot", args: "[--replace] <name> [label]", summary: "take a reset point of a shut-off VM (default label: clean-install); --replace retakes one", run: (*cli).cmdSnapshot},
		{name: "snapshots", args: "<name>", summary: "list the reset points of a VM, and those vm keeps", run: (*cli).cmdSnapshots},
		{name: "start", args: "[--view] <name>", summary: "start a VM headless and wait for its address; --view also opens virt-viewer", run: (*cli).cmdStart},
		{name: "stop", args: "[--force] <name>", summary: "ACPI shutdown and wait; --force forces the VM off at once", run: (*cli).cmdStop},
		{name: "version", summary: "print the werft version", run: (*cli).cmdVersion},
	}
}

// exitCodes is the exit-code contract of docs/DESIGN.md, "Agent interface".
var exitCodes = map[failure.Kind]int{
	failure.Failed:            1,
	failure.Usage:             2,
	failure.NotFound:          3,
	failure.Conflict:          4,
	failure.NeedsConfirmation: 5,
	failure.NoCapacity:        6,
	failure.Timeout:           7,
	failure.ChecksFailed:      8,
	failure.Unsupported:       9,
	failure.SecretUnavailable: 10,
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if code, ok := exitCodes[failure.KindOf(err)]; ok {
		return code
	}
	return 1
}

func (c *cli) run(args []string) int {
	args = c.globalFlags(args)
	err := c.dispatch(args)
	if errors.Is(err, errHelpShown) {
		return 0
	}
	if err != nil {
		c.reportError(err)
	}
	return exitCode(err)
}

// globalFlags takes --json out of args, wherever it stands before "--".
func (c *cli) globalFlags(args []string) []string {
	var rest []string
	for i, a := range args {
		if a == "--" {
			return append(rest, args[i:]...)
		}
		if a == "--json" {
			c.json = true
			continue
		}
		rest = append(rest, a)
	}
	return rest
}

func (c *cli) dispatch(args []string) error {
	if len(args) == 0 {
		args = []string{"help"}
	}
	name, rest := args[0], args[1:]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	cmd, ok := lookup(name)
	if !ok {
		return failure.New(failure.Usage, "unknown command %q; run 'werft help'", name)
	}
	return cmd.run(c, rest)
}

func lookup(name string) (command, bool) {
	i := slices.IndexFunc(commands, func(cmd command) bool { return cmd.name == name })
	if i < 0 {
		return command{}, false
	}
	return commands[i], true
}

func (c *cli) reportError(err error) {
	if c.json {
		c.writeJSON(map[string]any{"error": map[string]string{
			"kind":    string(failure.KindOf(err)),
			"message": err.Error(),
		}})
		return
	}
	_, _ = fmt.Fprintln(c.stderr, "werft:", err)
}

func (c *cli) writeJSON(v any) {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// output writes v as JSON with --json, else calls text.
func (c *cli) output(v any, text func(w io.Writer)) {
	if c.json {
		c.writeJSON(v)
		return
	}
	text(c.stdout)
}

// parseFlags parses the flags of command name. -h prints the command's usage,
// as JSON with --json, and ends the command with errHelpShown.
func (c *cli) parseFlags(fs *flag.FlagSet, name string, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		cmd, _ := lookup(name)
		c.output(commandHelpJSON(cmd, fs), func(w io.Writer) { _, _ = fmt.Fprint(w, commandUsage(cmd, fs)) })
		return errHelpShown
	}
	if err != nil {
		return failure.New(failure.Usage, "%s: %v", name, err)
	}
	return nil
}

// errHelpShown ends a command whose usage was asked for; run treats it as
// success.
var errHelpShown = errors.New("help shown")

// confirm asks question on the terminal unless yes is set. Without a
// terminal it fails with needs_confirmation and names --yes, so an agent
// never waits on a prompt.
func (c *cli) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if !c.tty {
		return failure.New(failure.NeedsConfirmation, "%s: needs confirmation and stdin is no terminal; pass --yes", question)
	}
	_, _ = fmt.Fprintf(c.stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return failure.New(failure.NeedsConfirmation, "%s: not confirmed", question)
}
