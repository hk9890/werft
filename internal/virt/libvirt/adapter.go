package libvirt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// Adapter implements virt.Hypervisor over one go-libvirt connection.
type Adapter struct {
	l *golibvirt.Libvirt
}

var _ virt.Hypervisor = (*Adapter)(nil)

// kindOf maps a libvirt error code to a failure kind. Anything not listed,
// and an error that is no libvirt error at all, is failure.Failed.
func kindOf(err error) failure.Kind {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return failure.Failed
	}
	switch golibvirt.ErrorNumber(lerr.Code) {
	case golibvirt.ErrNoDomain, golibvirt.ErrNoNetwork, golibvirt.ErrNoStoragePool,
		golibvirt.ErrNoStorageVol, golibvirt.ErrNoDomainSnapshot, golibvirt.ErrNoDomainMetadata,
		golibvirt.ErrDeviceMissing:
		return failure.NotFound
	case golibvirt.ErrOperationInvalid, golibvirt.ErrDomExist, golibvirt.ErrStorageVolExist,
		golibvirt.ErrResourceBusy, golibvirt.ErrStoragePoolBuilt:
		return failure.Conflict
	case golibvirt.ErrOperationTimeout, golibvirt.ErrAgentUnresponsive:
		return failure.Timeout
	case golibvirt.ErrNoSupport, golibvirt.ErrArgumentUnsupported,
		golibvirt.ErrOperationUnsupported, golibvirt.ErrConfigUnsupported:
		return failure.Unsupported
	case golibvirt.ErrOperationFailed:
		// libvirt reports a taken domain or snapshot name, and a domain UUID
		// taken under another name, with this generic code; only the message
		// tells them apart.
		if strings.Contains(lerr.Message, "already exists") || strings.Contains(lerr.Message, "is already defined with uuid") {
			return failure.Conflict
		}
	}
	return failure.Failed
}

func wrap(err error, format string, args ...any) error {
	return failure.Wrap(kindOf(err), err, format, args...)
}

func (a *Adapter) Close() error { return a.l.Disconnect() }

func (a *Adapter) LibVersion() (virt.Version, error) {
	n, err := a.l.ConnectGetLibVersion()
	if err != nil {
		return virt.Version{}, wrap(err, "libvirt version")
	}
	return virt.ParseVersion(n), nil
}

func (a *Adapter) HostTotalMemory() (uint64, error) {
	_, kib, _, _, _, _, _, _, err := a.l.NodeGetInfo()
	return kib << 10, wrap(err, "host memory")
}

func toDomain(d golibvirt.Domain) virt.Domain {
	u := d.UUID
	return virt.Domain{Name: d.Name, UUID: fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])}
}

func (a *Adapter) dom(name string) (golibvirt.Domain, error) {
	d, err := a.l.DomainLookupByName(name)
	return d, wrap(err, "domain %s", name)
}

func (a *Adapter) Domains() ([]virt.Domain, error) {
	ds, _, err := a.l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, wrap(err, "list domains")
	}
	out := make([]virt.Domain, len(ds))
	for i, d := range ds {
		out[i] = toDomain(d)
	}
	return out, nil
}

func (a *Adapter) Domain(name string) (virt.Domain, error) {
	d, err := a.dom(name)
	if err != nil {
		return virt.Domain{}, err
	}
	return toDomain(d), nil
}

func (a *Adapter) DefineDomain(xml string) (virt.Domain, error) {
	d, err := a.l.DomainDefineXMLFlags(xml, 0)
	if err != nil {
		return virt.Domain{}, wrap(err, "define domain")
	}
	return toDomain(d), nil
}

func (a *Adapter) StartDomain(name string) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	_, err = a.l.DomainCreateWithFlags(d, 0)
	return wrap(err, "start %s", name)
}

func (a *Adapter) ShutdownDomain(name string) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainShutdown(d), "shut down %s", name)
}

func (a *Adapter) DestroyDomain(name string) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainDestroy(d), "force off %s", name)
}

