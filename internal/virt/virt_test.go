package virt_test

import (
	"testing"

	"github.com/hk9890/werft/internal/virt"
)

func TestParseVersion(t *testing.T) {
	v := virt.ParseVersion(12000003)
	if v != (virt.Version{Major: 12, Minor: 0, Release: 3}) || v.String() != "12.0.3" {
		t.Errorf("ParseVersion = %v", v)
	}
}

func TestVersionAtLeast(t *testing.T) {
	min := virt.Version{Major: 11, Minor: 4}
	tests := []struct {
		v    virt.Version
		want bool
	}{
		{virt.Version{Major: 12}, true},
		{virt.Version{Major: 11, Minor: 4}, true},
		{virt.Version{Major: 11, Minor: 4, Release: 1}, true},
		{virt.Version{Major: 11, Minor: 3, Release: 9}, false},
		{virt.Version{Major: 10, Minor: 9}, false},
	}
	for _, tt := range tests {
		if got := tt.v.AtLeast(min); got != tt.want {
			t.Errorf("%v.AtLeast(%v) = %v", tt.v, min, got)
		}
	}
}
