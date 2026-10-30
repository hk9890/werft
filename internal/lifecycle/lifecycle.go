// Package lifecycle lists, starts and stops domains: the commands list,
// start and stop. It acts on any domain, build VM or not, and needs no root:
// every disk and NVRAM fact comes through the storage API.
package lifecycle

import (
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
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
// capacity of its first disk, zero when that disk is in no storage pool.
// ResetPoints counts native snapshots and the reset points vm keeps.
// Address is set only while the domain runs.
type Entry struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Used        uint64 `json:"used_bytes,omitempty"`
	Size        uint64 `json:"size_bytes,omitempty"`
	ResetPoints int    `json:"reset_points"`
	LUKS        bool   `json:"luks"`
	Address     string `json:"address,omitempty"`
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
	legacy, err := resetpoint.Legacy(h, name)
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
func diskUsage(h virt.Hypervisor, d *libvirtxml.Domain) (used, size uint64) {
	for _, disk := range domxml.Disks(d) {
		if disk.Device != "disk" || (disk.Source == "" && disk.Volume == "") {
			continue
		}
		pool, volume := disk.Pool, disk.Volume
		if volume == "" {
			var err error
			if pool, volume, err = h.VolumeByPath(disk.Source); err != nil {
				return 0, 0
			}
		}
		info, err := h.VolumeInfo(pool, volume)
		if err != nil {
			return 0, 0
		}
		return info.Allocation, info.Capacity
	}
	return 0, 0
}

// Address is how to reach a running domain: vsock/<cid> when it has a vsock
// CID, else the first IPv4 DHCP lease of its MACs, else empty.
func Address(h virt.Hypervisor, name string) string {
	cid, ip := addresses(h, name)
	if cid != "" {
		return cid
	}
	return ip
}

func addresses(h virt.Hypervisor, name string) (cid, ip string) {
	text, err := h.DomainXML(name, false)
	if err != nil {
		return "", ""
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return "", ""
	}
	if c, ok := domxml.VsockCID(d); ok {
		cid = "vsock/" + strconv.FormatUint(uint64(c), 10)
	}
	for _, i := range domxml.Interfaces(d) {
		if i.Network == "" || i.MAC == "" {
			continue
		}
		leases, _ := h.DHCPLeases(i.Network, i.MAC)
		for _, l := range leases {
			if net.ParseIP(l.IP).To4() != nil {
				return cid, l.IP
			}
		}
	}
	return cid, ""
}

// Started says what Start found and did.
type Started struct {
	AlreadyRunning bool
	// LUKS is set when the disk asks for its password at the console; start
	// does not type it.
	LUKS bool
}

// Start starts the domain unless it runs already. No viewer opens.
func Start(h virt.Hypervisor, name string) (Started, error) {
	state, err := domainState(h, name)
	if err != nil {
		return Started{}, err
	}
	desc, err := h.DomainDescription(name)
	if err != nil {
		return Started{}, err
	}
	s := Started{AlreadyRunning: state == virt.StateRunning, LUKS: domxml.HasLUKSMarker(desc)}
	if s.AlreadyRunning {
		return s, nil
	}
	return s, h.StartDomain(name)
}

// WaitAddress waits up to AddressTimeout for the guest to take a DHCP lease,
// the sign that it is up, and returns Address. It returns an empty string
// when no lease comes: the guest can still be booting.
func WaitAddress(h virt.Hypervisor, name string, sleep func(time.Duration)) string {
	poll(AddressTimeout, sleep, func() bool {
		_, ip := addresses(h, name)
		return ip != ""
	})
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
