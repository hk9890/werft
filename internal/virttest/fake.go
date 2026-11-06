// Package virttest is an in-memory virt.Hypervisor for unit tests: domains,
// snapshots, pools and volumes, a log of every call, and failure injection.
package virttest

import (
	"bytes"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// Fake is safe for concurrent use. Set Version and TotalMemory before the code
// under test runs.
type Fake struct {
	Version     virt.Version
	TotalMemory uint64

	mu       sync.Mutex
	calls    []string
	failures map[string]error
	domains  map[string]*domain
	pools    map[string]*pool
	leases   map[string][]virt.DHCPLease
	nextCID  uint32
}

type domain struct {
	uuid string
	// transient is a running domain that was undefined; it goes away when
	// it stops.
	transient  bool
	xml        string
	liveXML    string
	state      virt.State
	metadata   map[string]string
	snapshots  []snapshot
	keys       [][]uint32
	devices    []string
	screenshot []byte
	screenMIME string
	// ignoresACPI is a guest that does not react to ShutdownDomain.
	ignoresACPI bool
}

type snapshot struct {
	name        string
	description string
	xml         string
	state       virt.State
	// disks is the path of each disk the snapshot holds a qcow2 internal
	// snapshot in.
	disks []string
}

type pool struct {
	path     string
	active   bool
	capacity uint64
	volumes  map[string]*volume
}

type volume struct {
	path     string
	capacity uint64
	data     []byte
	// internal is the qcow2 internal snapshot table: the data at each
	// snapshot name. A copy of the volume does not take it.
	internal map[string][]byte
}

var _ virt.Hypervisor = (*Fake)(nil)

func New() *Fake {
	return &Fake{
		Version:     virt.Version{Major: 12},
		TotalMemory: 32 << 30,
		failures:    map[string]error{},
		domains:     map[string]*domain{},
		pools:       map[string]*pool{},
		leases:      map[string][]virt.DHCPLease{},
		nextCID:     3,
	}
}

// Calls is the log of calls so far, one entry per call: the method name and
// the names it acted on, separated by spaces, for example
// "StartDomain werft-a" or "CreateVolume werft disk.qcow2".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// FailOn makes every call whose log entry is call, or starts with call and a
// space, fail with err. "StartDomain" fails every start; "StartDomain a" only
// the start of a. A nil err removes the failure.
func (f *Fake) FailOn(call string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failures, call)
		return
	}
	f.failures[call] = err
}

// record logs a call and returns the injected failure for it, if any. The
// caller holds f.mu.
func (f *Fake) record(method string, names ...string) error {
	entry := strings.Join(append([]string{method}, names...), " ")
	f.calls = append(f.calls, entry)
	for call, err := range f.failures {
		if entry == call || strings.HasPrefix(entry, call+" ") {
			return err
		}
	}
	return nil
}

func notFound(what, name string) error {
	return failure.New(failure.NotFound, "%s %s not found", what, name)
}

func (f *Fake) domain(name string) (*domain, error) {
	d, ok := f.domains[name]
	if !ok {
		return nil, notFound("domain", name)
	}
	return d, nil
}

// SetState puts a domain in a state, as a guest that powers itself off or
// crashes would. A transient domain that shuts off goes away.
func (f *Fake) SetState(name string, s virt.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[name]; ok {
		d.state = s
		if s == virt.StateShutOff && d.transient {
			delete(f.domains, name)
		}
	}
}

func (f *Fake) AddDHCPLease(network string, l virt.DHCPLease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[network] = append(f.leases[network], l)
}

func (f *Fake) SetScreenshot(domain, mime string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[domain]; ok {
		d.screenMIME, d.screenshot = mime, slices.Clone(data)
	}
}

// SentKeys is every key combination sent to the domain, in order.
func (f *Fake) SentKeys(domain string) [][]uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[domain]; ok {
		return slices.Clone(d.keys)
	}
	return nil
}

// Devices is the XML of each device attached and not detached since.
func (f *Fake) Devices(domain string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[domain]; ok {
		return slices.Clone(d.devices)
	}
	return nil
}

