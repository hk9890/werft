// Command werft hands out disposable test VMs built from stock images.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

func main() {
	c := &cli{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		tty:        term.IsTerminal(int(os.Stdin.Fd())),
		readSecret: readPassword,
		dial:       dialLibvirt,
	}
	os.Exit(c.run(os.Args[1:]))
}

// readPassword reads a line from the terminal without echo. SIGINT and
// SIGTERM end the prompt with an error and turn the echo back on.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	type line struct {
		text string
		err  error
	}
	read := make(chan line, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		read <- line{string(b), err}
	}()
	select {
	case l := <-read:
		return l.text, l.err
	case sig := <-signals:
		_ = term.Restore(fd, state)
		return "", fmt.Errorf("interrupted by %v", sig)
	}
}
