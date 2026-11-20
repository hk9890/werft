// Package image makes and lists the stock images: sealed, immutable copies
// of a build VM reset point that leases start from (docs/DESIGN.md, "Stock
// images and seal").
package image

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/hk9890/werft/internal/failure"
)

// Record is one stock version in images.toml. Name is <image>-<tier>-<version>,
// and Version is <YYYYMMDD>[-n], the UTC date of the seal.
type Record struct {
	Name    string    `toml:"name" json:"name"`
	Image   string    `toml:"image" json:"image"`
	Tier    string    `toml:"tier" json:"tier"`
	Version string    `toml:"version" json:"version"`
	Date    time.Time `toml:"date" json:"date"`
	// Source is the build VM and Label its reset point the image comes from.
	Source string `toml:"source" json:"source"`
	Label  string `toml:"label" json:"label"`
	// Pool holds the volumes Disk and NVRAM.
	Pool  string `toml:"pool" json:"pool"`
	Disk  string `toml:"disk" json:"disk"`
	NVRAM string `toml:"nvram" json:"nvram"`
	// User is the image's own user, with passwordless sudo.
	User    string `toml:"user" json:"user"`
	Family  string `toml:"family" json:"family"`
	Systemd int    `toml:"systemd" json:"systemd"`
	// LUKS is set when the disk asks for a password at boot. A lease reads
	// that password from the keyring item with the attributes
	// DiskPasswordKeyring, else from the pass entry DiskPasswordPass, else
	// at the terminal. Both name where the password is, never the password.
	LUKS                bool              `toml:"luks,omitempty" json:"luks"`
	DiskPasswordKeyring map[string]string `toml:"disk_password_keyring,omitempty" json:"disk_password_keyring,omitempty"`
	DiskPasswordPass    string            `toml:"disk_password_pass,omitempty" json:"disk_password_pass,omitempty"`
	// Domain is the domain XML a lease of the image starts from: the build
	// VM on the stock volumes, without credentials. JSON leaves it out.
	Domain string `toml:"domain" json:"-"`
}

// catalog is the content of images.toml. Removed is the name of every record
// that prune deleted: seal numbers past every name the catalog ever held, so
// a name never comes back with other content. werft before prune ignores it.
// unknown is every key of the file that this werft does not know.
type catalog struct {
	Removed []string `toml:"removed,omitempty"`
	Images  []Record `toml:"image"`
	unknown []string
}

// Load reads the records of the images.toml at path, oldest first. A missing
// file has none.
func Load(path string) ([]Record, error) {
	c, err := load(path)
	return c.Images, err
}

func load(path string) (catalog, error) {
	var c catalog
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return catalog{}, nil
	}
	if err != nil {
		return catalog{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		c.unknown = append(c.unknown, k.String())
	}
	slices.Sort(c.unknown)
	c.unknown = slices.Compact(c.unknown)
	return c, nil
}

// Writable refuses the images.toml at path when it has keys that this werft
// does not know, with unsupported. A command calls it before it asks or
// changes anything; a write checks again under the global lock.
func Writable(path string) error {
	c, err := load(path)
	if err != nil {
		return err
	}
	return c.writable(path)
}

// writable refuses a catalog with unknown keys: a write keeps only the keys
// this werft knows, so it would delete what a newer werft recorded.
func (c catalog) writable(path string) error {
	if len(c.unknown) > 0 {
		return failure.New(failure.Unsupported, "%s has unknown keys %s: a newer werft wrote it; update werft",
			path, strings.Join(c.unknown, ", "))
	}
	return nil
}

// add appends r to the images.toml at path. The caller holds the global
// lock.
func add(path string, r Record) error {
	c, err := load(path)
	if err != nil {
		return err
	}
	if err := c.writable(path); err != nil {
		return err
	}
	c.Images = append(c.Images, r)
	return write(path, c)
}

// drop deletes the records with the names in gone from the images.toml at
// path, and keeps their names in Removed. The caller holds the global lock.
func drop(path string, gone []string) error {
	c, err := load(path)
	if err != nil {
		return err
	}
	if err := c.writable(path); err != nil {
		return err
	}
	c.Images = slices.DeleteFunc(c.Images, func(r Record) bool { return slices.Contains(gone, r.Name) })
	c.Removed = append(c.Removed, gone...)
	return write(path, c)
}

// write replaces the images.toml at path as a whole, so a reader gets the
// old records or the new ones.
func write(path string, c catalog) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// nextName is the stock name <image>-<tier>-<YYYYMMDD>[-n] of day, and its
// version: the first one past every name of that day in held that taken
// does not report. held is every name the catalog ever held, so a deleted
// name never comes back.
func nextName(image, tier string, day time.Time, held []string, taken func(name string) bool) (name, version string) {
	base := day.Format("20060102")
	stem := image + "-" + tier + "-" + base
	n := 1
	for _, h := range held {
		if m, ok := dayNumber(stem, h); ok && m >= n {
			n = m + 1
		}
	}
	for ; ; n++ {
		version = base
		if n > 1 {
			version = fmt.Sprintf("%s-%d", base, n)
		}
		name = image + "-" + tier + "-" + version
		if !taken(name) {
			return name, version
		}
	}
}

// dayNumber is n of the name <stem>[-n]: 1 for the stem alone.
func dayNumber(stem, name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, stem)
	if !ok {
		return 0, false
	}
	if rest == "" {
		return 1, true
	}
	digits, ok := strings.CutPrefix(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n >= 2
}

// held is every name the catalog c ever held.
func (c catalog) held() []string {
	names := slices.Clone(c.Removed)
	for _, r := range c.Images {
		names = append(names, r.Name)
	}
	return names
}