func (f *Fake) VolumeData(pool, name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pools[pool]; ok {
		if v, ok := p.volumes[name]; ok {
			return slices.Clone(v.data), true
		}
	}
	return nil, false
}

func (f *Fake) SetPoolCapacity(name string, capacity uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pools[name]; ok {
		p.capacity = capacity
	}
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("Close")
}

func (f *Fake) LibVersion() (virt.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Version, f.record("LibVersion")
}

func (f *Fake) HostTotalMemory() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.TotalMemory, f.record("HostTotalMemory")
}

func (f *Fake) Domains() ([]virt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Domains"); err != nil {
		return nil, err
	}
	var out []virt.Domain
	for name, d := range f.domains {
		out = append(out, virt.Domain{Name: name, UUID: d.uuid})
	}
	slices.SortFunc(out, func(a, b virt.Domain) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *Fake) Domain(name string) (virt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Domain", name); err != nil {
		return virt.Domain{}, err
	}
	d, err := f.domain(name)
	if err != nil {
		return virt.Domain{}, err
	}
	return virt.Domain{Name: name, UUID: d.uuid}, nil
}

// DefineDomain follows libvirt: an XML without a UUID gets a new one, and a
// name that another UUID holds is a conflict. The same name and UUID
// redefines the domain.
func (f *Fake) DefineDomain(xml string) (virt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, err := domxml.Parse(xml)
	if err != nil {
		return virt.Domain{}, failure.Wrap(failure.Failed, err, "define domain")
	}
	if err := f.record("DefineDomain", x.Name); err != nil {
		return virt.Domain{}, err
	}
	if x.Name == "" {
		return virt.Domain{}, failure.New(failure.Failed, "define domain: no name")
	}
	if x.UUID == "" {
		x.UUID = newUUID()
	}
	for name, d := range f.domains {
		switch {
		case name == x.Name && d.uuid != x.UUID:
			return virt.Domain{}, failure.New(failure.Conflict, "domain %s already exists with uuid %s", name, d.uuid)
		case name != x.Name && d.uuid == x.UUID:
			return virt.Domain{}, failure.New(failure.Conflict, "domain %s is already defined with uuid %s", name, d.uuid)
		}
	}
	out, err := x.Marshal()
	if err != nil {
		return virt.Domain{}, failure.Wrap(failure.Failed, err, "define domain")
	}
	metadata := map[string]string{}
	if x.Metadata != nil {
		if err := addMetadata(metadata, x.Metadata.XML); err != nil {
			return virt.Domain{}, failure.Wrap(failure.Failed, err, "define domain metadata")
		}
	}
	d, ok := f.domains[x.Name]
	if ok {
		d.xml, d.transient = out, false
	} else {
		d = &domain{uuid: x.UUID, xml: out, state: virt.StateShutOff, metadata: map[string]string{}}
		f.domains[x.Name] = d
	}
	maps.Copy(d.metadata, metadata)
	return virt.Domain{Name: x.Name, UUID: x.UUID}, nil
}

