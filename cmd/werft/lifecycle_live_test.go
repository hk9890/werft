//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

// TestLifecycleLive drives werft list, start and stop against a scratch
// domain on qemu:///system. list also reads every other domain on the host,
// and changes none.
func TestLifecycleLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-lc-life")

	// A legacy vm reset point is a copy of the NVRAM named
	// <nvram>.<label>.bak next to it.
	legacy := g.VarsVolume() + ".old.bak"
	livetest.DeleteVolumes(h, legacy)
	t.Cleanup(func() { livetest.DeleteVolumes(h, legacy) })
	_, err := h.CreateVolume(livetest.Pool, fmt.Sprintf("<volume><name>%s</name><capacity>0</capacity></volume>", legacy))
	livetest.Must(t, err, "create legacy reset point")
	livetest.Must(t, h.CreateSnapshot(g.Name, "native", ""), "create reset point")
	livetest.Must(t, h.SetDomainDescription(g.Name, domxml.LUKSMarker), "set LUKS marker")

	e := listEntry(t, g.Name)
	if e["state"] != "shut off" || e["reset_points"] != float64(2) || e["luks"] != true ||
		e["size_bytes"] != float64(10<<30) || e["used_bytes"] == nil || e["address"] != nil {
		t.Errorf("list entry of the shut-off domain = %v", e)
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
