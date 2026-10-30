// Package libvirt is the go-libvirt adapter for the hypervisor port.
package libvirt

import (
	"fmt"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// Dial connects to the system libvirt daemon over its local socket.
func Dial() (*golibvirt.Libvirt, error) {
	l := golibvirt.NewWithDialer(dialers.NewLocal())
	if err := l.ConnectToURI(golibvirt.QEMUSystem); err != nil {
		return nil, fmt.Errorf("connect to qemu:///system: %w", err)
	}
	return l, nil
}