// addMetadata puts each element of the metadata element text into m under
// its namespace, as DomainMetadata reads it.
func addMetadata(m map[string]string, text string) error {
	dec := xml.NewDecoder(strings.NewReader(text))
	for {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if el, ok := tok.(xml.StartElement); ok {
			if err := dec.Skip(); err != nil {
				return err
			}
			m[el.Name.Space] = text[start:dec.InputOffset()]
		}
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// StartDomain gives an auto vsock CID an address in the live XML, as libvirt
// does.
func (f *Fake) StartDomain(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StartDomain", name); err != nil {
		return err
	}
	d, err := f.domain(name)
	if err != nil {
		return err
	}
	if d.state != virt.StateShutOff && d.state != virt.StateCrashed {
		return failure.New(failure.Conflict, "domain %s is already active", name)
	}
	x, err := domxml.Parse(d.xml)
	if err != nil {
		return failure.Wrap(failure.Failed, err, "start %s", name)
	}
	if x.Devices != nil && x.Devices.VSock != nil && x.Devices.VSock.CID != nil && x.Devices.VSock.CID.Auto == "yes" {
		x.Devices.VSock.CID.Address = strconv.FormatUint(uint64(f.nextCID), 10)
		f.nextCID++
	}
	if d.liveXML, err = x.Marshal(); err != nil {
		return failure.Wrap(failure.Failed, err, "start %s", name)
	}
	d.state = virt.StateRunning
	return nil
}

// ShutdownDomain shuts the domain off at once, unless IgnoreShutdown was
// called for it.
func (f *Fake) ShutdownDomain(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[name]; ok && d.ignoresACPI && d.state != virt.StateShutOff {
		return f.record("ShutdownDomain", name)
	}
	return f.stop("ShutdownDomain", name)
}

// IgnoreShutdown makes the guest of domain ignore the ACPI shutdown request,
// as a guest that hangs or has no ACPI handler does.
func (f *Fake) IgnoreShutdown(domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.domains[domain]; ok {
		d.ignoresACPI = true
	}
}

func (f *Fake) DestroyDomain(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stop("DestroyDomain", name)
}

// stop shuts the domain off. The caller holds f.mu.
func (f *Fake) stop(method, name string) error {
	if err := f.record(method, name); err != nil {
		return err
	}
	d, err := f.domain(name)
	if err != nil {
		return err
	}
	if d.state == virt.StateShutOff {
		return failure.New(failure.Conflict, "domain %s is not running", name)
	}
	d.state, d.liveXML, d.devices = virt.StateShutOff, "", nil
	if d.transient {
		delete(f.domains, name)
	}
	return nil
}

// UndefineDomain follows libvirt: a domain with snapshots needs
// UndefineSnapshots, a domain with NVRAM needs UndefineNVRAM or
// UndefineKeepNVRAM. UndefineNVRAM keeps a pool volume at the NVRAM path, as
// libvirt does: the qemu driver unlinks the file and does not tell the
// storage driver, so the pool still holds the name.
// A running domain becomes transient: it keeps running and goes away when it
// stops.
func (f *Fake) UndefineDomain(name string, flags virt.UndefineFlags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("UndefineDomain", name); err != nil {
		return err
	}
	d, err := f.domain(name)
	if err != nil {
		return err
	}
	if d.transient {
		return failure.New(failure.Conflict, "cannot undefine transient domain %s", name)
	}
	if len(d.snapshots) > 0 && flags&virt.UndefineSnapshots == 0 {
		return failure.New(failure.Conflict, "cannot delete inactive domain %s with %d snapshots", name, len(d.snapshots))
	}
	x, err := domxml.Parse(d.xml)
	if err != nil {
		return failure.Wrap(failure.Failed, err, "undefine %s", name)
	}
	if _, ok := domxml.NVRAMOf(x); ok {
		if flags&(virt.UndefineNVRAM|virt.UndefineKeepNVRAM) == 0 {
			return failure.New(failure.Conflict, "cannot undefine domain %s with nvram", name)
		}
	}
	if d.state == virt.StateShutOff {
		delete(f.domains, name)
	} else {
		d.transient = true
	}
	return nil
}

func (f *Fake) DomainState(name string) (virt.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DomainState", name); err != nil {
		return virt.StateOther, err
	}
	d, err := f.domain(name)
	if err != nil {
		return virt.StateOther, err
	}
	return d.state, nil
}

func (f *Fake) DomainXML(name string, inactive bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DomainXML", name); err != nil {
		return "", err
	}
	d, err := f.domain(name)
	if err != nil {
		return "", err
	}
	if !inactive && d.liveXML != "" {
		return d.liveXML, nil
	}
	return d.xml, nil
}

func (f *Fake) DomainMetadata(name, uri string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DomainMetadata", name, uri); err != nil {
		return "", err
	}
	d, err := f.domain(name)
	if err != nil {
		return "", err
	}
	m, ok := d.metadata[uri]
	if !ok {
		return "", notFound("metadata", uri)
	}
	return m, nil
}

