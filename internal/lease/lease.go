// Package lease gives out and takes back disposable VMs: a domain on a thin
// overlay of a stock image (docs/DESIGN.md, "Lease lifecycle").
package lease

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/console"
	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

const (
	// Lifetime is how long a lease lives when werft lease has no --for.
	Lifetime = 4 * time.Hour
	// MinLifetime is longer than the address and SSH waits of a start
	// together, so the reap pass of another werft run does not release a
	// lease that still boots.
	MinLifetime = 15 * time.Minute
	// MaxLifetime caps the lifetime at lease time; werft extend adds more.
	MaxLifetime = 24 * time.Hour
	// MinMemory is the least memory werft lease --memory gives a VM. It
	// catches a size given in GiB where MiB is meant.
	MinMemory = 256 << 20
	// BootTimeout is how long a lease waits for an address, and then for
	// SSH.
	BootTimeout = 5 * time.Minute
	// Namespace is the namespace of the werft metadata of a lease domain.
	Namespace = "https://github.com/hk9890/werft/lease"
	// MinPoolFree is the free space a pool must have for the overlay of a
	// new lease, which grows as the guest writes.
	MinPoolFree = 1 << 30
)

// Lease is what an agent gets: how to reach the VM, and until when.
type Lease struct {
	ID        string    `json:"id"`
	Domain    string    `json:"domain"`
	Image     string    `json:"image"`
	Tier      string    `json:"tier"`
	Stock     string    `json:"stock"`
	Owner     string    `json:"owner"`
	State     string    `json:"state,omitempty"`
	User      string    `json:"user"`
	Address   string    `json:"address"`
	Key       string    `json:"key"`
	SSHConfig string    `json:"ssh_config"`
	SSH       string    `json:"ssh"`
	Created   time.Time `json:"created"`
	Expiry    time.Time `json:"expiry"`

	pool, overlay string
	// state is the state directory of the lease: only a werft run with that
	// state directory lists, extends and reaps it.
	state string
}

// Env is what a lease runs with.
type Env struct {
	Ctx context.Context
	H   virt.Hypervisor
	Dir state.Dir
	// Prefix starts the name of every lease domain.
	Prefix string
	// MemoryBudget is the free host memory, in bytes, that a new lease
	// leaves.
	MemoryBudget uint64
	// Shell logs in with the ssh_config at config.
	Shell func(config string) guest.Shell
	// NewKey makes an ed25519 key pair at path.
	NewKey func(path string) error
	// Global takes the global lock.
	Global func() (release func(), err error)
	// View opens a viewer on the domain; nil opens none.
	View func(domain string)
	// Secrets and Terminal read the disk password of a LUKS stock image.
	Secrets  secret.Store
	Terminal secret.Terminal
	Now      func() time.Time
	Sleep    func(time.Duration)
	Out      io.Writer
}

func (e Env) printf(format string, args ...any) { _, _ = fmt.Fprintf(e.Out, format+"\n", args...) }

// Options are the choices of werft lease. A zero Memory or CPUs keeps the
// value of the stock definition; a zero Lifetime is Lifetime.
type Options struct {
	Owner string
	// Memory is the memory of the VM in bytes.
	Memory   uint64
	CPUs     uint
	Lifetime time.Duration
}

// Resolve is the stock version stock of image and tier in the images.toml
// at catalog; with stock empty, the newest one. With tier empty, the stock
// version gives the tier.
func Resolve(catalog, img, tier, stock string) (image.Record, error) {
	records, err := image.Load(catalog)
	if err != nil {
		return image.Record{}, err
	}
	if stock != "" {
		for _, r := range records {
			if r.Name != stock {
				continue
			}
			if r.Image != img {
				return image.Record{}, failure.New(failure.Usage, "stock version %s is of image %s, not of image %s", stock, r.Image, img)
			}
			if tier != "" && r.Tier != tier {
				return image.Record{}, failure.New(failure.Usage, "stock version %s is of tier %s, not of tier %s", stock, r.Tier, tier)
			}
			return r, nil
		}
		return image.Record{}, failure.New(failure.NotFound, "no stock version %s; werft images lists them", stock)
	}
	for i := len(records) - 1; i >= 0; i-- {
		if r := records[i]; r.Image == img && r.Tier == tier {
			return r, nil
		}
	}
	return image.Record{}, failure.New(failure.NotFound, "no stock image %s of tier %s; werft images lists them", img, tier)
}

