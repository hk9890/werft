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

// List is every lease of e.Prefix and e.Dir: a domain with the lease name
// pattern and werft lease metadata that names e.Dir, or no state directory
// (a lease from before the state element). A lease of another state
// directory, such as one of a live test, is not listed, so a reap never
// takes it for an orphan. The address and user come from the lease
// ssh_config, which a lease that still boots does not have yet.
func List(e Env) ([]Lease, error) {
	domains, err := e.H.Domains()
	if err != nil {
		return nil, err
	}
	pattern := namePattern(e.Prefix)
	leases := []Lease{}
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
		if r.State != "" && r.State != string(e.Dir) {
			continue
		}
		l := Lease{ID: m[3], Domain: d.Name, Image: m[1], Tier: m[2], Stock: r.Stock, Owner: r.Owner, state: string(e.Dir)}
		l.Created, _ = time.Parse(time.RFC3339, r.Created)
		l.Expiry, _ = time.Parse(time.RFC3339, r.Expiry)
		dir := e.Dir.Lease(l.ID)
		l.Key, l.SSHConfig = filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "ssh_config")
		l.SSH = "ssh -F " + l.SSHConfig + " " + l.ID
		l.Address, l.User = readHostConfig(l.SSHConfig)
		leases = append(leases, l)
	}
	return leases, nil
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

// Reap releases, under the global lock, the expired leases, the lease
// domains without a lease directory, and the lease directories without a
// lease domain. It touches only the domains List finds and the directories
// under e.Dir.Leases() named like a lease ID. A release that fails keeps
// the overlay rule of Release; Reap goes on with the next and names every
// failure.
func Reap(e Env) (Reaped, error) {
	release, err := e.Global()
	if err != nil {
		return Reaped{}, err
	}
	defer release()
	leases, err := List(e)
	if err != nil {
		return Reaped{}, err
	}
	now := e.Now()
	r := Reaped{Expired: []Lease{}, OrphanDomains: []string{}, OrphanDirectories: []string{}}
	var errs []error
	ids := map[string]bool{}
	for _, l := range leases {
		ids[l.ID] = true
		dir := e.Dir.Lease(l.ID)
		_, statErr := os.Stat(dir)
		orphan := errors.Is(statErr, os.ErrNotExist)
		if !l.expired(now) && !orphan {
			continue
		}
		if err := releaseDomain(e, l.Domain, dir); err != nil {
			errs = append(errs, err)
			continue
		}
		if l.expired(now) {
			e.printf("Released %s: it expired at %s", l.Domain, l.Expiry.Format(time.RFC3339))
			r.Expired = append(r.Expired, l)
		} else {
			e.printf("Released %s: it has no lease directory", l.Domain)
			r.OrphanDomains = append(r.OrphanDomains, l.Domain)
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
func releaseDomain(e Env, name, dir string) error {
	pool, overlay, err := overlayOf(e.H, name)
	if err != nil {
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
	leases, err := List(e)
	if err != nil {
		return Lease{}, err
	}
	for _, l := range leases {
		if l.ID != id {
			continue
		}
		if l.Expiry.IsZero() {
			return Lease{}, failure.New(failure.Failed, "%s has no readable expiry in its lease metadata", l.Domain)
		}
		if l.expired(e.Now()) {
			return Lease{}, failure.New(failure.NotFound, "lease %s expired at %s; werft lease makes a new one", id, l.Expiry.Format(time.RFC3339))
		}
		l.Expiry = l.Expiry.Add(d)
		if err := e.H.SetDomainMetadata(l.Domain, Namespace, "werft", metadata(l, "")); err != nil {
			return Lease{}, err
		}
		return l, nil
	}
	return Lease{}, failure.New(failure.NotFound, "no lease %s; werft leases lists them", id)
}