func (a *Adapter) UndefineDomain(name string, flags virt.UndefineFlags) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	var f golibvirt.DomainUndefineFlagsValues
	if flags&virt.UndefineNVRAM != 0 {
		f |= golibvirt.DomainUndefineNvram
	}
	if flags&virt.UndefineKeepNVRAM != 0 {
		f |= golibvirt.DomainUndefineKeepNvram
	}
	if flags&virt.UndefineSnapshots != 0 {
		f |= golibvirt.DomainUndefineSnapshotsMetadata
	}
	return wrap(a.l.DomainUndefineFlags(d, f), "undefine %s", name)
}

func (a *Adapter) DomainState(name string) (virt.State, error) {
	d, err := a.dom(name)
	if err != nil {
		return virt.StateOther, err
	}
	s, _, err := a.l.DomainGetState(d, 0)
	if err != nil {
		return virt.StateOther, wrap(err, "state of %s", name)
	}
	switch golibvirt.DomainState(s) {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked:
		return virt.StateRunning, nil
	case golibvirt.DomainPaused, golibvirt.DomainPmsuspended:
		return virt.StatePaused, nil
	case golibvirt.DomainShutdown:
		return virt.StateShuttingDown, nil
	case golibvirt.DomainShutoff:
		return virt.StateShutOff, nil
	case golibvirt.DomainCrashed:
		return virt.StateCrashed, nil
	}
	return virt.StateOther, nil
}

func (a *Adapter) DomainXML(name string, inactive bool) (string, error) {
	d, err := a.dom(name)
	if err != nil {
		return "", err
	}
	var f golibvirt.DomainXMLFlags
	if inactive {
		f = golibvirt.DomainXMLInactive
	}
	x, err := a.l.DomainGetXMLDesc(d, f)
	return x, wrap(err, "XML of %s", name)
}

func (a *Adapter) DomainMetadata(name, uri string) (string, error) {
	d, err := a.dom(name)
	if err != nil {
		return "", err
	}
	m, err := a.l.DomainGetMetadata(d, int32(golibvirt.DomainMetadataElement), golibvirt.OptString{uri}, golibvirt.DomainAffectConfig)
	return m, metadataErr(err, uri, name)
}

// metadataErr is the error of a metadata read. A transient domain has no
// persistent config, so libvirt fails with OPERATION_INVALID; it has no
// metadata, so the read fails with failure.NotFound.
func metadataErr(err error, uri, name string) error {
	if kindOf(err) == failure.Conflict {
		return failure.Wrap(failure.NotFound, err, "metadata %s of %s", uri, name)
	}
	return wrap(err, "metadata %s of %s", uri, name)
}

func (a *Adapter) SetDomainMetadata(name, uri, key, xml string) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	// Only a NULL value removes the element; libvirt parses an empty string
	// as XML and fails.
	var value, keyArg golibvirt.OptString
	if xml != "" {
		value, keyArg = golibvirt.OptString{xml}, golibvirt.OptString{key}
	}
	err = a.l.DomainSetMetadata(d, int32(golibvirt.DomainMetadataElement), value, keyArg,
		golibvirt.OptString{uri}, golibvirt.DomainAffectConfig)
	return wrap(err, "set metadata %s of %s", uri, name)
}

func (a *Adapter) DomainDescription(name string) (string, error) {
	d, err := a.dom(name)
	if err != nil {
		return "", err
	}
	desc, err := a.l.DomainGetMetadata(d, int32(golibvirt.DomainMetadataDescription), nil, golibvirt.DomainAffectConfig)
	if kindOf(err) == failure.NotFound {
		return "", nil
	}
	return desc, wrap(err, "description of %s", name)
}

func (a *Adapter) SetDomainDescription(name, description string) error {
	d, err := a.dom(name)
	if err != nil {
		return err
	}
	var value golibvirt.OptString
	if description != "" {
		value = golibvirt.OptString{description}
	}
	err = a.l.DomainSetMetadata(d, int32(golibvirt.DomainMetadataDescription), value, nil, nil, golibvirt.DomainAffectConfig)
	return wrap(err, "set description of %s", name)
}