// Start leases a VM from the stock version rec with the options o, and
// returns when it accepts an SSH login with the lease key. A failed start
// releases what it made.
func Start(e Env, rec image.Record, o Options) (l Lease, err error) {
	if rec.Domain == "" {
		return Lease{}, failure.New(failure.Unsupported, "%s has no domain definition for leases; seal %s again", rec.Name, rec.Source)
	}
	tmpl, err := domxml.Parse(rec.Domain)
	if err != nil {
		return Lease{}, failure.Wrap(failure.Failed, err, "the domain definition of %s", rec.Name)
	}
	if err := domxml.Resize(tmpl, o.Memory, o.CPUs); err != nil {
		return Lease{}, failure.Wrap(failure.Unsupported, err, "set the memory or the vCPUs of a lease of %s", rec.Name)
	}
	if o.Lifetime == 0 {
		o.Lifetime = Lifetime
	}
	password, err := diskPassword(e, rec)
	if err != nil {
		return Lease{}, err
	}
	l, err = reserve(e, rec, o, tmpl)
	if err != nil {
		return Lease{}, err
	}
	booted := false
	defer func() {
		if err != nil {
			note := ""
			if booted && e.Ctx.Err() == nil {
				note = evidence(e, l.Domain)
			}
			err = abandon(e, l, note, err)
		}
	}()
	e.printf("Booting %s (headless, no window)", l.Domain)
	if err = e.H.StartDomain(l.Domain); err != nil {
		return l, err
	}
	booted = true
	l.State = virt.StateRunning.String()
	if e.View != nil {
		e.View(l.Domain)
	}
	r := (&pipeline.Run{Ctx: e.Ctx, Sleep: e.Sleep}).Init()
	if rec.LUKS {
		if err = steps.TypeDiskPassword(r.Ctx, e.H, l.Domain, password, r.Sleep, e.Out); err != nil {
			return l, err
		}
	}
	if l.Address, err = address(e, r, l.Domain, rec.Systemd); err != nil {
		return l, err
	}
	addr := guest.Addr{User: l.User, Host: l.Address}
	if err = guest.WriteHostConfig(l.SSHConfig, l.ID, addr, l.Key); err != nil {
		return l, failure.Wrap(failure.Failed, err, "write %s", l.SSHConfig)
	}
	e.printf("Waiting up to %v for SSH at %s", BootTimeout, addr)
	shell := e.Shell(l.SSHConfig)
	err = r.PollLogin(BootTimeout, "no SSH answer from "+l.Domain+" at "+l.Address, func() bool {
		return shell.Run(r.Ctx, guest.Addr{User: l.User, Host: l.ID}, "true", nil, io.Discard, io.Discard) == nil
	})
	if rec.LUKS && failure.Is(err, failure.Timeout) {
		err = failure.Wrap(failure.Timeout, err, "the disk can have refused the typed password, and nothing types it twice; check the disk password of %s", rec.Image)
	}
	return l, err
}