func (f *Fake) SetDomainMetadata(name, uri, key, xml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SetDomainMetadata", name, uri); err != nil {
		return err
	}
	d, err := f.domain(name)
	if err != nil {
		return err
	}
	if xml == "" {
		delete(d.metadata, uri)
	} else {
		d.metadata[uri] = xml
	}
	return nil
}

func (f *Fake) DomainDescription(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DomainDescription", name); err != nil {
		return "", err
	}
	d, err := f.domain(name)
	if err != nil {
		return "", err
	}
	x, err := domxml.Parse(d.xml)
	if err != nil {
		return "", failure.Wrap(failure.Failed, err, "description of %s", name)
	}
	return x.Description, nil
}

func (f *Fake) SetDomainDescription(name, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SetDomainDescription", name); err != nil {
		return err
	}
	d, err := f.domain(name)
	if err != nil {
		return err
	}
	x, err := domxml.Parse(d.xml)
	if err == nil {
		x.Description = description
		d.xml, err = x.Marshal()
	}
	return failure.Wrap(failure.Failed, err, "set description of %s", name)
}

func (d *domain) snapshot(name string) (int, error) {
	i := slices.IndexFunc(d.snapshots, func(s snapshot) bool { return s.name == name })
	if i < 0 {
		return 0, notFound("snapshot", name)
	}
	return i, nil
}

func (f *Fake) CreateSnapshot(domain, name, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CreateSnapshot", domain, name); err != nil {
		return err
	}
	d, err := f.domain(domain)
	if err != nil {
		return err
	}
	if _, err := d.snapshot(name); err == nil {
		return failure.New(failure.Conflict, "snapshot %s of %s already exists", name, domain)
	}
	x, err := domxml.Parse(d.xml)
	if err != nil {
		return failure.Wrap(failure.Failed, err, "domain XML")
	}
	var disks []string
	for _, disk := range domxml.Disks(x) {
		if v := f.volumeAt(disk.Source); v != nil {
			v.internal[name] = slices.Clone(v.data)
			disks = append(disks, disk.Source)
		}
	}
	d.snapshots = append(d.snapshots, snapshot{name: name, description: description, xml: d.xml, state: d.state, disks: disks})
	return nil
}

// volumeAt is the volume at path, or nil. The caller holds f.mu.
func (f *Fake) volumeAt(path string) *volume {
	for _, p := range f.pools {
		for _, v := range p.volumes {
			if v.path == path {
				if v.internal == nil {
					v.internal = map[string][]byte{}
				}
				return v
			}
		}
	}
	return nil
}

// QemuImgSnapshot adds the internal snapshot name with data to the qcow2
// volume, as qemu-img snapshot -c does: libvirt does not know it until its
// metadata is redefined.
func (f *Fake) QemuImgSnapshot(pool, volume, name, data string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.volume(pool, volume)
	if err != nil {
		return err
	}
	if v.internal == nil {
		v.internal = map[string][]byte{}
	}
	v.internal[name] = []byte(data)
	return nil
}

// RedefineSnapshot takes the domain XML from the snapshot description, as
// libvirt does, and replaces a snapshot of the same name.
func (f *Fake) RedefineSnapshot(domain, xml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var x libvirtxml.DomainSnapshot
	if err := x.Unmarshal(xml); err != nil {
		return failure.Wrap(failure.Failed, err, "snapshot XML")
	}
	if err := f.record("RedefineSnapshot", domain, x.Name); err != nil {
		return err
	}
	d, err := f.domain(domain)
	if err != nil {
		return err
	}
	if x.Name == "" || x.Domain == nil || x.CreationTime == "" {
		return failure.New(failure.Failed, "redefine a snapshot of %s: name, creationTime and domain are required", domain)
	}
	dx, err := x.Domain.Marshal()
	if err != nil {
		return failure.Wrap(failure.Failed, err, "snapshot XML")
	}
	s := snapshot{name: x.Name, description: x.Description, xml: dx, state: virt.StateShutOff, disks: internalDisks(&x)}
	if i, err := d.snapshot(x.Name); err == nil {
		d.snapshots[i] = s
	} else {
		d.snapshots = append(d.snapshots, s)
	}
	return nil
}

