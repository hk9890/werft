package lease

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

// namePattern matches the domain name of a lease with prefix:
// <prefix><image>-<tier>-<id>.
func namePattern(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `(.+)-(plain|full)-([a-z0-9]{6})$`)
}

// record is the werft metadata of a lease domain.
type record struct {
	Owner   string `xml:"owner"`
	Created string `xml:"created"`
	Expiry  string `xml:"expiry"`
	Stock   string `xml:"stock"`
	State   string `xml:"state"`
}

// Ownership of a lease domain against the state directory of a run.
const (
	// own: the metadata names this state directory.
	own = iota
	// stateless: the metadata names no state directory; the lease is from
	// before the state element. Its state directory is unknown.
	stateless
	// foreign: the metadata names another state directory.
	foreign
)

// found is a lease domain and its ownership.
type found struct {
	Lease
	owner int
}

// scan is every domain with the lease name pattern of e.Prefix and werft
// lease metadata, whatever state directory it names. The address and user
// come from the lease ssh_config, which a lease that still boots does not
// have yet.
func scan(e Env) ([]found, error) {
	domains, err := e.H.Domains()
	if err != nil {
		return nil, err
	}
	pattern := namePattern(e.Prefix)
	var all []found
	for _, d := range domains {
		m := pattern.FindStringSubmatch(d.Name)
		if m == nil {
			continue
		}
		text, err := e.H.DomainMetadata(d.Name, Namespace)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var r record
		if err := xml.Unmarshal([]byte(text), &r); err != nil {
			return nil, failure.Wrap(failure.Failed, err, "the lease metadata of %s", d.Name)
		}
		f := found{Lease: Lease{ID: m[3], Domain: d.Name, Image: m[1], Tier: m[2], Stock: r.Stock, Owner: r.Owner, state: r.State}, owner: ownership(r.State, e.Dir)}
		f.Created, _ = time.Parse(time.RFC3339, r.Created)
		f.Expiry, _ = time.Parse(time.RFC3339, r.Expiry)
		dir := e.Dir.Lease(f.ID)
		f.Key, f.SSHConfig = filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "ssh_config")
		f.SSH = "ssh -F " + f.SSHConfig + " " + f.ID
		f.Address, f.User = readHostConfig(f.SSHConfig)
		all = append(all, f)
	}
	return all, nil
}

// Stocks is the stock version that each domain with werft lease metadata
// names, whatever its name and its state directory: werft images --prune
// keeps those versions.
func Stocks(h virt.Hypervisor) (map[string]bool, error) {
	domains, err := h.Domains()
	if err != nil {
		return nil, err
	}
	stocks := map[string]bool{}
	for _, d := range domains {
		text, err := h.DomainMetadata(d.Name, Namespace)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var r record
		if err := xml.Unmarshal([]byte(text), &r); err != nil {
			return nil, failure.Wrap(failure.Failed, err, "the lease metadata of %s", d.Name)
		}
		stocks[r.Stock] = true
	}
	return stocks, nil
}

// ownership compares the state directory a lease names with dir, also
// after symlinks, so /home and /var/home name one directory.
func ownership(named string, dir state.Dir) int {
	switch {
	case named == "":
		return stateless
	case named == string(dir):
		return own
	}
	a, errA := filepath.EvalSymlinks(named)
	b, errB := filepath.EvalSymlinks(string(dir))
	if errA == nil && errB == nil && a == b {
		return own
	}
	return foreign
}

// List is every lease of e.Prefix that this state directory may list and
// extend: those that name it, and those that name no state directory. Each
// has its domain state; a lease whose domain is gone meanwhile is not
// listed.
func List(e Env) ([]Lease, error) {
	all, err := scan(e)
	if err != nil {
		return nil, err
	}
	leases := []Lease{}
	for _, f := range all {
		if f.owner == foreign {
			continue
		}
		l, err := withState(e, f.Lease)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		leases = append(leases, l)
	}
	return leases, nil
}

// withState is l with the state of its domain. A lease that does not run
// has no address: the address of its last boot is stale, and a start can
// give it a new one. Reap does not read the state, so a domain whose state
// libvirt cannot give does not stop a reap.
func withState(e Env, l Lease) (Lease, error) {
	st, err := e.H.DomainState(l.Domain)
	if err != nil {
		return l, err
	}
	l.State = st.String()
	if st != virt.StateRunning {
		l.Address = ""
	}
	return l, nil
}

// expired is true from the expiry on. A lease without a readable expiry
// never expires: reap keeps what it cannot judge.
func (l Lease) expired(now time.Time) bool {
	return !l.Expiry.IsZero() && !now.Before(l.Expiry)
}

// readHostConfig is the HostName and User of the ssh_config werft lease
// wrote, empty when there is none.
func readHostConfig(path string) (host, user string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		switch key, value, _ := strings.Cut(strings.TrimSpace(s.Text()), " "); key {
		case "HostName":
			host = value
		case "User":
			user = value
		}
	}
	return host, user
}

