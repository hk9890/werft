package image_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// stock writes a record of each name to catalog, oldest first, and makes its
// two volumes in pool werft.
func stock(t *testing.T, f *virttest.Fake, catalog string, names ...string) {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		parts := strings.SplitN(name, "-", 3)
		fmt.Fprintf(&b, "[[image]]\nname = %q\nimage = %q\ntier = %q\npool = \"werft\"\ndisk = \"%s.qcow2\"\nnvram = \"%s_VARS.qcow2\"\n\n",
			name, parts[0], parts[1], name, name)
		volumes(t, f, name)
	}
	must(t, os.WriteFile(catalog, []byte(b.String()), 0o600))
}

// volumes makes the disk and NVRAM volume of the stock name in pool werft.
func volumes(t *testing.T, f *virttest.Fake, name string) {
	t.Helper()
	for _, v := range []string{name + ".qcow2", name + "_VARS.qcow2"} {
		_, err := f.CreateVolume("werft", "<volume><name>"+v+"</name><capacity>0</capacity></volume>")
		must(t, err)
	}
}

func pruneEnv(f *virttest.Fake, catalog string, leased ...string) image.PruneEnv {
	return image.PruneEnv{
		H: f, Catalog: catalog,
		Leased: func() (map[string]bool, error) {
			m := map[string]bool{}
			for _, name := range leased {
				m[name] = true
			}
			return m, nil
		},
		Global: func() (func(), error) { return func() {}, nil },
		Out:    io.Discard,
	}
}

func names(records []image.Record) []string {
	out := []string{}
	for _, r := range records {
		out = append(out, r.Name)
	}
	return out
}

// TestPrune: an old version without a lease goes with its two volumes and
// its record. The newest version of each image and tier stays, and so do
// the versions a lease domain names or an overlay backs onto, and the
// volumes a running seal reserves.
func TestPrune(t *testing.T) {
	f := world(t)
	must(t, virt.EnsurePool(f, "werft", "/werft"))
	catalog := filepath.Join(t.TempDir(), "images.toml")
	stock(t, f, catalog, "vm-plain-20261101", "vm-full-20261101", "vm-plain-20261102", "vm-plain-20261103", "vm-plain-20261104")
	_, err := f.CreateVolume("werft", domxml.Overlay("werft-vm-plain-abc123.qcow2", 0, "/werft/vm-plain-20261103.qcow2"))
	must(t, err)
	volumes(t, f, "vm-plain-20261105") // a seal reserves it; it has no record yet
	e := pruneEnv(f, catalog, "vm-plain-20261102")

	planned, err := image.Prunable(e)
	if err != nil || !slices.Equal(names(planned), []string{"vm-plain-20261101"}) {
		t.Fatalf("Prunable = %v, %v, want vm-plain-20261101 alone", names(planned), err)
	}
	removed, err := image.Prune(e, planned)
	if err != nil || !slices.Equal(names(removed), []string{"vm-plain-20261101"}) {
		t.Fatalf("Prune = %v, %v", names(removed), err)
	}
	for _, v := range []string{"vm-plain-20261101.qcow2", "vm-plain-20261101_VARS.qcow2"} {
		if _, err := f.VolumePath("werft", v); !failure.Is(err, failure.NotFound) {
			t.Errorf("volume %s is left: %v", v, err)
		}
	}
	records, err := image.Load(catalog)
	if want := []string{"vm-full-20261101", "vm-plain-20261102", "vm-plain-20261103", "vm-plain-20261104"}; err != nil || !slices.Equal(names(records), want) {
		t.Errorf("catalog = %v, %v, want %v", names(records), err, want)
	}
	for _, name := range []string{"vm-full-20261101", "vm-plain-20261102", "vm-plain-20261103", "vm-plain-20261104", "vm-plain-20261105"} {
		if _, err := f.VolumePath("werft", name+".qcow2"); err != nil {
			t.Errorf("volume of %s: %v", name, err)
		}
	}
}

// TestPruneRemovesOnlyWhatWasPlanned: a version that became prunable after
// the confirmation stays, and one a lease took meanwhile stays too.
func TestPruneRemovesOnlyWhatWasPlanned(t *testing.T) {
	f := world(t)
	must(t, virt.EnsurePool(f, "werft", "/werft"))
	catalog := filepath.Join(t.TempDir(), "images.toml")
	stock(t, f, catalog, "vm-plain-20261101", "vm-plain-20261102", "vm-plain-20261103")
	planned, err := image.Prunable(pruneEnv(f, catalog))
	must(t, err)
	removed, err := image.Prune(pruneEnv(f, catalog, "vm-plain-20261101"), planned[1:])
	if err != nil || !slices.Equal(names(removed), []string{"vm-plain-20261102"}) {
		t.Errorf("Prune = %v, %v, want vm-plain-20261102 alone", names(removed), err)
	}
}

// TestPruneKeepsTheRecordWhenAVolumeStays: the next prune tries again.
func TestPruneKeepsTheRecordWhenAVolumeStays(t *testing.T) {
	f := world(t)
	must(t, virt.EnsurePool(f, "werft", "/werft"))
	catalog := filepath.Join(t.TempDir(), "images.toml")
	stock(t, f, catalog, "vm-plain-20261101", "vm-plain-20261102")
	e := pruneEnv(f, catalog)
	planned, err := image.Prunable(e)
	must(t, err)
	f.FailOn("DeleteVolume werft vm-plain-20261101_VARS.qcow2", errors.New("busy"))
	if _, err := image.Prune(e, planned); !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "busy") {
		t.Errorf("Prune = %v, want failed with the cause", err)
	}
	if records, _ := image.Load(catalog); len(records) != 2 {
		t.Errorf("catalog = %v, want both records", names(records))
	}
	f.FailOn("DeleteVolume werft vm-plain-20261101_VARS.qcow2", nil)
	if removed, err := image.Prune(e, planned); err != nil || len(removed) != 1 {
		t.Errorf("second Prune = %v, %v, want vm-plain-20261101", names(removed), err)
	}
}

// TestSealNeverReusesAPrunedName: seal numbers past every name the catalog
// ever held, so a name never comes back with other content.
func TestSealNeverReusesAPrunedName(t *testing.T) {
	f := world(t)
	sh := &shell{f: f}
	e := env(t, f, sh)
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	for range 3 {
		_, err := s.Run(e)
		must(t, err)
	}
	p := pruneEnv(f, e.Catalog)
	planned, err := image.Prunable(p)
	must(t, err)
	if removed, err := image.Prune(p, planned); err != nil || len(removed) != 2 {
		t.Fatalf("Prune = %v, %v, want the two older versions", names(removed), err)
	}
	rec, err := s.Run(e)
	must(t, err)
	if rec.Name != "vm-plain-20261102-4" {
		t.Errorf("seal after prune = %s, want vm-plain-20261102-4", rec.Name)
	}
}
