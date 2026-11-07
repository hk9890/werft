package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/console"
	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/guest/assets"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

const (
	// Plain is the tier of a stock image from the provision-base reset
	// point of a build VM: the vendor install, prepared for SSH access.
	Plain = "plain"
	// Full is the tier of a stock image from the clean reset point of the
	// -full build VM: the state after the bootstrap, secrets included.
	Full = "full"
)

const (
	bootTimeout     = 3 * time.Minute
	shutdownTimeout = 2 * time.Minute
)

// hostKeyDropin makes sshd in the vsock instances of systemd-ssh-generator
// create missing host keys, for the boot of the temporary domain: a copy of
// a cloud image has none.
var hostKeyDropin = []byte("[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n")

// Seal is a checked plan to make a stock version from a reset point of a
// build VM.
type Seal struct {
	BuildVM string
	Label   string
	Image   string
	Tier    string
	// LUKS is set when the disk of the reset point asks for a password at
	// boot: the seal types it in the temporary domain.
	LUKS bool

	domain    *libvirtxml.Domain
	disk      domxml.Disk
	nvramPool string
	nvram     string
	nvramPath string
}

// Plan checks that the build VM can be sealed from label now, and changes
// nothing. The provision-base reset point of a build VM gives the plain
// tier; the clean reset point of its -full build VM gives the full tier.
func Plan(h virt.Hypervisor, buildVM, label string) (*Seal, error) {
	img, tier, err := tierOf(buildVM, label)
	if err != nil {
		return nil, err
	}
	if err := resetpoint.CheckRevert(h, buildVM, label); err != nil {
		return nil, err
	}
	// The revert replaces the definition of the build VM with the one the
	// reset point holds, so the checks read that one.
	text, err := h.SnapshotXML(buildVM, label)
	if err != nil {
		return nil, err
	}
	var snap libvirtxml.DomainSnapshot
	if err := snap.Unmarshal(text); err != nil || snap.Domain == nil {
		return nil, failure.New(failure.Failed, "the reset point '%s' of %s holds no domain definition: %v", label, buildVM, err)
	}
	s := &Seal{BuildVM: buildVM, Label: label, Image: img, Tier: tier}
	if err := s.inspect(h, snap.Domain); err != nil {
		return nil, err
	}
	return s, nil
}

// tierOf is the image and tier that the label of the build VM gives.
func tierOf(buildVM, label string) (img, tier string, err error) {
	base, full := strings.CutSuffix(buildVM, resetpoint.FullSuffix)
	switch {
	case !full && label == resetpoint.ProvisionBase:
		return buildVM, Plain, nil
	case full && base != "" && label == resetpoint.Clean:
		return base, Full, nil
	}
	return "", "", failure.New(failure.Usage, "seal makes the %s tier from the reset point '%s' of a build VM, and the %s tier from the reset point '%s' of its %s build VM",
		Plain, resetpoint.ProvisionBase, Full, resetpoint.Clean, resetpoint.FullSuffix)
}

