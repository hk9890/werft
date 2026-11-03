// Package lease gives out and takes back disposable VMs: a domain on a thin
// overlay of a stock image (docs/DESIGN.md, "Lease lifecycle").
package lease

import (
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
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

const (
	// Lifetime is how long a lease lives.
	Lifetime = 4 * time.Hour
	// BootTimeout is how long a lease waits for an address, and then for
	// SSH.
	BootTimeout = 5 * time.Minute
	// Namespace is the namespace of the werft metadata of a lease domain.
	Namespace = "https://github.com/hk9890/werft/lease"
	// vsockSystemd is the first systemd version with SSH over AF_VSOCK.
	vsockSystemd = 256
)

// Lease is what an agent gets: how to reach the VM, and until when.
type Lease struct {
	ID        string    `json:"id"`
	Domain    string    `json:"domain"`
	Image     string    `json:"image"`
	Tier      string    `json:"tier"`
	Stock     string    `json:"stock"`
	Owner     string    `json:"owner"`
	User      string    `json:"user"`
	Address   string    `json:"address"`
	Key       string    `json:"key"`
	SSHConfig string    `json:"ssh_config"`
	SSH       string    `json:"ssh"`
	Created   time.Time `json:"created"`
	Expiry    time.Time `json:"expiry"`

	pool, overlay string
}

// Env is what a lease runs with.
type Env struct {
	Ctx context.Context
	H   virt.Hypervisor
	Dir state.Dir
	// Prefix starts the name of every lease domain.
	Prefix string
	// Shell logs in with the ssh_config at config.
	Shell func(config string) guest.Shell
	// NewKey makes an ed25519 key pair at path.
	NewKey func(path string) error
	// Global takes the global lock.
	Global func() (release func(), err error)
	// View opens a viewer on the domain; nil opens none.
	View  func(domain string)
	Now   func() time.Time
	Sleep func(time.Duration)
	Out   io.Writer
}

func (e Env) printf(format string, args ...any) { _, _ = fmt.Fprintf(e.Out, format+"\n", args...) }

// Resolve is the newest stock version of image and tier in the images.toml
// at catalog.
func Resolve(catalog, img, tier string) (image.Record, error) {
	records, err := image.Load(catalog)
	if err != nil {
		return image.Record{}, err
	}
	for i := len(records) - 1; i >= 0; i-- {
		if r := records[i]; r.Image == img && r.Tier == tier {
			return r, nil
		}
	}
	return image.Record{}, failure.New(failure.NotFound, "no stock image %s of tier %s; werft images lists them", img, tier)
}

// Start leases a VM from the stock version rec for owner, and returns when
// it accepts an SSH login with the lease key. A failed start releases what
// it made.
func Start(e Env, rec image.Record, owner string) (l Lease, err error) {
	if rec.Domain == "" {
		return Lease{}, failure.New(failure.Unsupported, "%s has no domain definition for leases; seal %s again", rec.Name, rec.Source)
	}
	tmpl, err := domxml.Parse(rec.Domain)
	if err != nil {
		return Lease{}, failure.Wrap(failure.Failed, err, "the domain definition of %s", rec.Name)
	}
	l, err = reserve(e, rec, owner, tmpl)
	if err != nil {
		return Lease{}, err
	}
	defer func() {
		if err != nil {
			err = abandon(e, l, err)
		}
	}()
	e.printf("Booting %s (headless, no window)", l.Domain)
	if err = e.H.StartDomain(l.Domain); err != nil {
		return l, err
	}
	if e.View != nil {
		e.View(l.Domain)
	}
	r := (&pipeline.Run{Ctx: e.Ctx, Sleep: e.Sleep}).Init()
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
	return l, err
}

// reserve makes the lease directory with its key pair, the overlay, and the
// domain, under the global lock. The defined domain reserves the name.
func reserve(e Env, rec image.Record, owner string, tmpl *libvirtxml.Domain) (Lease, error) {
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
	id := newID()
	dir := e.Dir.Lease(id)
	now := e.Now().UTC().Truncate(time.Second)
	l := Lease{
		ID: id, Domain: e.Prefix + rec.Image + "-" + rec.Tier + "-" + id,
		Image: rec.Image, Tier: rec.Tier, Stock: rec.Name, Owner: owner, User: rec.User,
		Key: filepath.Join(dir, "id_ed25519"), SSHConfig: filepath.Join(dir, "ssh_config"),
		Created: now, Expiry: now.Add(Lifetime), pool: rec.Pool,
	}
	l.SSH = "ssh -F " + l.SSHConfig + " " + id
	if err := os.MkdirAll(e.Dir.Leases(), 0o700); err != nil {
		return Lease{}, failure.Wrap(failure.Failed, err, "create %s", e.Dir.Leases())
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Lease{}, failure.Wrap(failure.Conflict, err, "create the lease directory")
	}
	fail := func(err error) (Lease, error) { return Lease{}, abandon(e, l, err) }
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
	d, err := domxml.Lease(tmpl, l.Domain, stockDisk, overlay.Path, vars, metadata(l), credentials(l.Domain, rec.User, strings.TrimSpace(string(pub))))
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

// address is vsock/<cid> when the guest has SSH over AF_VSOCK, else the IPv4
// DHCP lease of its MAC.
func address(e Env, r *pipeline.Run, domain string, systemd int) (string, error) {
	if systemd >= vsockSystemd {
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
func abandon(e Env, l Lease, cause error) error {
	if err := remove(e.H, l.Domain, l.pool, l.overlay, e.Dir.Lease(l.ID)); err != nil {
		return failure.Wrap(failure.KindOf(cause), cause, "%s did not start; remove what is left with werft release %s: %v", l.Domain, l.ID, err)
	}
	return failure.Wrap(failure.KindOf(cause), cause, "%s did not start and is released", l.Domain)
}

// metadata is the werft element of a lease domain: owner, created, expiry
// and stock version.
func metadata(l Lease) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<werft:lease xmlns:werft=%q>", Namespace)
	for _, f := range [][2]string{
		{"owner", l.Owner}, {"created", l.Created.Format(time.RFC3339)},
		{"expiry", l.Expiry.Format(time.RFC3339)}, {"stock", l.Stock},
	} {
		b.WriteString("<werft:" + f[0] + ">")
		_ = xml.EscapeText(&b, []byte(f[1]))
		b.WriteString("</werft:" + f[0] + ">")
	}
	b.WriteString("</werft:lease>")
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

// newID is 6 random characters of idAlphabet.
func newID() string {
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
// with its NVRAM, and removes the lease directory. The overlay goes before
// the domain, so a release that stops half-way finds it again through the
// domain. It tries every step and names those that failed.
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
			errs = append(errs, fmt.Errorf("delete volume %s in pool %s: %w", overlay, pool, err))
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