func (a *Adapter) snap(domain, name string) (golibvirt.DomainSnapshot, error) {
	d, err := a.dom(domain)
	if err != nil {
		return golibvirt.DomainSnapshot{}, err
	}
	s, err := a.l.DomainSnapshotLookupByName(d, name, 0)
	return s, wrap(err, "snapshot %s of %s", name, domain)
}

func (a *Adapter) CreateSnapshot(domain, name, description string) error {
	d, err := a.dom(domain)
	if err != nil {
		return err
	}
	x, err := (&libvirtxml.DomainSnapshot{Name: name, Description: description}).Marshal()
	if err != nil {
		return fmt.Errorf("snapshot XML: %w", err)
	}
	_, err = a.l.DomainSnapshotCreateXML(d, x, 0)
	return wrap(err, "create snapshot %s of %s", name, domain)
}

func (a *Adapter) RedefineSnapshot(domain, xml string) error {
	d, err := a.dom(domain)
	if err != nil {
		return err
	}
	_, err = a.l.DomainSnapshotCreateXML(d, xml, uint32(golibvirt.DomainSnapshotCreateRedefine))
	return wrap(err, "redefine a snapshot of %s", domain)
}

func (a *Adapter) Snapshots(domain string) ([]string, error) {
	d, err := a.dom(domain)
	if err != nil {
		return nil, err
	}
	ss, _, err := a.l.DomainListAllSnapshots(d, 1, 0)
	if err != nil {
		return nil, wrap(err, "snapshots of %s", domain)
	}
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.Name
	}
	return names, nil
}

func (a *Adapter) SnapshotXML(domain, name string) (string, error) {
	s, err := a.snap(domain, name)
	if err != nil {
		return "", err
	}
	x, err := a.l.DomainSnapshotGetXMLDesc(s, 0)
	return x, wrap(err, "XML of snapshot %s of %s", name, domain)
}

func (a *Adapter) RevertSnapshot(domain, name string) error {
	s, err := a.snap(domain, name)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainRevertToSnapshot(s, 0), "revert %s to %s", domain, name)
}

func (a *Adapter) DeleteSnapshot(domain, name string) error {
	s, err := a.snap(domain, name)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainSnapshotDelete(s, 0), "delete snapshot %s of %s", name, domain)
}

func (a *Adapter) pool(name string) (golibvirt.StoragePool, error) {
	p, err := a.l.StoragePoolLookupByName(name)
	return p, wrap(err, "pool %s", name)
}

func (a *Adapter) DefinePool(name, path string) error {
	x, err := (&libvirtxml.StoragePool{Type: "dir", Name: name, Target: &libvirtxml.StoragePoolTarget{Path: path}}).Marshal()
	if err != nil {
		return fmt.Errorf("pool XML: %w", err)
	}
	_, err = a.l.StoragePoolDefineXML(x, 0)
	return wrap(err, "define pool %s", name)
}

func (a *Adapter) BuildPool(name string) error {
	p, err := a.pool(name)
	if err != nil {
		return err
	}
	return wrap(a.l.StoragePoolBuild(p, 0), "build pool %s", name)
}

func (a *Adapter) StartPool(name string) error {
	p, err := a.pool(name)
	if err != nil {
		return err
	}
	return wrap(a.l.StoragePoolCreate(p, 0), "start pool %s", name)
}

func (a *Adapter) RefreshPool(name string) error {
	p, err := a.pool(name)
	if err != nil {
		return err
	}
	return wrap(a.l.StoragePoolRefresh(p, 0), "refresh pool %s", name)
}

func (a *Adapter) PoolInfo(name string) (virt.PoolInfo, error) {
	p, err := a.pool(name)
	if err != nil {
		return virt.PoolInfo{}, err
	}
	state, capacity, allocation, available, err := a.l.StoragePoolGetInfo(p)
	if err != nil {
		return virt.PoolInfo{}, wrap(err, "info of pool %s", name)
	}
	return virt.PoolInfo{
		Active:     golibvirt.StoragePoolState(state) == golibvirt.StoragePoolRunning,
		Capacity:   capacity,
		Allocation: allocation,
		Available:  available,
	}, nil
}