// reread reads the definition of the build VM after the revert, for the
// values Run uses. It must agree with the reset point Plan checked.
func (s *Seal) reread(h virt.Hypervisor) error {
	text, err := h.DomainXML(s.BuildVM, true)
	if err != nil {
		return err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	planned := *s
	if err := s.inspect(h, d); err != nil {
		return failure.Wrap(failure.Failed, err, "%s after the revert to '%s' differs from the reset point", s.BuildVM, s.Label)
	}
	if s.disk != planned.disk || s.nvramPath != planned.nvramPath || s.nvram != planned.nvram ||
		(planned.nvramPool != "" && s.nvramPool != planned.nvramPool) {
		return failure.New(failure.Failed, "%s after the revert to '%s' has another disk or NVRAM than the reset point; nothing is copied", s.BuildVM, s.Label)
	}
	if s.LUKS != planned.LUKS {
		return failure.New(failure.Failed, "%s after the revert to '%s' differs from the reset point in its LUKS marker; nothing is copied", s.BuildVM, s.Label)
	}
	return nil
}

// inspect reads the one writable disk and the NVRAM of the definition d of
// the build VM, and changes nothing.
func (s *Seal) inspect(h virt.Hypervisor, d *libvirtxml.Domain) error {
	disk, err := onlyDisk(h, d)
	if err != nil {
		return err
	}
	nv, ok := domxml.NVRAMOf(d)
	if !ok || nv.Path == "" {
		return failure.New(failure.Unsupported, "%s has no UEFI NVRAM; a stock image is a disk and its NVRAM", s.BuildVM)
	}
	pool, vol, err := virt.FindNVRAM(h, nv.Path)
	if err != nil {
		return failure.Wrap(failure.KindOf(err), err, "NVRAM %s of %s is in no storage pool", nv.Path, s.BuildVM)
	}
	s.domain, s.disk, s.nvramPath, s.nvramPool, s.nvram = d, disk, nv.Path, pool, vol
	s.LUKS = domxml.HasLUKSMarker(domxml.Description(d))
	return nil
}

// onlyDisk is the one writable disk of d, with its pool and volume. A stock
// image is one disk; a read-only one, such as an installer ISO, stays out.
func onlyDisk(h virt.Hypervisor, d *libvirtxml.Domain) (domxml.Disk, error) {
	var out []domxml.Disk
	for _, disk := range domxml.Disks(d) {
		if disk.Device == "disk" && !disk.Shared {
			out = append(out, disk)
		}
	}
	if len(out) != 1 {
		return domxml.Disk{}, failure.New(failure.Unsupported, "%s has %d writable disks; a stock image has one", d.Name, len(out))
	}
	disk := out[0]
	if disk.Pool == "" {
		pool, vol, err := h.VolumeByPath(disk.Source)
		if err != nil {
			return domxml.Disk{}, failure.Wrap(failure.KindOf(err), err, "disk %s is in no storage pool", disk.Source)
		}
		disk.Pool, disk.Volume = pool, vol
	}
	return disk, nil
}

// Env is what a seal runs with.
type Env struct {
	Ctx context.Context
	H   virt.Hypervisor
	// Shell logs in as root with the private half of Key.
	Shell guest.Shell
	Key   string
	// Pool at PoolPath gets the stock volumes.
	Pool     string
	PoolPath string
	// Catalog is the images.toml the record goes to.
	Catalog string
	// User is the image's own user; WerftKey is the public werft key
	// prepare installed for it, or empty.
	User     string
	WerftKey string
	// Global takes the global lock.
	Global func() (release func(), err error)
	// DiskPassword says where the disk password of a LUKS build VM comes
	// from; Secrets and Terminal read it.
	DiskPassword secret.Spec
	Secrets      secret.Store
	Terminal     secret.Terminal
	Now          func() time.Time
	Sleep        func(time.Duration)
	Out          io.Writer
}

func (e Env) printf(format string, args ...any) { _, _ = fmt.Fprintf(e.Out, format+"\n", args...) }

// Run resets the build VM to the label, reserves the stock name with a
// temporary domain on the stock volumes, copies disk and NVRAM into them,
// seals them in the temporary domain, and writes the record. A failed run
// removes the temporary domain and the volumes, and names what it could not
// remove.
func (s *Seal) Run(e Env) (rec Record, err error) {
	password, err := s.diskPassword(e)
	if err != nil {
		return Record{}, err
	}
	e.printf("Resetting %s to '%s'", s.BuildVM, s.Label)
	if err := resetpoint.Revert(e.H, s.BuildVM, s.Label); err != nil {
		return Record{}, err
	}
	if err := s.reread(e.H); err != nil {
		return Record{}, err
	}
	if s.nvramPool == "" {
		if s.nvramPool, s.nvram, err = virt.NVRAMVolume(e.H, s.nvramPath); err != nil {
			return Record{}, failure.Wrap(failure.KindOf(err), err, "NVRAM %s of %s is in no storage pool", s.nvramPath, s.BuildVM)
		}
	}
	if err := virt.EnsurePool(e.H, e.Pool, e.PoolPath); err != nil {
		return Record{}, err
	}
	rec, err = s.reserve(e)
	if err != nil {
		return Record{}, err
	}
	defer func() {
		if err != nil {
			err = remove(e, rec, err)
		}
	}()
	if err = s.copyVolumes(e, rec); err != nil {
		return rec, err
	}
	if err = s.seal(e, &rec, password); err != nil {
		return rec, err
	}
	if err = e.H.UndefineDomain(rec.Name, virt.UndefineKeepNVRAM); err != nil {
		return rec, err
	}
	release, err := e.Global()
	if err != nil {
		return rec, err
	}
	defer release()
	if err = add(e.Catalog, rec); err != nil {
		return rec, failure.Wrap(failure.Failed, err, "write %s", e.Catalog)
	}
	return rec, nil
}

// diskPassword reads the disk password of a LUKS build VM, before anything
// changes; empty when the disk asks for none. Without a pass entry and
// without a terminal, it fails with secret_unavailable.
func (s *Seal) diskPassword(e Env) (string, error) {
	if !s.LUKS {
		return "", nil
	}
	v, err := secret.Read(e.DiskPassword, e.Secrets, e.Terminal, func(note string) {
		e.printf("The disk password of %s %s", s.BuildVM, note)
	})
	if err != nil {
		return "", failure.Wrap(failure.KindOf(err), err, "the disk of %s asks for a password at boot; seal reads it from the pass entry guest_password.pass of its recipe, else at a terminal", s.BuildVM)
	}
	return v, console.Typeable(v)
}

// remove undoes a failed run: the temporary domain and the stock volumes.
// It keeps the kind of cause and names what is left.
func remove(e Env, rec Record, cause error) error {
	var left []string
	if err := e.H.DestroyDomain(rec.Name); err != nil && !failure.Is(err, failure.Conflict) && !failure.Is(err, failure.NotFound) {
		left = append(left, fmt.Sprintf("domain %s (destroy: %v)", rec.Name, err))
	} else if err := e.H.UndefineDomain(rec.Name, virt.UndefineKeepNVRAM); err != nil && !failure.Is(err, failure.NotFound) {
		left = append(left, fmt.Sprintf("domain %s (undefine: %v)", rec.Name, err))
	}
	for _, v := range []string{rec.Disk, rec.NVRAM} {
		if err := e.H.DeleteVolume(rec.Pool, v); err != nil && !failure.Is(err, failure.NotFound) {
			left = append(left, fmt.Sprintf("volume %s in pool %s (delete: %v)", v, rec.Pool, err))
		}
	}
	if len(left) > 0 {
		return failure.Wrap(failure.KindOf(cause), cause, "%s is not sealed; remove what is left by hand: %s", rec.Name, strings.Join(left, "; "))
	}
	return failure.Wrap(failure.KindOf(cause), cause, "%s is not sealed, and its domain and volumes are removed", rec.Name)
}

// reserve picks the stock name and defines the temporary domain under it,
// on the stock volumes copyVolumes makes. It holds the global lock, so two
// seals of one day take two names. The copy runs without the lock, because
// the domain holds the name.
func (s *Seal) reserve(e Env) (Record, error) {
	release, err := e.Global()
	if err != nil {
		return Record{}, err
	}
	defer release()
	records, err := Load(e.Catalog)
	if err != nil {
		return Record{}, err
	}
	volumes, err := e.H.Volumes(e.Pool)
	if err != nil {
		return Record{}, err
	}
	dir, err := poolPath(e.H, e.Pool)
	if err != nil {
		return Record{}, err
	}
	now := e.Now()
	name, version := nextName(s.Image, s.Tier, now, func(name string) bool {
		_, err := e.H.Domain(name)
		return err == nil || hasName(records, name) ||
			slices.Contains(volumes, name+".qcow2") || slices.Contains(volumes, name+"_VARS.qcow2")
	})
	rec := Record{
		Name: name, Image: s.Image, Tier: s.Tier, Version: version, Date: now.UTC().Truncate(time.Second),
		Source: s.BuildVM, Label: s.Label, Pool: e.Pool, Disk: name + ".qcow2", NVRAM: name + "_VARS.qcow2", User: e.User,
	}
	if s.LUKS {
		rec.LUKS, rec.DiskPasswordPass = true, e.DiskPassword.Pass
	}
	if rec.Domain, err = s.defineTemporary(e, rec, dir); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func poolPath(h virt.Hypervisor, name string) (string, error) {
	pools, err := h.Pools()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(pools, func(p virt.Pool) bool { return p.Name == name })
	if i < 0 {
		return "", failure.New(failure.NotFound, "no storage pool %s", name)
	}
	return pools[i].Path, nil
}

// copyVolumes copies disk and NVRAM of the build VM to the stock volumes.
func (s *Seal) copyVolumes(e Env, rec Record) error {
	e.printf("Copying the disk and NVRAM of %s to %s in pool %s", s.BuildVM, rec.Name, e.Pool)
	info, err := e.H.VolumeInfo(s.disk.Pool, s.disk.Volume)
	if err != nil {
		return err
	}
	if _, err := e.H.CreateVolumeFrom(e.Pool, domxml.Volume(rec.Disk, info.Capacity, "qcow2"), s.disk.Pool, s.disk.Volume); err != nil {
		return err
	}
	_, err = e.H.CreateVolumeFrom(e.Pool, domxml.Volume(rec.NVRAM, 0, "qcow2"), s.nvramPool, s.nvram)
	return err
}

// defineTemporary defines the domain the seal boots: the build VM on the
// stock volumes in dir, with a vsock device, and with the key of Env as the
// only credentials for root login. It returns the definition before the
// credentials, which leases of the image start from.
func (s *Seal) defineTemporary(e Env, rec Record, dir string) (template string, err error) {
	d, err := domxml.Clone(s.domain, rec.Name, map[string]string{s.disk.Target: filepath.Join(dir, rec.Disk)}, filepath.Join(dir, rec.NVRAM))
	if err != nil {
		return "", err
	}
	domxml.EnsureVsock(d)
	domxml.SetCredentials(d, nil)
	if template, err = d.Marshal(); err != nil {
		return "", err
	}
	domxml.SetCredentials(d, []domxml.Credential{
		{Name: "ssh.ephemeral-authorized_keys-all", Value: []byte(e.Key + "\n")},
		{Name: "systemd.unit-dropin.sshd-vsock@.service", Value: hostKeyDropin},
	})
	text, err := d.Marshal()
	if err != nil {
		return "", err
	}
	_, err = e.H.DefineDomain(text)
	return template, err
}

// seal boots the temporary domain and runs the seal script as root. The
// script deletes the SSH host keys and then powers the guest off by itself:
// a new SSH login would start an sshd whose drop-in makes new host keys, and
// every lease of the image would share them. It fills in the guest facts of
// rec.
func (s *Seal) seal(e Env, rec *Record, password string) error {
	r := (&pipeline.Run{Ctx: e.Ctx, Sleep: e.Sleep}).Init()
	e.printf("Booting %s to seal it (headless, no window)", rec.Name)
	if err := e.H.StartDomain(rec.Name); err != nil {
		return err
	}
	if s.LUKS {
		if err := steps.TypeDiskPassword(r.Ctx, e.H, rec.Name, password, r.Sleep, e.Out); err != nil {
			return err
		}
	}
	cid, err := vsockCID(e.H, rec.Name)
	if err != nil {
		return err
	}
	addr := guest.Addr{User: "root", Host: fmt.Sprintf("vsock/%d", cid)}
	err = r.PollLogin(bootTimeout, "no SSH answer from "+rec.Name+" at "+addr.Host, func() bool {
		return e.Shell.Run(r.Ctx, addr, "true", nil, io.Discard, io.Discard) == nil
	})
	if s.LUKS && failure.Is(err, failure.Timeout) {
		return failure.Wrap(failure.Timeout, err, "the disk can have refused the typed password, and nothing types it twice; check the disk password of %s", s.BuildVM)
	}
	if err != nil {
		return err
	}
	e.printf("Removing the machine-id, hostname and SSH host keys of %s; it powers off after", rec.Name)
	var out bytes.Buffer
	if err := e.Shell.Run(r.Ctx, addr, guest.ScriptCommand(assets.Seal, e.User, e.WerftKey), nil, &out, e.Out); err != nil {
		return failure.Wrap(failure.Failed, err, "the seal script failed in %s", rec.Name)
	}
	if rec.Family, rec.Systemd, err = guestFacts(out.String()); err != nil {
		return err
	}
	return r.Poll(shutdownTimeout, 2*time.Second, rec.Name+" did not power off", func() bool {
		state, err := e.H.DomainState(rec.Name)
		return err == nil && state == virt.StateShutOff
	})
}

func vsockCID(h virt.Hypervisor, name string) (uint32, error) {
	text, err := h.DomainXML(name, false)
	if err != nil {
		return 0, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return 0, err
	}
	cid, ok := domxml.VsockCID(d)
	if !ok {
		return 0, failure.New(failure.Failed, "the running %s has no vsock CID", name)
	}
	return cid, nil
}

// guestFacts reads the family and systemd version the seal script prints.
func guestFacts(out string) (family string, systemd int, err error) {
	lines := strings.Fields(out)
	if len(lines) == 2 {
		systemd, err = strconv.Atoi(lines[1])
	}
	if len(lines) != 2 || err != nil {
		return "", 0, failure.New(failure.Failed, "the seal script printed %q, not the guest family and systemd version", out)
	}
	return lines[0], systemd, nil
}
