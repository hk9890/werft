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
	"time"

	"github.com/BurntSushi/toml"
)

// Record is one stock version in images.toml. Name is <image>-<tier>-<version>,
// and Version is <YYYYMMDD>[-n].
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
	// Domain is the domain XML a lease of the image starts from: the build
	// VM on the stock volumes, without credentials. JSON leaves it out.
	Domain string `toml:"domain" json:"-"`
}

type catalog struct {
	Images []Record `toml:"image"`
}

// Load reads the records of the images.toml at path, oldest first. A missing
// file has none.
func Load(path string) ([]Record, error) {
	var c catalog
	_, err := toml.DecodeFile(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return c.Images, nil
}

// add appends r to the images.toml at path. The file is replaced as a whole,
// so a reader gets the old records or the new ones. The caller holds the
// global lock.
func add(path string, r Record) error {
	records, err := Load(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(catalog{Images: append(records, r)}); err != nil {
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

// nextName is the first stock name <image>-<tier>-<YYYYMMDD>[-n] of day
// that taken does not report, and its version.
func nextName(image, tier string, day time.Time, taken func(name string) bool) (name, version string) {
	base := day.Format("20060102")
	version = base
	for n := 2; ; n++ {
		name = image + "-" + tier + "-" + version
		if !taken(name) {
			return name, version
		}
		version = fmt.Sprintf("%s-%d", base, n)
	}
}

func hasName(records []Record, name string) bool {
	return slices.ContainsFunc(records, func(r Record) bool { return r.Name == name })
}
