package virt_test

import (
	"testing"

	"github.com/hk9890/werft/internal/virt"
)

func TestNewestLease(t *testing.T) {
	a := virt.DHCPLease{IP: "192.168.122.5", Expiry: 100}
	b := virt.DHCPLease{IP: "192.168.122.9", Expiry: 200}
	forever := virt.DHCPLease{IP: "192.168.122.7"}
	tests := []struct {
		name   string
		leases []virt.DHCPLease
		want   string
	}{
		{"none", nil, ""},
		{"one", []virt.DHCPLease{a}, a.IP},
		{"one that never expires", []virt.DHCPLease{forever}, forever.IP},
		{"newest last", []virt.DHCPLease{a, b}, b.IP},
		{"newest first", []virt.DHCPLease{b, a}, b.IP},
		{"tie", []virt.DHCPLease{b, {IP: "192.168.122.8", Expiry: 200}}, ""},
		{"never expires beside another", []virt.DHCPLease{a, forever}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := virt.NewestLease(tt.leases); got != tt.want {
				t.Errorf("NewestLease = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewLeases(t *testing.T) {
	old := virt.DHCPLease{IP: "192.168.122.5", Expiry: 100}
	tests := []struct {
		name string
		now  virt.DHCPLease
		want bool
	}{
		{"the same lease", old, false},
		{"renewed", virt.DHCPLease{IP: "192.168.122.5", Expiry: 200}, true},
		{"another address", virt.DHCPLease{IP: "192.168.122.9", Expiry: 100}, true},
		// A lease that never expires shows no renewal.
		{"infinite", virt.DHCPLease{IP: "192.168.122.5", Expiry: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := virt.NewLeases([]virt.DHCPLease{old}, []virt.DHCPLease{tt.now})
			if (len(got) > 0) != tt.want {
				t.Errorf("NewLeases = %+v, want new %v", got, tt.want)
			}
		})
	}
}