// Reaped is what a reap released.
type Reaped struct {
	// Expired is every lease released because its expiry passed.
	Expired []Lease `json:"expired"`
	// OrphanDomains is every lease domain released because it had no lease
	// directory.
	OrphanDomains []string `json:"orphan_domains"`
	// OrphanDirectories is every lease directory removed because no lease
	// domain had its ID.
	OrphanDirectories []string `json:"orphan_directories"`
}

// Reap releases, under the global lock:
//   - an expired lease of this state directory or of none;
//   - a lease domain that names this state directory and has no lease
//     directory. A lease without a state directory is never an orphan: its
//     lease directory can be in another state directory;
//   - a directory under e.Dir.Leases() named like a lease ID that no lease
//     domain has, whatever state directory the domain names.
//
// A release that fails keeps the overlay rule of Release; Reap goes on with
// the next and names every failure.
func Reap(e Env) (Reaped, error) {
	release, err := e.Global()
	if err != nil {
		return Reaped{}, err
	}
	defer release()
	all, err := scan(e)
	if err != nil {
		return Reaped{}, err
	}
	now := e.Now()
	r := Reaped{Expired: []Lease{}, OrphanDomains: []string{}, OrphanDirectories: []string{}}
	var errs []error
	ids := map[string]bool{}
	for _, f := range all {
		ids[f.ID] = true
		if f.owner == foreign {
			continue
		}
		dir := e.Dir.Lease(f.ID)
		expired := f.expired(now)
		if !expired {
			_, statErr := os.Stat(dir)
			if f.owner != own || !errors.Is(statErr, os.ErrNotExist) {
				continue
			}
		}
		if err := releaseDomain(e, f.Domain, dir); err != nil {
			errs = append(errs, err)
			continue
		}
		if expired {
			e.printf("Released %s: it expired at %s", f.Domain, f.Expiry.Format(time.RFC3339))
			r.Expired = append(r.Expired, f.Lease)
		} else {
			e.printf("Released %s: it has no lease directory", f.Domain)
			r.OrphanDomains = append(r.OrphanDomains, f.Domain)
		}
	}
	entries, err := os.ReadDir(e.Dir.Leases())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !idPattern.MatchString(entry.Name()) || ids[entry.Name()] {
			continue
		}
		dir := e.Dir.Lease(entry.Name())
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		e.printf("Removed %s: no lease domain has its ID", dir)
		r.OrphanDirectories = append(r.OrphanDirectories, dir)
	}
	if len(errs) > 0 {
		return r, failure.Wrap(failure.Failed, errors.Join(errs...), "reap")
	}
	return r, nil
}

// releaseDomain releases the lease domain name and its lease directory as
// Release does.
// A werft release can remove the domain meanwhile; then remove finds every
// part gone or removes what is left.
func releaseDomain(e Env, name, dir string) error {
	pool, overlay, err := overlayOf(e.H, name)
	if err != nil && !failure.Is(err, failure.NotFound) {
		return fmt.Errorf("release %s: %w", name, err)
	}
	if err := remove(e.H, name, pool, overlay, dir); err != nil {
		return fmt.Errorf("release %s: %w", name, err)
	}
	return nil
}

// Extend moves the expiry of lease id by d, under the global lock, so a
// reap cannot release the lease between the check and the write. An
// expired lease is gone for extend: reap releases it.
func Extend(e Env, id string, d time.Duration) (Lease, error) {
	if !idPattern.MatchString(id) {
		return Lease{}, failure.New(failure.Usage, "%q is no lease id: werft lease prints one, 6 letters and digits", id)
	}
	if d <= 0 {
		return Lease{}, failure.New(failure.Usage, "--for is a positive duration such as 1h or 30m, not %v", d)
	}
	release, err := e.Global()
	if err != nil {
		return Lease{}, err
	}
	defer release()
	all, err := scan(e)
	if err != nil {
		return Lease{}, err
	}
	for _, f := range all {
		if f.owner == foreign || f.ID != id {
			continue
		}
		l := f.Lease
		if l.Expiry.IsZero() {
			return Lease{}, failure.New(failure.Failed, "%s has no readable expiry in its lease metadata", l.Domain)
		}
		if l.expired(e.Now()) {
			return Lease{}, failure.New(failure.NotFound, "lease %s expired at %s; werft lease makes a new one", id, l.Expiry.Format(time.RFC3339))
		}
		l, err := withState(e, l)
		if err != nil {
			return Lease{}, err
		}
		l.Expiry = l.Expiry.Add(d)
		if err := e.H.SetDomainMetadata(l.Domain, Namespace, "werft", metadata(l, "")); err != nil {
			return Lease{}, err
		}
		return l, nil
	}
	return Lease{}, failure.New(failure.NotFound, "no lease %s; werft leases lists them", id)
}
