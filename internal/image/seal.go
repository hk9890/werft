package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/guest/assets"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
)

// Plain is the tier of a stock image from the provision-base reset point of
// a build VM: the vendor install, prepared for SSH access.
const Plain = "plain"

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

	domain    *libvirtxml.Domain
	disk      domxml.Disk
	nvramPool string
	nvram     string
	nvramPath string
}

// Plan checks that the build VM can be sealed from label now, and changes
// nothing. Only the plain tier exists yet.
func Plan(h virt.Hypervisor, buildVM, label string) (*Seal, error) {
	if label != resetpoint.ProvisionBase || strings.HasSuffix(buildVM, resetpoint.FullSuffix) {
		return nil, failure.New(failure.Usage, "seal makes the %s tier from the reset point '%s' of a build VM; no other tier exists yet", Plain, resetpoint.ProvisionBase)
	}
	if err := resetpoint.CheckRevert(h, buildVM, label); err != nil {
		return nil, err
	}
	text, err := h.DomainXML(buildVM, true)
	if err != nil {
		return nil, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return nil, err
	}
	if domxml.HasLUKSMarker(domxml.Description(d)) {
		return nil, failure.New(failure.Unsupported, "the disk of %s asks for a password at boot; seal cannot type it yet", buildVM)
	}
	s := &Seal{BuildVM: buildVM, Label: label, Image: buildVM, Tier: Plain, domain: d}
	if s.disk, err = onlyDisk(h, d); err != nil {
		return nil, err
	}
	nv, ok := domxml.NVRAMOf(d)
	if !ok || nv.Path == "" {
		return nil, failure.New(failure.Unsupported, "%s has no UEFI NVRAM; a stock image is a disk and its NVRAM", buildVM)
	}
	s.nvramPath = nv.Path
	if s.nvramPool, s.nvram, err = virt.FindNVRAM(h, nv.Path); err != nil {
		return nil, failure.Wrap(failure.KindOf(err), err, "NVRAM %s of %s is in no storage pool", nv.Path, buildVM)
	}
	return s, nil
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
	Now    func() time.Time
	Sleep  func(time.Duration)
	Out    io.Writer
}

func (e Env) printf(format string, args ...any) { _, _ = fmt.Fprintf(e.Out, format+"\n", args...) }

// Run resets the build VM to the label, copies its disk and NVRAM into new
// stock volumes, seals them in a temporary domain, and writes the record.
// A failed run removes the temporary domain and the volumes.
func (s *Seal) Run(e Env) (rec Record, err error) {
	e.printf("Resetting %s to '%s'", s.BuildVM, s.Label)
	if err := resetpoint.Revert(e.H, s.BuildVM, s.Label); err != nil {
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
	rec, err = s.copyVolumes(e)
	if err != nil {
		return Record{}, err
	}
	defined := false
	defer func() {
		if err == nil {
			return
		}
		if defined {
			_ = e.H.DestroyDomain(rec.Name)
			_ = e.H.UndefineDomain(rec.Name, virt.UndefineKeepNVRAM)
		}
		_ = e.H.DeleteVolume(rec.Pool, rec.Disk)
		_ = e.H.DeleteVolume(rec.Pool, rec.NVRAM)
		err = failure.Wrap(failure.KindOf(err), err, "%s is not sealed, and its volumes are removed", rec.Name)
	}()
	if err = s.defineTemporary(e, rec); err != nil {
		return rec, err
	}
	defined = true
	if err = s.seal(e, &rec); err != nil {
		return rec, err
	}
	if err = e.H.UndefineDomain(rec.Name, virt.UndefineKeepNVRAM); err != nil {
		return rec, err
	}
	defined = false
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

// copyVolumes picks the stock name and copies disk and NVRAM under it. It
// holds the global lock, so two seals of one day take two names.
func (s *Seal) copyVolumes(e Env) (Record, error) {
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
	e.printf("Copying the disk and NVRAM of %s to %s in pool %s", s.BuildVM, name, e.Pool)
	info, err := e.H.VolumeInfo(s.disk.Pool, s.disk.Volume)
	if err != nil {
		return Record{}, err
	}
	if _, err := e.H.CreateVolumeFrom(e.Pool, domxml.Volume(rec.Disk, info.Capacity, "qcow2"), s.disk.Pool, s.disk.Volume); err != nil {
		return Record{}, err
	}
	if _, err := e.H.CreateVolumeFrom(e.Pool, domxml.Volume(rec.NVRAM, 0, "qcow2"), s.nvramPool, s.nvram); err != nil {
		_ = e.H.DeleteVolume(e.Pool, rec.Disk)
		return Record{}, err
	}
	return rec, nil
}

// defineTemporary defines the domain the seal boots: the build VM on the
// copies, with a vsock device, and with the key of Env as the only
// credentials for root login.
func (s *Seal) defineTemporary(e Env, rec Record) error {
	disk, err := e.H.VolumePath(rec.Pool, rec.Disk)
	if err != nil {
		return err
	}
	nvram, err := e.H.VolumePath(rec.Pool, rec.NVRAM)
	if err != nil {
		return err
	}
	d, err := domxml.Clone(s.domain, rec.Name, map[string]string{s.disk.Target: disk}, nvram)
	if err != nil {
		return err
	}
	domxml.EnsureVsock(d)
	domxml.SetCredentials(d, []domxml.Credential{
		{Name: "ssh.ephemeral-authorized_keys-all", Value: []byte(e.Key + "\n")},
		{Name: "systemd.unit-dropin.sshd-vsock@.service", Value: hostKeyDropin},
	})
	text, err := d.Marshal()
	if err != nil {
		return err
	}
	_, err = e.H.DefineDomain(text)
	return err
}

// seal boots the temporary domain, runs the seal script as root, and powers
// it off from inside. It fills in the guest facts of rec.
func (s *Seal) seal(e Env, rec *Record) error {
	r := (&pipeline.Run{Ctx: e.Ctx, Sleep: e.Sleep}).Init()
	e.printf("Booting %s to seal it (headless, no window)", rec.Name)
	if err := e.H.StartDomain(rec.Name); err != nil {
		return err
	}
	cid, err := vsockCID(e.H, rec.Name)
	if err != nil {
		return err
	}
	addr := guest.Addr{User: "root", Host: fmt.Sprintf("vsock/%d", cid)}
	run := func(cmd string, stdout io.Writer) error {
		return e.Shell.Run(r.Ctx, addr, cmd, nil, stdout, e.Out)
	}
	err = r.PollLogin(bootTimeout, "no SSH answer from "+rec.Name+" at "+addr.Host, func() bool {
		return e.Shell.Run(r.Ctx, addr, "true", nil, io.Discard, io.Discard) == nil
	})
	if err != nil {
		return err
	}
	e.printf("Removing the machine-id, hostname and SSH host keys of %s", rec.Name)
	var out bytes.Buffer
	if err := run(guest.ScriptCommand(assets.Seal, e.User, e.WerftKey), &out); err != nil {
		return failure.Wrap(failure.Failed, err, "the seal script failed in %s", rec.Name)
	}
	if rec.Family, rec.Systemd, err = guestFacts(out.String()); err != nil {
		return err
	}
	e.printf("Powering %s off from inside", rec.Name)
	_ = run("systemctl poweroff", io.Discard)
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