func (a *Adapter) Pools() ([]virt.Pool, error) {
	ps, _, err := a.l.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, wrap(err, "list pools")
	}
	out := make([]virt.Pool, len(ps))
	for i, p := range ps {
		text, err := a.l.StoragePoolGetXMLDesc(p, 0)
		if err != nil {
			return nil, wrap(err, "XML of pool %s", p.Name)
		}
		var x libvirtxml.StoragePool
		if err := x.Unmarshal(text); err != nil {
			return nil, fmt.Errorf("XML of pool %s: %w", p.Name, err)
		}
		out[i] = virt.Pool{Name: p.Name}
		if x.Target != nil {
			out[i].Path = x.Target.Path
		}
	}
	return out, nil
}

func (a *Adapter) vol(pool, name string) (golibvirt.StorageVol, error) {
	p, err := a.pool(pool)
	if err != nil {
		return golibvirt.StorageVol{}, err
	}
	v, err := a.l.StorageVolLookupByName(p, name)
	return v, wrap(err, "volume %s in pool %s", name, pool)
}

func (a *Adapter) volume(v golibvirt.StorageVol) (virt.Volume, error) {
	path, err := a.l.StorageVolGetPath(v)
	if err != nil {
		return virt.Volume{}, wrap(err, "path of volume %s", v.Name)
	}
	return virt.Volume{Name: v.Name, Path: path}, nil
}

func (a *Adapter) Volumes(pool string) ([]string, error) {
	p, err := a.pool(pool)
	if err != nil {
		return nil, err
	}
	vs, _, err := a.l.StoragePoolListAllVolumes(p, 1, 0)
	if err != nil {
		return nil, wrap(err, "volumes of pool %s", pool)
	}
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = v.Name
	}
	return names, nil
}

func (a *Adapter) CreateVolume(pool, xml string) (virt.Volume, error) {
	p, err := a.pool(pool)
	if err != nil {
		return virt.Volume{}, err
	}
	v, err := a.l.StorageVolCreateXML(p, xml, 0)
	if err != nil {
		return virt.Volume{}, wrap(err, "create volume in pool %s", pool)
	}
	return a.volume(v)
}

func (a *Adapter) CreateVolumeFrom(pool, xml, fromPool, from string) (virt.Volume, error) {
	p, err := a.pool(pool)
	if err != nil {
		return virt.Volume{}, err
	}
	src, err := a.vol(fromPool, from)
	if err != nil {
		return virt.Volume{}, err
	}
	v, err := a.l.StorageVolCreateXMLFrom(p, xml, src, 0)
	if err != nil {
		return virt.Volume{}, wrap(err, "create volume in pool %s from %s", pool, from)
	}
	return a.volume(v)
}

func (a *Adapter) UploadVolume(pool, name string, r io.Reader, length uint64) error {
	v, err := a.vol(pool, name)
	if err != nil {
		return err
	}
	return wrap(a.l.StorageVolUpload(v, r, 0, length, 0), "upload to volume %s", name)
}

func (a *Adapter) DownloadVolume(pool, name string, w io.Writer) error {
	v, err := a.vol(pool, name)
	if err != nil {
		return err
	}
	return wrap(a.l.StorageVolDownload(v, w, 0, 0, 0), "download volume %s", name)
}

func (a *Adapter) DeleteVolume(pool, name string) error {
	v, err := a.vol(pool, name)
	if err != nil {
		return err
	}
	return wrap(a.l.StorageVolDelete(v, 0), "delete volume %s", name)
}

func (a *Adapter) VolumePath(pool, name string) (string, error) {
	v, err := a.vol(pool, name)
	if err != nil {
		return "", err
	}
	vol, err := a.volume(v)
	return vol.Path, err
}

func (a *Adapter) VolumeInfo(pool, name string) (virt.VolumeInfo, error) {
	v, err := a.vol(pool, name)
	if err != nil {
		return virt.VolumeInfo{}, err
	}
	_, capacity, allocation, err := a.l.StorageVolGetInfo(v)
	if err != nil {
		return virt.VolumeInfo{}, wrap(err, "info of volume %s", name)
	}
	return virt.VolumeInfo{Capacity: capacity, Allocation: allocation}, nil
}