// internalDisks is the source of each disk the snapshot marks internal; all
// disks when it lists none.
func internalDisks(x *libvirtxml.DomainSnapshot) []string {
	internal := map[string]bool{}
	if x.Disks != nil {
		for _, disk := range x.Disks.Disks {
			internal[disk.Name] = disk.Snapshot == "internal"
		}
	}
	var paths []string
	for _, disk := range domxml.Disks(x.Domain) {
		if on, listed := internal[disk.Target]; (on || !listed && x.Disks == nil) && disk.Source != "" {
			paths = append(paths, disk.Source)
		}
	}
	return paths
}

func (f *Fake) Snapshots(domain string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Snapshots", domain); err != nil {
		return nil, err
	}
	d, err := f.domain(domain)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(d.snapshots))
	for i, s := range d.snapshots {
		names[i] = s.name
	}
	return names, nil
}

// SnapshotXML holds the domain XML at the time of the snapshot, as libvirt's
// does.
func (f *Fake) SnapshotXML(domain, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SnapshotXML", domain, name); err != nil {
		return "", err
	}
	d, err := f.domain(domain)
	if err != nil {
		return "", err
	}
	i, err := d.snapshot(name)
	if err != nil {
		return "", err
	}
	s := d.snapshots[i]
	x, err := domxml.Parse(s.xml)
	if err != nil {
		return "", failure.Wrap(failure.Failed, err, "snapshot XML")
	}
	state := "shutoff"
	if s.state == virt.StateRunning {
		state = "running"
	}
	out, err := (&libvirtxml.DomainSnapshot{Name: s.name, Description: s.description, State: state, Domain: x}).Marshal()
	return out, failure.Wrap(failure.Failed, err, "snapshot XML")
}

// RevertSnapshot restores the domain XML and state of the snapshot, and the
// data of each disk volume it holds. It fails, as qemu-img does, when such a
// volume lacks the internal snapshot.
func (f *Fake) RevertSnapshot(domain, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("RevertSnapshot", domain, name); err != nil {
		return err
	}
	d, err := f.domain(domain)
	if err != nil {
		return err
	}
	i, err := d.snapshot(name)
	if err != nil {
		return err
	}
	for _, path := range d.snapshots[i].disks {
		v := f.volumeAt(path)
		if v == nil {
			continue
		}
		data, ok := v.internal[name]
		if !ok {
			return failure.New(failure.Failed, "disk %s has no internal snapshot %s", path, name)
		}
		v.data = slices.Clone(data)
	}
	d.xml, d.state, d.liveXML = d.snapshots[i].xml, d.snapshots[i].state, ""
	return nil
}

func (f *Fake) DeleteSnapshot(domain, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DeleteSnapshot", domain, name); err != nil {
		return err
	}
	d, err := f.domain(domain)
	if err != nil {
		return err
	}
	i, err := d.snapshot(name)
	if err != nil {
		return err
	}
	for _, path := range d.snapshots[i].disks {
		if v := f.volumeAt(path); v != nil {
			delete(v.internal, name)
		}
	}
	d.snapshots = slices.Delete(d.snapshots, i, i+1)
	return nil
}

func (f *Fake) pool(name string) (*pool, error) {
	p, ok := f.pools[name]
	if !ok {
		return nil, notFound("pool", name)
	}
	return p, nil
}

func (f *Fake) activePool(name string) (*pool, error) {
	p, err := f.pool(name)
	if err == nil && !p.active {
		err = failure.New(failure.Conflict, "storage pool %s is not active", name)
	}
	return p, err
}

func (f *Fake) DefinePool(name, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DefinePool", name); err != nil {
		return err
	}
	if _, ok := f.pools[name]; ok {
		return failure.New(failure.Conflict, "pool %s already exists", name)
	}
	for other, p := range f.pools {
		if p.path == path {
			return failure.New(failure.Failed, "storage source conflict with pool: '%s'", other)
		}
	}
	f.pools[name] = &pool{path: path, capacity: 100 << 30, volumes: map[string]*volume{}}
	return nil
}

