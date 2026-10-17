package server

import (
	"net/netip"
	"testing"
)

func TestSuggestRange(t *testing.T) {
	sub := netip.MustParsePrefix("10.0.10.0/24")
	a := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	if got := suggestRange(sub, []netip.Addr{a("10.0.10.1"), a("10.0.10.5")}); got != "10.0.10.224/27" {
		t.Errorf("got %q", got)
	}
	if got := suggestRange(sub, []netip.Addr{a("10.0.10.1"), a("10.0.10.230")}); got != "10.0.10.192/27" {
		t.Errorf("a range must skip the server's own address: %q", got)
	}
	if got := suggestRange(netip.MustParsePrefix("10.0.10.0/28"), nil); got != "" {
		t.Errorf("too small for a /27: %q", got)
	}
}

func TestFreeSubnet(t *testing.T) {
	used := map[string]netip.Prefix{"a": netip.MustParsePrefix("192.168.100.0/24"), "b": netip.MustParsePrefix("192.168.0.0/23")}
	if got := freeSubnet(used); got != "192.168.101.0/24" {
		t.Errorf("got %q", got)
	}
}
