// Package lifecycle lists, starts and stops domains: the commands list,
// start and stop. It acts on any domain, build VM or not, and needs no root:
// every disk and NVRAM fact comes through the storage API.
package lifecycle

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
)

// The timeouts of vm: a guest takes a DHCP lease within a minute of its
// start, and powers off within two minutes of an ACPI request.
const (
	AddressTimeout  = 60 * time.Second
	ShutdownTimeout = 120 * time.Second
	pollInterval    = 3 * time.Second
)

// Entry is one domain in the list. Used and Size are the allocation and
// capacity of its first disk, nil when that disk is in no storage pool.
// ResetPoints counts native snapshots and the reset points vm keeps.
// Address is empty unless the domain runs.
type Entry struct {
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Used        *uint64 `json:"used_bytes"`
	Size        *uint64 `json:"size_bytes"`
	ResetPoints int     `json:"reset_points"`
	LUKS        bool    `json:"luks"`
	Address     string  `json:"address"`
}

// List describes every domain, sorted by name. A domain that goes away
// while List runs is left out.
func List(h virt.Hypervisor) ([]Entry, error) {
	domains, err := h.Domains()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(domains, func(a, b virt.Domain) int { return strings.Compare(a.Name, b.Name) })
	entries := []Entry{}
	for _, d := range domains {
		e, err := describe(h, d.Name)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func describe(h virt.Hypervisor, name string) (Entry, error) {
	state, err := h.DomainState(name)
	if err != nil {
		return Entry{}, err
	}
	text, err := h.DomainXML(name, true)
	if err != nil {
		return Entry{}, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return Entry{}, err
	}
	native, err := h.Snapshots(name)
	if err != nil {
		return Entry{}, err
	}
	legacy, err := resetpoint.LegacyOf(d)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		Name:        name,
		State:       state.String(),
		ResetPoints: len(native) + len(legacy),
		LUKS:        domxml.HasLUKSMarker(domxml.Description(d)),
	}
	e.Used, e.Size = diskUsage(h, d)
	if state == virt.StateRunning {
		e.Address = Address(h, name)
	}
	return e, nil
}

// diskUsage is the allocation and capacity of the first disk, as vm shows
// them. A disk outside every pool, such as a block device, has none.
func diskUsage(h virt.Hypervisor, d *libvirtxml.Domain) (used, size *uint64) {
	for _, disk := range domxml.Disks(d) {
		if disk.Device != "disk" || (disk.Source == "" && disk.Volume == "") {
			continue
		}
		pool, volume := disk.Pool, disk.Volume
		if volume == "" {
			var err error
			if pool, volume, err = h.VolumeByPath(disk.Source); err != nil {
				return nil, nil
			}
		}
		info, err := h.VolumeInfo(pool, volume)
		if err != nil {
			return nil, nil
		}
		return &info.Allocation, &info.Capacity
	}
	return nil, nil
}

// Address is how to reach a running domain: vsock/<cid> when it has a vsock
// CID, else its first IPv4 DHCP lease, else empty.
func Address(h virt.Hypervisor, name string) string {
	cid, ips := virt.DomainAddresses(h, name)
	if cid != "" || len(ips) == 0 {
		return cid
	}
	return ips[0]
}

// Started says what Start found and did.
type Started struct {
	AlreadyRunning bool
	// LUKS is set when the disk asks for its password at the console; start
	// does not type it.
	LUKS bool
	// Leases are the DHCP leases libvirt held before the start; WaitAddress
	// waits for a newer one.
	Leases []virt.DHCPLease
}

// Start starts the domain unless it runs already. A paused domain is a
// conflict. No viewer opens.
func Start(h virt.Hypervisor, name string) (Started, error) {
	state, err := domainState(h, name)
	if err != nil {
		return Started{}, err
	}
	desc, err := h.DomainDescription(name)
	if err != nil {
		return Started{}, err
	}
	if state == virt.StatePaused {
		return Started{}, failure.New(failure.Conflict, "%s is paused; resume it: virsh -c qemu:///system resume %s", name, name)
	}
	s := Started{AlreadyRunning: state == virt.StateRunning, LUKS: domxml.HasLUKSMarker(desc)}
	if s.AlreadyRunning {
		return s, nil
	}
	s.Leases = virt.DomainLeases(h, name)
	return s, h.StartDomain(name)
}

// WaitAddress waits up to AddressTimeout for the guest to take a DHCP lease
// that is not in before, the sign that it is up, and then returns Address.
// A lease from before the start can outlive the domain. It returns an empty
// string when no lease comes, also for a domain with a vsock CID: the guest
// can still be booting, or hang before its network is up.
func WaitAddress(h virt.Hypervisor, name string, before []virt.DHCPLease, sleep func(time.Duration)) string {
	up := poll(AddressTimeout, sleep, func() bool {
		return virt.NewLease(before, virt.DomainLeases(h, name))
	})
	if !up {
		return ""
	}
	return Address(h, name)
}

type Stopped int

const (
	AlreadyShutOff Stopped = iota
	ShutDown
	ForcedOff
)

// Stop sends an ACPI shutdown and waits up to ShutdownTimeout for the domain
// to shut off; force destroys it instead. progress gets a line when the wait
// starts.
func Stop(h virt.Hypervisor, name string, force bool, sleep func(time.Duration), progress io.Writer) (Stopped, error) {
	state, err := domainState(h, name)
	if err != nil {
		return 0, err
	}
	switch {
	case state == virt.StateShutOff:
		return AlreadyShutOff, nil
	case force:
		return ForcedOff, h.DestroyDomain(name)
	}
	if err := h.ShutdownDomain(name); err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(progress, "ACPI shutdown sent to %s, waiting up to %v\n", name, ShutdownTimeout)
	off := poll(ShutdownTimeout, sleep, func() bool {
		s, err := h.DomainState(name)
		return err == nil && s == virt.StateShutOff
	})
	if !off {
		return 0, failure.New(failure.Timeout, "%s did not shut down within %v; werft stop --force %s kills it", name, ShutdownTimeout, name)
	}
	return ShutDown, nil
}

func domainState(h virt.Hypervisor, name string) (virt.State, error) {
	state, err := h.DomainState(name)
	if failure.Is(err, failure.NotFound) {
		return 0, failure.New(failure.NotFound, "unknown VM %s; werft list shows the VMs", name)
	}
	return state, err
}

// poll calls cond every pollInterval until it holds or timeout has passed,
// and reports whether it held.
func poll(timeout time.Duration, sleep func(time.Duration), cond func() bool) bool {
	for waited := time.Duration(0); ; waited += pollInterval {
		if cond() {
			return true
		}
		if waited >= timeout {
			return false
		}
		sleep(pollInterval)
	}
}
