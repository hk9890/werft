// Package virt is the hypervisor port: the calls werft makes to libvirt, and
// the types that cross the boundary. Errors carry a failure kind; a missing
// object is failure.NotFound, an object in the wrong state failure.Conflict.
//
// Every object is addressed by name. Metadata and the description are read
// from and written to the persistent definition, so they hold while the
// domain is shut off.
package virt

import (
	"fmt"
	"io"
)

type Hypervisor interface {
	LibVersion() (Version, error)
	HostFreeMemory() (uint64, error)

	Domains() ([]Domain, error)
	Domain(name string) (Domain, error)
	// DefineDomain fails with failure.Conflict when another domain has the
	// name, so a define reserves the name.
	DefineDomain(xml string) (Domain, error)
	StartDomain(name string) error
	ShutdownDomain(name string) error
	DestroyDomain(name string) error
	UndefineDomain(name string, flags UndefineFlags) error
	DomainState(name string) (State, error)
	DomainXML(name string, inactive bool) (string, error)
	// DomainMetadata is the metadata element in namespace uri; failure.NotFound
	// when the domain has none.
	DomainMetadata(name, uri string) (string, error)
	// SetDomainMetadata with an empty xml removes the element.
	SetDomainMetadata(name, uri, key, xml string) error
	// DomainDescription is empty when the domain has none.
	DomainDescription(name string) (string, error)
	SetDomainDescription(name, description string) error

	CreateSnapshot(domain, name, description string) error
	// RedefineSnapshot adds the metadata of a snapshot that exists already in
	// the disks: xml is a full snapshot description, with its domain.
	RedefineSnapshot(domain, xml string) error
	Snapshots(domain string) ([]string, error)
	SnapshotXML(domain, name string) (string, error)
	RevertSnapshot(domain, name string) error
	DeleteSnapshot(domain, name string) error

	DefinePool(name, path string) error
	BuildPool(name string) error
	StartPool(name string) error
	RefreshPool(name string) error
	PoolInfo(name string) (PoolInfo, error)
	// Pools is every pool libvirt knows, active or not.
	Pools() ([]Pool, error)

	// Volumes is the names of the volumes in pool, as libvirt knows them
	// since its last refresh.
	Volumes(pool string) ([]string, error)
	CreateVolume(pool, xml string) (Volume, error)
	// CreateVolumeFrom creates a volume in pool from xml, filled with the
	// content of volume from in pool fromPool, converted to the format xml names.
	CreateVolumeFrom(pool, xml, fromPool, from string) (Volume, error)
	UploadVolume(pool, name string, r io.Reader, length uint64) error
	DownloadVolume(pool, name string, w io.Writer) error
	DeleteVolume(pool, name string) error
	VolumePath(pool, name string) (string, error)
	VolumeInfo(pool, name string) (VolumeInfo, error)
	// VolumeByPath is the pool and name of the volume at path.
	VolumeByPath(path string) (pool, name string, err error)

	DHCPLeases(network, mac string) ([]DHCPLease, error)

	// Screenshot writes the image of the domain's first screen to w and
	// returns its MIME type.
	Screenshot(domain string, w io.Writer) (string, error)
	// SendKey presses the keys, given as Linux input key codes, together.
	SendKey(domain string, keycodes []uint32) error
	AttachDevice(domain, xml string) error
	DetachDevice(domain, xml string) error

	Close() error
}

type Domain struct {
	Name string
	UUID string
}

type State int

const (
	StateOther State = iota
	StateRunning
	StatePaused
	StateShuttingDown
	StateShutOff
	StateCrashed
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateShuttingDown:
		return "shutting down"
	case StateShutOff:
		return "shut off"
	case StateCrashed:
		return "crashed"
	}
	return "other"
}

type UndefineFlags uint

const (
	// UndefineNVRAM deletes the NVRAM file with the domain.
	UndefineNVRAM UndefineFlags = 1 << iota
	// UndefineKeepNVRAM leaves the NVRAM file in place.
	UndefineKeepNVRAM
	// UndefineSnapshots removes the snapshot metadata. Without it a domain
	// with snapshots cannot be undefined.
	UndefineSnapshots
)

// Pool is a storage pool and the directory it covers.
type Pool struct {
	Name string
	Path string
}

type PoolInfo struct {
	Active     bool
	Capacity   uint64
	Allocation uint64
	Available  uint64
}

type Volume struct {
	Name string
	Path string
}

type VolumeInfo struct {
	Capacity   uint64
	Allocation uint64
}

type DHCPLease struct {
	MAC      string
	IP       string
	Hostname string
	Expiry   int64
}

// Version is a libvirt version, major.minor.release.
type Version struct{ Major, Minor, Release uint64 }

// ParseVersion decodes the number libvirt reports: major*1000000 +
// minor*1000 + release.
func ParseVersion(n uint64) Version {
	return Version{Major: n / 1000000, Minor: n / 1000 % 1000, Release: n % 1000}
}

func (v Version) AtLeast(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Release >= o.Release
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Release) }