func (f *Fake) BuildPool(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("BuildPool", name); err != nil {
		return err
	}
	_, err := f.pool(name)
	return err
}

func (f *Fake) StartPool(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StartPool", name); err != nil {
		return err
	}
	p, err := f.pool(name)
	if err != nil {
		return err
	}
	if p.active {
		return failure.New(failure.Conflict, "storage pool %s is already active", name)
	}
	p.active = true
	return nil
}

func (f *Fake) RefreshPool(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("RefreshPool", name); err != nil {
		return err
	}
	_, err := f.activePool(name)
	return err
}

func (f *Fake) Pools() ([]virt.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Pools"); err != nil {
		return nil, err
	}
	var out []virt.Pool
	for name, p := range f.pools {
		out = append(out, virt.Pool{Name: name, Path: p.path})
	}
	slices.SortFunc(out, func(a, b virt.Pool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *Fake) PoolInfo(name string) (virt.PoolInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("PoolInfo", name); err != nil {
		return virt.PoolInfo{}, err
	}
	p, err := f.pool(name)
	if err != nil {
		return virt.PoolInfo{}, err
	}
	var used uint64
	for _, v := range p.volumes {
		used += uint64(len(v.data))
	}
	return virt.PoolInfo{Active: p.active, Capacity: p.capacity, Allocation: used, Available: p.capacity - min(used, p.capacity)}, nil
}

func (f *Fake) newVolume(poolName, xml string) (*pool, string, *volume, error) {
	p, err := f.activePool(poolName)
	if err != nil {
		return nil, "", nil, err
	}
	var x libvirtxml.StorageVolume
	if err := x.Unmarshal(xml); err != nil {
		return nil, "", nil, failure.Wrap(failure.Failed, err, "volume XML")
	}
	if _, ok := p.volumes[x.Name]; ok {
		return nil, "", nil, failure.New(failure.Conflict, "storage volume %s exists already", x.Name)
	}
	v := &volume{path: p.path + "/" + x.Name}
	if x.Capacity != nil {
		v.capacity = scale(x.Capacity.Value, x.Capacity.Unit)
	}
	return p, x.Name, v, nil
}

func scale(n uint64, unit string) uint64 {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(unit, "iB"), "B")) {
	case "k":
		return n << 10
	case "m":
		return n << 20
	case "g":
		return n << 30
	case "t":
		return n << 40
	}
	return n
}

func (f *Fake) Volumes(poolName string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Volumes", poolName); err != nil {
		return nil, err
	}
	p, err := f.activePool(poolName)
	if err != nil {
		return nil, err
	}
	names := slices.Collect(maps.Keys(p.volumes))
	slices.Sort(names)
	return names, nil
}

func (f *Fake) CreateVolume(poolName, xml string) (virt.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var x libvirtxml.StorageVolume
	_ = x.Unmarshal(xml)
	if err := f.record("CreateVolume", poolName, x.Name); err != nil {
		return virt.Volume{}, err
	}
	p, name, v, err := f.newVolume(poolName, xml)
	if err != nil {
		return virt.Volume{}, err
	}
	p.volumes[name] = v
	return virt.Volume{Name: name, Path: v.path}, nil
}

func (f *Fake) CreateVolumeFrom(poolName, xml, fromPool, from string) (virt.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var x libvirtxml.StorageVolume
	_ = x.Unmarshal(xml)
	if err := f.record("CreateVolumeFrom", poolName, x.Name, fromPool, from); err != nil {
		return virt.Volume{}, err
	}
	src, err := f.volume(fromPool, from)
	if err != nil {
		return virt.Volume{}, err
	}
	p, name, v, err := f.newVolume(poolName, xml)
	if err != nil {
		return virt.Volume{}, err
	}
	v.data, v.capacity = slices.Clone(src.data), max(v.capacity, src.capacity)
	p.volumes[name] = v
	return virt.Volume{Name: name, Path: v.path}, nil
}

