//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

// TestLifecycleLive drives werft list, start and stop against a scratch
// domain on qemu:///system, and list against a domain with a vm reset point.
// list also reads every other domain on the host, and changes none.
func TestLifecycleLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-lc-life")

	livetest.Must(t, h.CreateSnapshot(g.Name, "native", ""), "create reset point")
	livetest.Must(t, h.SetDomainDescription(g.Name, domxml.LUKSMarker), "set LUKS marker")
	legacy := legacyDomain(t, h)

	e := listEntry(t, g.Name)
	if e["state"] != "shut off" || e["reset_points"] != float64(1) || e["luks"] != true ||
		e["size_bytes"] != float64(10<<30) || e["used_bytes"] == nil || e["address"] != "" {
		t.Errorf("list entry of the shut-off domain = %v", e)
	}
	if e := listEntry(t, legacy); e["reset_points"] != float64(1) {
		t.Errorf("list entry of the domain with a vm reset point = %v, want 1 reset point", e)
	}
	livetest.Must(t, h.SetDomainDescription(g.Name, ""), "remove LUKS marker")

	doc := werftJSON(t, 0, "start", g.Name)
	addr, _ := doc["address"].(string)
	cid, err := strconv.ParseUint(strings.TrimPrefix(addr, "vsock/"), 10, 32)
	if doc["already_running"] != false || !strings.HasPrefix(addr, "vsock/") || err != nil {
		t.Fatalf("start = %v", doc)
	}
	g.CID = uint32(cid)
	waitSSH(t, g)
	if e := listEntry(t, g.Name); e["state"] != "running" || e["address"] != addr {
		t.Errorf("list entry of the running domain = %v, want address %s", e, addr)
	}
	if doc := werftJSON(t, 0, "start", g.Name); doc["already_running"] != true {
		t.Errorf("second start = %v", doc)
	}

	if doc := werftJSON(t, 0, "stop", g.Name); doc["result"] != "shut_down" {
		t.Errorf("stop = %v", doc)
	}
	if s, err := h.DomainState(g.Name); err != nil || s != virt.StateShutOff {
		t.Fatalf("state after stop = %v, %v", s, err)
	}
	if doc := werftJSON(t, 0, "stop", g.Name); doc["result"] != "already_shut_off" {
		t.Errorf("stop of a shut-off domain = %v", doc)
	}

	werftJSON(t, 0, "start", g.Name)
	if doc := werftJSON(t, 0, "stop", "--force", g.Name); doc["result"] != "forced_off" {
		t.Errorf("stop --force = %v", doc)
	}
	if doc := werftJSON(t, 3, "stop", "werft-test-lc-none"); doc["error"] == nil {
		t.Errorf("stop of an unknown domain = %v", doc)
	}
}

// legacyDomain defines a domain that is never started, with a raw NVRAM
// path in a temporary directory and a vm reset point next to it: a copy
// named <nvram>.<label>.bak. vm wrote these copies with sudo cp, outside the
// storage API, so werft must find them in the directory.
func legacyDomain(t *testing.T, h virt.Hypervisor) string {
	t.Helper()
	const name = "werft-test-lc-legacy"
	livetest.RemoveDomain(t, h, name)
	dir := t.TempDir()
	nvram := filepath.Join(dir, name+"_VARS.fd")
	livetest.Must(t, os.WriteFile(nvram+".old.bak", nil, 0o600), "write legacy reset point")
	_, err := h.DefineDomain(fmt.Sprintf(`<domain type='kvm'><name>%s</name><memory unit='MiB'>256</memory><os>
<type arch='x86_64' machine='q35'>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='raw'>%s</nvram></os><features><acpi/></features></domain>`, name, nvram))
	livetest.Must(t, err, "define "+name)
	t.Cleanup(func() { livetest.RemoveDomain(t, h, name) })
	return name
}

func werftJSON(t *testing.T, code int, args ...string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{
		stdout: &out, stderr: &errOut, dial: dialLibvirt, sleep: time.Sleep,
		openViewer: func(string) error { t.Fatal("a viewer opened"); return nil },
	}
	if got := c.run(append([]string{"--json"}, args...)); got != code {
		t.Fatalf("werft %q exited %d, want %d\nstdout: %s\nstderr: %s", args, got, code, out.String(), errOut.String())
	}
	var doc map[string]any
	livetest.Must(t, json.Unmarshal(out.Bytes(), &doc), "parse the output of werft "+strings.Join(args, " "))
	return doc
}

func listEntry(t *testing.T, name string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdout: &out, stderr: &errOut, dial: dialLibvirt}
	if code := c.run([]string{"--json", "list"}); code != 0 {
		t.Fatalf("werft list exited %d: %s", code, errOut.String())
	}
	var entries []map[string]any
	livetest.Must(t, json.Unmarshal(out.Bytes(), &entries), "parse the output of werft list")
	for _, e := range entries {
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("werft list has no %s: %s", name, out.String())
	return nil
}

// waitSSH waits until the guest answers, so it has booted far enough to
// react to an ACPI shutdown.
func waitSSH(t *testing.T, g *livetest.Guest) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := g.SSH("true"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no SSH over vsock/%d\nconsole:\n%s", g.CID, livetest.ConsoleTail(g.H, g.ConsoleVolume()))
		}
		time.Sleep(5 * time.Second)
	}
}
