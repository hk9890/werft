// Package libvirt is the go-libvirt adapter for the hypervisor port.
package libvirt

import (
	"fmt"
	"os"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

const (
	monolithicSocket = "/var/run/libvirt/libvirt-sock"
	modularSocket    = "/var/run/libvirt/virtqemud-sock"
)

// Dial connects to qemu:///system over the socket of the monolithic daemon,
// or of virtqemud on a host with modular daemons.
func Dial() (*Adapter, error) {
	socket := monolithicSocket
	if _, err := os.Stat(socket); err != nil {
		socket = modularSocket
	}
	l := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(socket)))
	if err := l.ConnectToURI(golibvirt.QEMUSystem); err != nil {
		return nil, fmt.Errorf("connect to qemu:///system: %w", err)
	}
	return &Adapter{l: l}, nil
}