func (f *Fake) volume(poolName, name string) (*volume, error) {
	p, err := f.pool(poolName)
	if err != nil {
		return nil, err
	}
	v, ok := p.volumes[name]
	if !ok {
		return nil, notFound("volume", name)
	}
	return v, nil
}

func (f *Fake) UploadVolume(pool, name string, r io.Reader, length uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("UploadVolume", pool, name); err != nil {
		return err
	}
	v, err := f.volume(pool, name)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(length)))
	if err != nil {
		return failure.Wrap(failure.Failed, err, "upload to volume %s", name)
	}
	v.data = data
	return nil
}

func (f *Fake) DownloadVolume(pool, name string, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DownloadVolume", pool, name); err != nil {
		return err
	}
	v, err := f.volume(pool, name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewReader(v.data))
	return failure.Wrap(failure.Failed, err, "download volume %s", name)
}

func (f *Fake) DeleteVolume(pool, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DeleteVolume", pool, name); err != nil {
		return err
	}
	if _, err := f.volume(pool, name); err != nil {
		return err
	}
	delete(f.pools[pool].volumes, name)
	return nil
}

func (f *Fake) VolumePath(pool, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumePath", pool, name); err != nil {
		return "", err
	}
	v, err := f.volume(pool, name)
	if err != nil {
		return "", err
	}
	return v.path, nil
}

func (f *Fake) VolumeInfo(pool, name string) (virt.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumeInfo", pool, name); err != nil {
		return virt.VolumeInfo{}, err
	}
	v, err := f.volume(pool, name)
	if err != nil {
		return virt.VolumeInfo{}, err
	}
	used := uint64(len(v.data))
	return virt.VolumeInfo{Capacity: max(v.capacity, used), Allocation: used}, nil
}

func (f *Fake) VolumeByPath(path string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("VolumeByPath", path); err != nil {
		return "", "", err
	}
	for poolName, p := range f.pools {
		for name, v := range p.volumes {
			if v.path == path {
				return poolName, name, nil
			}
		}
	}
	return "", "", notFound("volume at", path)
}

func (f *Fake) DHCPLeases(network, mac string) ([]virt.DHCPLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DHCPLeases", network); err != nil {
		return nil, err
	}
	var out []virt.DHCPLease
	for _, l := range f.leases[network] {
		if mac == "" || strings.EqualFold(l.MAC, mac) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *Fake) running(name string) (*domain, error) {
	d, err := f.domain(name)
	if err == nil && d.state != virt.StateRunning {
		err = failure.New(failure.Conflict, "domain %s is not running", name)
	}
	return d, err
}

func (f *Fake) Screenshot(domain string, w io.Writer) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Screenshot", domain); err != nil {
		return "", err
	}
	d, err := f.running(domain)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(d.screenshot); err != nil {
		return "", failure.Wrap(failure.Failed, err, "screenshot of %s", domain)
	}
	return d.screenMIME, nil
}

func (f *Fake) SendKey(domain string, keycodes []uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SendKey", domain); err != nil {
		return err
	}
	d, err := f.running(domain)
	if err != nil {
		return err
	}
	d.keys = append(d.keys, slices.Clone(keycodes))
	return nil
}

func (f *Fake) AttachDevice(domain, xml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("AttachDevice", domain); err != nil {
		return err
	}
	d, err := f.running(domain)
	if err != nil {
		return err
	}
	d.devices = append(d.devices, xml)
	return nil
}

// DetachDevice removes a device attached with the same XML.
func (f *Fake) DetachDevice(domain, xml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DetachDevice", domain); err != nil {
		return err
	}
	d, err := f.running(domain)
	if err != nil {
		return err
	}
	i := slices.Index(d.devices, xml)
	if i < 0 {
		return failure.New(failure.NotFound, "no matching device on %s", domain)
	}
	d.devices = slices.Delete(d.devices, i, i+1)
	return nil
}
