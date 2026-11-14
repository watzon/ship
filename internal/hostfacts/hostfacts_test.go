package hostfacts

import (
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func hostsFor(pool config.Pool) []scheduler.Host {
	return scheduler.HostsForEnvironment(config.Environment{Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": pool}}})
}

var web1Fact = state.HostFact{Name: "web-1", Pool: "web", User: "root", Provider: "vultr", ProviderID: "abc", IPv4: "203.0.113.10", PublicAddress: "203.0.113.10"}

func TestMatchHosts(t *testing.T) {
	tests := []struct {
		name        string
		pool        config.Pool
		facts       []state.HostFact
		wantFacts   map[string]bool
		wantErr     []string
		wantContact map[string]string
	}{
		{
			name:        "count host matched by name",
			pool:        config.Pool{Count: 1},
			facts:       []state.HostFact{web1Fact},
			wantFacts:   map[string]bool{"web-1": true},
			wantContact: map[string]string{"web-1": "203.0.113.10"},
		},
		{
			name:        "pinned host matches its fact",
			pool:        config.Pool{Count: 1, Pin: map[string]string{"web-1": "203.0.113.10"}},
			facts:       []state.HostFact{web1Fact},
			wantFacts:   map[string]bool{"web-1": true},
			wantContact: map[string]string{"web-1": "203.0.113.10"},
		},
		{
			name:        "pinned host needs no fact",
			pool:        config.Pool{Count: 2, Pin: map[string]string{"web-2": "198.51.100.7"}},
			facts:       []state.HostFact{web1Fact},
			wantFacts:   map[string]bool{"web-1": true, "web-2": false},
			wantContact: map[string]string{"web-2": "198.51.100.7"},
		},
		{
			name:    "pinned address disagrees with fact",
			pool:    config.Pool{Count: 1, Pin: map[string]string{"web-1": "198.51.100.7"}},
			facts:   []state.HostFact{web1Fact},
			wantErr: []string{"host web-1 pool=web is pinned to 198.51.100.7 but host facts record 203.0.113.10", "update hosts.pools.<pool>.pin"},
		},
		{
			// Issue #5: the user replaced count: 1 with hosts: [<ipv4 of web-1>].
			name:  "bare address of provisioned host",
			pool:  config.Pool{Hosts: []string{"203.0.113.10"}},
			facts: []state.HostFact{web1Fact},
			wantErr: []string{
				"missing 203.0.113.10 pool=web; extra web-1 pool=web",
				"203.0.113.10 is the address of provisioned host web-1",
				"set hosts.pools.web.pin.web-1: 203.0.113.10",
				"run ship provision plan production",
			},
		},
		{
			name:    "new host without fact",
			pool:    config.Pool{Count: 2},
			facts:   []state.HostFact{web1Fact},
			wantErr: []string{"missing web-2 pool=web", "run ship provision plan production", PinDocs},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches, err := MatchHosts("production", hostsFor(tt.pool), tt.facts)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected error, got matches %+v", matches)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error missing %q:\n%v", want, err)
					}
				}
				if strings.Contains(err.Error(), "provision apply") || strings.Contains(err.Error(), "--yes") {
					t.Fatalf("hint must not suggest provision apply: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != len(tt.wantFacts) {
				t.Fatalf("matches = %+v", matches)
			}
			for _, match := range matches {
				if want, ok := tt.wantFacts[match.Host.Name]; !ok || want != match.HasFact {
					t.Fatalf("match %s has fact %v, want %v", match.Host.Name, match.HasFact, want)
				}
				if want, ok := tt.wantContact[match.Host.Name]; ok {
					got := match.Host.ContactTarget()
					if match.HasFact {
						got = Contact(match.Fact)
					}
					if got != want {
						t.Fatalf("contact for %s = %q, want %q", match.Host.Name, got, want)
					}
				}
			}
		})
	}
}

func TestMatchHostsRejectsInvalidFacts(t *testing.T) {
	_, err := MatchHosts("production", hostsFor(config.Pool{Count: 1}), []state.HostFact{web1Fact, web1Fact})
	if err == nil || !strings.Contains(err.Error(), "duplicate host web-1 in pool web") {
		t.Fatalf("err = %v", err)
	}
	_, err = MatchHosts("production", hostsFor(config.Pool{Count: 1}), []state.HostFact{{Name: "web-1"}})
	if err == nil || !strings.Contains(err.Error(), "without name or pool") {
		t.Fatalf("err = %v", err)
	}
}

func TestMatchHostsComparesPinsAsIPAddresses(t *testing.T) {
	tests := []struct{ pin, recorded string }{
		{"2001:DB8::1", "2001:db8::0:1"},
		{"203.0.113.10", "::ffff:203.0.113.10"},
		{"::ffff:203.0.113.10", "203.0.113.10"},
	}
	for _, tt := range tests {
		fact := state.HostFact{Name: "web-1", Pool: "web", User: "root", PublicAddress: tt.recorded}
		if _, err := MatchHosts("production", hostsFor(config.Pool{Count: 1, Pin: map[string]string{"web-1": tt.pin}}), []state.HostFact{fact}); err != nil {
			t.Errorf("pin %s vs fact %s: %v", tt.pin, tt.recorded, err)
		}
	}
	fact := state.HostFact{Name: "web-1", Pool: "web", User: "root", PublicAddress: "fe80::1%eth1"}
	if _, err := MatchHosts("production", hostsFor(config.Pool{Count: 1, Pin: map[string]string{"web-1": "fe80::1%eth0"}}), []state.HostFact{fact}); err == nil {
		t.Error("zones must match exactly")
	}
}
