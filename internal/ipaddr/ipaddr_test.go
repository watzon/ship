package ipaddr

import "testing"

func TestEqual(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"203.0.113.10", "203.0.113.10", true},
		{" 203.0.113.10 ", "203.0.113.10", true},
		{"::ffff:203.0.113.10", "203.0.113.10", true},
		{"2001:DB8::1", "2001:db8::0:1", true},
		{"2001:db8::1", "2001:db8::2", false},
		{"fe80::1%eth0", "fe80::1%eth0", true},
		{"fe80::1%eth0", "fe80::1", false},
		{"fe80::1%eth0", "fe80::1%eth1", false},
		{"::ffff:203.0.113.10%eth0", "203.0.113.10", false},
		{"web-1", "web-1", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := Equal(tt.a, tt.b); got != tt.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCanonical(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"2001:DB8:0::1", "2001:db8::1", true},
		{"::ffff:203.0.113.10", "203.0.113.10", true},
		{"203.0.113.10", "203.0.113.10", true},
		{"fe80::1%eth0", "fe80::1%eth0", true},
		{"web-1.example.com", "", false},
	}
	for _, tt := range tests {
		got, ok := Canonical(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Canonical(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
