package netkey

import (
	"net"
	"testing"
)

// A NAT64 address stands for its IPv4 address, for keys and for Public.
func TestNAT64(t *testing.T) {
	if got := Of("64:ff9b::203.0.113.5"); got != "203.0.113.5" {
		t.Errorf("key %q", got)
	}
	if Public(net.ParseIP("64:ff9b::10.0.0.1")) {
		t.Error("NAT64 address of a private one is public")
	}
	if !Public(net.ParseIP("64:ff9b::8.8.8.8")) {
		t.Error("NAT64 address of a public one is not")
	}
	if got := Of("2001:4860::1"); got != "2001:4860::/64" {
		t.Errorf("IPv6 key %q", got)
	}
}
