// Command werft hands out disposable test VMs built from stock images.
package main

import (
	"os"

	"golang.org/x/term"
)

func main() {
	c := &cli{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		tty:    term.IsTerminal(int(os.Stdin.Fd())),
		readSecret: func() (string, error) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(b), err
		},
		dial: dialLibvirt,
	}
	os.Exit(c.run(os.Args[1:]))
}
