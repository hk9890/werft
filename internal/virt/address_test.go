package virt_test

import (
	"testing"

	"github.com/hk9890/werft/internal/virt"
)

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