// diskPassword reads the disk password of a LUKS stock image before the
// lease changes anything: from the pass entry of the record, else at the
// terminal. An agent has no terminal, so its lease needs the pass entry and
// an unlocked GPG agent; else it fails with secret_unavailable.
func diskPassword(e Env, rec image.Record) (string, error) {
	if !rec.LUKS {
		return "", nil
	}
	spec := secret.Spec{Pass: rec.DiskPasswordPass, Prompt: "Disk password of " + rec.Name}
	v, err := secret.Read(spec, e.Secrets, e.Terminal, func(note string) {
		e.printf("The disk password of %s %s", rec.Name, note)
	})
	if err != nil && rec.DiskPasswordPass == "" {
		return "", failure.Wrap(failure.KindOf(err), err, "%s has an encrypted disk, and its record names no pass entry, so only a terminal gives the password; seal it again with guest_password.pass in the recipe of %s", rec.Name, rec.Image)
	}
	if err != nil {
		return "", failure.Wrap(failure.KindOf(err), err, "%s has an encrypted disk, and pass gives its password only when the GPG agent is unlocked; ask the user to unlock the password store, for example by running pass show %s once, then lease again; do not print or ask for the password yourself", rec.Name, rec.DiskPasswordPass)
	}
	return v, console.Typeable(v)
}

// reserve makes the lease directory with its key pair, the overlay, and the
// domain, under the global lock. The defined domain reserves the name.
func reserve(e Env, rec image.Record, o Options, tmpl *libvirtxml.Domain) (Lease, error) {
	stockDisk, err := e.H.VolumePath(rec.Pool, rec.Disk)
	if err != nil {
		return Lease{}, err
	}
	vars, err := e.H.VolumePath(rec.Pool, rec.NVRAM)
	if err != nil {
		return Lease{}, err
	}
	info, err := e.H.VolumeInfo(rec.Pool, rec.Disk)
	if err != nil {
		return Lease{}, err
	}
	release, err := e.Global()
	if err != nil {
		return Lease{}, err
	}
	defer release()
	if err := stillThere(e.Dir.Images(), rec.Name); err != nil {
		return Lease{}, err
	}
	if err := admit(e, tmpl, rec.Pool); err != nil {
		return Lease{}, err
	}
	id := newID()
	dir := e.Dir.Lease(id)
	now := e.Now().UTC().Truncate(time.Second)
	l := Lease{
		ID: id, Domain: e.Prefix + rec.Image + "-" + rec.Tier + "-" + id,
		Image: rec.Image, Tier: rec.Tier, Stock: rec.Name, Owner: o.Owner, User: rec.User,
		Key: filepath.Join(dir, "id_ed25519"), SSHConfig: filepath.Join(dir, "ssh_config"),
		Created: now, Expiry: now.Add(o.Lifetime), pool: rec.Pool, state: string(e.Dir),
	}
	l.SSH = "ssh -F " + l.SSHConfig + " " + id
	if err := os.MkdirAll(e.Dir.Leases(), 0o700); err != nil {
		return Lease{}, failure.Wrap(failure.Failed, err, "create %s", e.Dir.Leases())
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		kind := failure.Failed
		if errors.Is(err, os.ErrExist) {
			kind = failure.Conflict
		}
		return Lease{}, failure.Wrap(kind, err, "create the lease directory")
	}
	fail := func(err error) (Lease, error) { return Lease{}, abandon(e, l, "", err) }
	if err := e.NewKey(l.Key); err != nil {
		return fail(failure.Wrap(failure.Failed, err, "create the lease key"))
	}
	pub, err := os.ReadFile(l.Key + ".pub")
	if err != nil {
		return fail(failure.Wrap(failure.Failed, err, "read the lease key"))
	}
	e.printf("Creating %s on %s", l.Domain, rec.Name)
	overlay, err := e.H.CreateVolume(rec.Pool, domxml.Overlay(l.Domain+".qcow2", info.Capacity, stockDisk))
	if err != nil {
		return fail(err)
	}
	l.overlay = overlay.Name
	d, err := domxml.Lease(tmpl, l.Domain, stockDisk, overlay.Path, vars, metadata(l, "werft:"), credentials(l.Domain, rec.User, strings.TrimSpace(string(pub))))
	if err != nil {
		return fail(failure.Wrap(failure.Failed, err, "build the lease domain"))
	}
	text, err := d.Marshal()
	if err != nil {
		return fail(failure.Wrap(failure.Failed, err, "build the lease domain"))
	}
	if _, err := e.H.DefineDomain(text); err != nil {
		return fail(err)
	}
	return l, nil
}

