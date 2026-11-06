package main

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lease"
)

func TestGuide(t *testing.T) {
	r := runCLI("", false, "guide")
	if r.code != 0 || r.stdout != guide || !strings.HasPrefix(guide, "werft - a guide for agents\n") {
		t.Errorf("guide = %+v", r)
	}
	r = runCLI("", false, "--json", "guide")
	var v map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v["guide"] != guide || len(v) != 1 {
		t.Errorf("--json guide = %q, %v; want {\"guide\": <text>}", r.stdout, err)
	}
	if r := runCLI("", false, "guide", "extra"); r.code != 2 {
		t.Errorf("guide extra = %+v, want exit 2", r)
	}
}

// guideExample is an example call of the guide: an indented werft line.
var guideExample = regexp.MustCompile(`(?m)^[ \t]+werft (?:--json )?([a-z]+)(.*)$`)

// guideNames is every werft mention of the guide: one command, or a list
// such as "werft start, stop or reset".
var guideNames = regexp.MustCompile(`werft (?:--json )?([a-z]+(?:, [a-z]+)*(?:,? (?:and|or) [a-z]+)?)`)

func TestGuideNamesRealCommandsAndFlags(t *testing.T) {
	examples := guideExample.FindAllStringSubmatch(guide, -1)
	if len(examples) < 6 {
		t.Fatalf("found %d werft examples in the guide, want the examples", len(examples))
	}
	for _, call := range examples {
		cmd, ok := lookup(call[1])
		if !ok {
			continue // the check of every werft mention below reports it
		}
		for _, f := range regexp.MustCompile(`--[a-z]+`).FindAllString(call[2], -1) {
			if f != "--json" && !strings.Contains(cmd.args, f) {
				t.Errorf("the guide gives %s flag %s, which werft %s does not take", call[1], f, cmd.args)
			}
		}
	}
	flat := strings.Join(strings.Fields(guide), " ")
	named := map[string]bool{}
	for _, m := range guideNames.FindAllStringSubmatch(flat, -1) {
		for _, name := range regexp.MustCompile(`,? (?:and|or) |, `).Split(m[1], -1) {
			named[name] = true
			if _, ok := lookup(name); !ok {
				t.Errorf("the guide names command %q in %q, which werft does not have", name, m[0])
			}
		}
	}
	for _, name := range []string{"images", "lease", "leases", "extend", "release", "reap", "migrate"} {
		if !named[name] {
			t.Errorf("the check did not see command %q in the guide", name)
		}
	}
}

func TestGuideExitCodesMatchTheContract(t *testing.T) {
	_, table, ok := strings.Cut(guide, "\nExit codes:\n")
	if !ok {
		t.Fatal("the guide has no exit code table")
	}
	got := map[failure.Kind]int{}
	for _, m := range regexp.MustCompile(`(?m)^  (\d+)\s+([a-z_]+)`).FindAllStringSubmatch(table, -1) {
		code, _ := strconv.Atoi(m[1])
		got[failure.Kind(m[2])] = code
	}
	want := map[failure.Kind]int{"ok": 0}
	maps.Copy(want, exitCodes)
	if !maps.Equal(got, want) {
		t.Errorf("guide exit codes = %v, want %v", got, want)
	}
}

func TestGuideListsTheLeaseFields(t *testing.T) {
	_, block, ok := strings.Cut(guide, "fields:\n\n")
	if !ok {
		t.Fatal("the guide has no lease field list")
	}
	block, _, _ = strings.Cut(block, "\n\n")
	var got []string
	for _, line := range strings.Split(block, "\n") {
		got = append(got, strings.Fields(line)[0])
	}
	doc, err := json.Marshal(lease.Lease{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(doc, &fields); err != nil {
		t.Fatal(err)
	}
	if want := slices.Sorted(maps.Keys(fields)); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("guide lease fields = %v, want %v", got, want)
	}
}
