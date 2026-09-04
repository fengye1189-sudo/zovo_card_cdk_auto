package handler

import (
	"reflect"
	"testing"
)

func TestNormalizeLookupCodes(t *testing.T) {
	got := normalizeLookupCodes(append(
		[]string{"sxc-aaaa-bbbb-cccc-dddd", "SXC-AAAA-BBBB-CCCC-DDDD", "ab"},
		splitLookupText("sxc-eeee-ffff-gggg-hhhh\nsxc-iiii-jjjj-kkkk-llll,sxc-eeee-ffff-gggg-hhhh")...,
	))
	want := []string{
		"SXC-AAAA-BBBB-CCCC-DDDD",
		"SXC-EEEE-FFFF-GGGG-HHHH",
		"SXC-IIII-JJJJ-KKKK-LLLL",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeLookupCodes = %#v, want %#v", got, want)
	}
}

func TestSplitLookupTextEmpty(t *testing.T) {
	if got := splitLookupText("  \n"); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
}