func (a *Adapter) VolumeByPath(path string) (string, string, error) {
	v, err := a.l.StorageVolLookupByPath(path)
	if err != nil {
		return "", "", wrap(err, "volume at %s", path)
	}
	return v.Pool, v.Name, nil
}

func (a *Adapter) VolumeBacking(pool, name string) (string, error) {
	v, err := a.vol(pool, name)
	if err != nil {
		return "", err
	}
	text, err := a.l.StorageVolGetXMLDesc(v, 0)
	if err != nil {
		return "", wrap(err, "XML of volume %s", name)
	}
	var x libvirtxml.StorageVolume
	if err := x.Unmarshal(text); err != nil {
		return "", failure.Wrap(failure.Failed, err, "XML of volume %s", name)
	}
	if x.BackingStore == nil {
		return "", nil
	}
	return x.BackingStore.Path, nil
}

func (a *Adapter) DHCPLeases(network, mac string) ([]virt.DHCPLease, error) {
	n, err := a.l.NetworkLookupByName(network)
	if err != nil {
		return nil, wrap(err, "network %s", network)
	}
	var m golibvirt.OptString
	if mac != "" {
		m = golibvirt.OptString{mac}
	}
	ls, _, err := a.l.NetworkGetDhcpLeases(n, m, 1, 0)
	if err != nil {
		return nil, wrap(err, "DHCP leases of network %s", network)
	}
	out := make([]virt.DHCPLease, len(ls))
	for i, l := range ls {
		out[i] = virt.DHCPLease{MAC: first(l.Mac), IP: l.Ipaddr, Hostname: first(l.Hostname), Expiry: l.Expirytime}
	}
	return out, nil
}

func first(s golibvirt.OptString) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func (a *Adapter) Screenshot(domain string, w io.Writer) (string, error) {
	d, err := a.dom(domain)
	if err != nil {
		return "", err
	}
	cw := &countingWriter{w: w}
	mime, err := a.l.DomainScreenshot(d, cw, 0, 0)
	switch {
	case cw.err != nil:
		return "", wrap(cw.err, "screenshot of %s", domain)
	case err == nil && len(mime) > 0:
		return mime[0], nil
	case err == nil || (cw.n > 0 && isEmptyMIMEDecode(err)):
		return imageMIME(cw.head), nil
	}
	return "", wrap(err, "screenshot of %s", domain)
}

// isEmptyMIMEDecode is go-libvirt's failure to decode the MIME type from the
// end-of-stream packet, which is empty: a screenshot that streamed fine. The
// error type is internal to go-libvirt, so only its message identifies it.
func isEmptyMIMEDecode(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "xdr:") && strings.Contains(msg, "EOF while decoding")
}

// countingWriter keeps the first bytes, which name the image format, and the
// first write error. It hides that error from go-libvirt and drops the rest
// of the stream: go-libvirt stops reading on a write error, and the next
// stream packet then blocks the connection for good.
type countingWriter struct {
	w    io.Writer
	n    int
	head []byte
	err  error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return len(p), nil
	}
	if len(c.head) < 8 {
		c.head = append(c.head, p[:min(len(p), 8-len(c.head))]...)
	}
	n, err := c.w.Write(p)
	c.n += n
	c.err = err
	return len(p), nil
}

func imageMIME(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("P6")):
		return "image/x-portable-pixmap"
	}
	return "application/octet-stream"
}

func (a *Adapter) SendKey(domain string, keycodes []uint32) error {
	d, err := a.dom(domain)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainSendKey(d, uint32(golibvirt.KeycodeSetLinux), 0, keycodes, 0), "send key to %s", domain)
}

func (a *Adapter) AttachDevice(domain, xml string) error {
	d, err := a.dom(domain)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainAttachDeviceFlags(d, xml, uint32(golibvirt.DomainDeviceModifyLive)), "attach device to %s", domain)
}

func (a *Adapter) DetachDevice(domain, xml string) error {
	d, err := a.dom(domain)
	if err != nil {
		return err
	}
	return wrap(a.l.DomainDetachDeviceFlags(d, xml, uint32(golibvirt.DomainDeviceModifyLive)), "detach device from %s", domain)
}