// stillThere fails with not_found when the images.toml at catalog has no
// record name any more. The caller holds the global lock, so werft images
// --prune cannot delete the version between this check and the overlay.
func stillThere(catalog, name string) error {
	records, err := image.Load(catalog)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(records, func(r image.Record) bool { return r.Name == name }) {
		return failure.New(failure.NotFound, "the stock version %s is gone from %s; run werft lease again for the newest one", name, catalog)
	}
	return nil
}

// admit refuses a lease of tmpl with no_capacity when the host memory, less
// the memory committed to domains and the memory of the lease, is below the
// budget, or when pool has less than MinPoolFree free. The caller holds the
// global lock until the lease domain is defined, so the next admission
// counts it.
func admit(e Env, tmpl *libvirtxml.Domain, pool string) error {
	need, err := domxml.Memory(tmpl)
	if err != nil {
		return failure.Wrap(failure.Failed, err, "the domain definition of the stock image")
	}
	total, err := e.H.HostTotalMemory()
	if err != nil {
		return err
	}
	held, err := committed(e.H)
	if err != nil {
		return err
	}
	if held > total || need > total-held || total-held-need < e.MemoryBudget {
		return failure.New(failure.NoCapacity, "no capacity: the host has %d MiB of memory, domains hold %d MiB, the lease needs %d MiB, and memory_budget_mib in config.toml keeps %d MiB; release a lease, stop a VM, or lower memory_budget_mib",
			total>>20, held>>20, need>>20, e.MemoryBudget>>20)
	}
	info, err := e.H.PoolInfo(pool)
	if err != nil {
		return err
	}
	if info.Available < MinPoolFree {
		return failure.New(failure.NoCapacity, "no capacity: pool %s has %d MiB free, and a lease needs %d MiB", pool, info.Available>>20, MinPoolFree>>20)
	}
	return nil
}

// committed is the configured memory of every domain that is not shut off,
// and of every lease domain in any state: a lease that is defined but not
// started yet takes its memory when it starts. A domain that goes during the
// count holds nothing.
func committed(h virt.Hypervisor) (uint64, error) {
	domains, err := h.Domains()
	if err != nil {
		return 0, err
	}
	var sum uint64
	for _, d := range domains {
		s, err := h.DomainState(d.Name)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if s == virt.StateShutOff {
			_, err := h.DomainMetadata(d.Name, Namespace)
			if failure.Is(err, failure.NotFound) {
				continue
			}
			if err != nil {
				return 0, err
			}
		}
		text, err := h.DomainXML(d.Name, true)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		dom, err := domxml.Parse(text)
		if err != nil {
			return 0, failure.Wrap(failure.Failed, err, "the definition of %s", d.Name)
		}
		n, err := domxml.Memory(dom)
		if err != nil {
			return 0, failure.Wrap(failure.Failed, err, "the memory of %s", d.Name)
		}
		sum += n
	}
	return sum, nil
}

// address is vsock/<cid> when the guest has SSH over AF_VSOCK, else the IPv4
// DHCP lease of its MAC.
func address(e Env, r *pipeline.Run, domain string, systemd int) (string, error) {
	if systemd >= guest.VsockSystemd {
		cid, _ := virt.DomainAddresses(e.H, domain)
		if cid == "" {
			return "", failure.New(failure.Failed, "the running %s has no vsock CID", domain)
		}
		return cid, nil
	}
	e.printf("Waiting up to %v for a DHCP lease of %s", BootTimeout, domain)
	var ip string
	err := r.Poll(BootTimeout, 2*time.Second, "no DHCP lease for "+domain, func() bool {
		if _, ips := virt.DomainAddresses(e.H, domain); len(ips) > 0 {
			ip = ips[0]
		}
		return ip != ""
	})
	return ip, err
}

// abandon releases a lease whose start failed, and keeps the kind of cause.
// note, empty or starting with "; ", follows "did not start" in the error.
// werft release finds a lease only through its domain, so an overlay left
// without a domain gets the command that deletes it.
func abandon(e Env, l Lease, note string, cause error) error {
	err := remove(e.H, l.Domain, l.pool, l.overlay, e.Dir.Lease(l.ID))
	var left *overlayError
	if errors.As(err, &left) {
		if _, derr := e.H.Domain(l.Domain); failure.Is(derr, failure.NotFound) {
			return failure.Wrap(failure.KindOf(cause), cause, "%s did not start%s; delete its overlay with virsh -c qemu:///system vol-delete --pool %s %s: %v", l.Domain, note, left.pool, left.volume, err)
		}
	}
	if err != nil {
		return failure.Wrap(failure.KindOf(cause), cause, "%s did not start%s; remove what is left with werft release %s: %v", l.Domain, note, l.ID, err)
	}
	return failure.Wrap(failure.KindOf(cause), cause, "%s did not start%s; it is released", l.Domain, note)
}

// metadata is the werft element of a lease domain: owner, created, expiry,
// stock version and state directory. With prefix "werft:" it declares the namespace, for a
// domain definition. With prefix "" it has none, as SetDomainMetadata
// wants: libvirt adds the namespace and refuses an element that declares
// its prefix already.
func metadata(l Lease, prefix string) string {
	var b strings.Builder
	if prefix == "" {
		b.WriteString("<lease>")
	} else {
		fmt.Fprintf(&b, "<%slease xmlns:%s=%q>", prefix, strings.TrimSuffix(prefix, ":"), Namespace)
	}
	for _, f := range [][2]string{
		{"owner", l.Owner}, {"created", l.Created.Format(time.RFC3339)},
		{"expiry", l.Expiry.Format(time.RFC3339)}, {"stock", l.Stock}, {"state", l.state},
	} {
		b.WriteString("<" + prefix + f[0] + ">")
		_ = xml.EscapeText(&b, []byte(f[1]))
		b.WriteString("</" + prefix + f[0] + ">")
	}
	b.WriteString("</" + prefix + "lease>")
	return b.String()
}

// credentials give the lease its hostname, a new machine-id, and the lease
// key for user: over vsock for every user through systemd-ssh-generator,
// and over TCP in the authorized_keys of user. systemd-tmpfiles appends it
// there from werft.ssh.authorized_keys, which its service imports only with
// the drop-in.
func credentials(hostname, user, pub string) []domxml.Credential {
	home := "/home/" + user
	tmpfiles := fmt.Sprintf("d %[1]s/.ssh 0700 %[2]s %[2]s -\nf %[1]s/.ssh/authorized_keys 0600 %[2]s %[2]s -\nw+^ %[1]s/.ssh/authorized_keys - - - - werft.ssh.authorized_keys\n", home, user)
	return []domxml.Credential{
		{Name: "system.hostname", Value: []byte(hostname)},
		{Name: "system.machine_id", Value: []byte(newMachineID())},
		{Name: "ssh.ephemeral-authorized_keys-all", Value: []byte(pub + "\n")},
		{Name: "werft.ssh.authorized_keys", Value: []byte("\n" + pub + "\n")},
		{Name: "tmpfiles.extra", Value: []byte(tmpfiles)},
		{Name: "systemd.unit-dropin.systemd-tmpfiles-setup.service", Value: []byte("[Service]\nImportCredential=werft.*\n")},
	}
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var idPattern = regexp.MustCompile(`^[a-z0-9]{6}$`)

// newID is 6 random characters of idAlphabet. Tests replace it.
var newID = func() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

func newMachineID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Release destroys the lease id: its domain with its NVRAM, its overlay and
// its lease directory. It acts only on a domain with werft lease metadata.
// Each step accepts "already gone", so a second release, or one after a
// half-done release, succeeds. It returns the domain it released, empty
// when there was none.
func Release(h virt.Hypervisor, dir state.Dir, prefix, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", failure.New(failure.Usage, "%q is no lease id: werft lease prints one, 6 letters and digits", id)
	}
	name, err := find(h, prefix, id)
	if err != nil {
		return "", err
	}
	var pool, overlay string
	if name != "" {
		if pool, overlay, err = overlayOf(h, name); err != nil {
			return "", err
		}
	}
	if err := remove(h, name, pool, overlay, dir.Lease(id)); err != nil {
		return name, failure.Wrap(failure.Failed, err, "release %s", id)
	}
	return name, nil
}

// Released is one lease that a release by owner destroyed.
type Released struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
}

// ReleaseOwner releases every lease of List with the owner. A release that
// fails does not stop the others: the error names each lease that failed,
// with the kind of the first failure.
func ReleaseOwner(e Env, owner string) ([]Released, error) {
	leases, err := List(e)
	if err != nil {
		return nil, err
	}
	released := []Released{}
	var first error
	var failed []string
	for _, l := range leases {
		if l.Owner != owner {
			continue
		}
		domain, err := Release(e.H, e.Dir, e.Prefix, l.ID)
		if err != nil {
			first = cmp.Or(first, err)
			failed = append(failed, l.ID)
			continue
		}
		released = append(released, Released{ID: l.ID, Domain: domain})
	}
	return released, failure.Wrap(failure.KindOf(first), first, "the release of lease %s of owner %s failed", strings.Join(failed, ", "), owner)
}

// find is the lease domain of id, empty when there is none.
func find(h virt.Hypervisor, prefix, id string) (string, error) {
	domains, err := h.Domains()
	if err != nil {
		return "", err
	}
	for _, d := range domains {
		if !strings.HasPrefix(d.Name, prefix) || !strings.HasSuffix(d.Name, "-"+id) {
			continue
		}
		_, err := h.DomainMetadata(d.Name, Namespace)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		return d.Name, nil
	}
	return "", nil
}

// overlayOf is the pool and volume of the one disk of the lease domain.
func overlayOf(h virt.Hypervisor, name string) (pool, volume string, err error) {
	text, err := h.DomainXML(name, true)
	if err != nil {
		return "", "", err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return "", "", err
	}
	for _, disk := range domxml.Disks(d) {
		if disk.Source == "" {
			continue
		}
		pool, volume, err = h.VolumeByPath(disk.Source)
		if failure.Is(err, failure.NotFound) {
			return "", "", nil
		}
		return pool, volume, err
	}
	return "", "", nil
}

// remove destroys the domain, deletes the overlay, undefines the domain
// with its NVRAM, and removes the lease directory. The domain is the only
// way to find the overlay again, so when the overlay delete fails, remove
// keeps the domain and the directory, and a second release tries again.
// Else it tries every step and names those that failed.
func remove(h virt.Hypervisor, name, pool, overlay, dir string) error {
	var errs []error
	gone := func(err error) bool { return err == nil || failure.Is(err, failure.NotFound) }
	if name != "" {
		if err := h.DestroyDomain(name); !gone(err) && !failure.Is(err, failure.Conflict) {
			errs = append(errs, fmt.Errorf("destroy %s: %w", name, err))
		}
	}
	if overlay != "" {
		if err := h.DeleteVolume(pool, overlay); !gone(err) {
			return errors.Join(append(errs, &overlayError{pool: pool, volume: overlay, err: err})...)
		}
	}
	if name != "" {
		if err := h.UndefineDomain(name, virt.UndefineNVRAM|virt.UndefineSnapshots); !gone(err) {
			errs = append(errs, fmt.Errorf("undefine %s: %w", name, err))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
	}
	return errors.Join(errs...)
}

// overlayError is an overlay that remove could not delete.
type overlayError struct {
	pool, volume string
	err          error
}

func (o *overlayError) Error() string {
	return fmt.Sprintf("delete volume %s in pool %s: %v", o.volume, o.pool, o.err)
}

func (o *overlayError) Unwrap() error { return o.err }
