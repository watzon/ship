// Package hostfacts matches configured hosts against the host facts that
// `ship provision apply` saves in .ship/environments/<env>/hosts.json.
//
// Mismatch errors from this package must stay safe: they may point at the
// read-only `ship provision plan` and at pinning hosts by address, but never at
// a command that can create or replace servers.
package hostfacts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/ipaddr"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

// PinDocs points users at the documentation for hosts.pools.<pool>.pin.
const PinDocs = "docs/configuration/README.md#pinning-hosts-by-address"

// Match is one configured host and the host fact saved for it, if any.
type Match struct {
	Host    scheduler.Host
	Fact    state.HostFact
	HasFact bool
}

type key struct {
	name string
	pool string
}

// MatchHosts pairs every configured host with its saved host fact by logical
// name and pool. A pinned host needs no fact, but when one exists its address
// must agree with the pin. Unmatched configured hosts and unmatched facts are
// reported together in one error with a safe hint.
func MatchHosts(envName string, hosts []scheduler.Host, facts []state.HostFact) ([]Match, error) {
	factsByKey := map[key]state.HostFact{}
	for _, fact := range facts {
		k := key{name: strings.TrimSpace(fact.Name), pool: strings.TrimSpace(fact.Pool)}
		if k.name == "" || k.pool == "" {
			return nil, fmt.Errorf("host facts for %s contain a host without name or pool", envName)
		}
		if _, exists := factsByKey[k]; exists {
			return nil, fmt.Errorf("host facts for %s contain duplicate host %s in pool %s", envName, k.name, k.pool)
		}
		factsByKey[k] = fact
	}

	matches := make([]Match, 0, len(hosts))
	matched := map[key]struct{}{}
	var missingHosts []scheduler.Host
	var pinConflicts []string
	for _, host := range hosts {
		k := key{name: host.Name, pool: host.Pool}
		fact, ok := factsByKey[k]
		if ok {
			matched[k] = struct{}{}
			if pin := strings.TrimSpace(host.PinnedAddress); pin != "" {
				if recorded := Contact(fact); recorded != "" && !ipaddr.Equal(recorded, pin) {
					pinConflicts = append(pinConflicts, fmt.Sprintf("host %s pool=%s is pinned to %s but host facts record %s", host.Name, host.Pool, pin, recorded))
				}
			}
			matches = append(matches, Match{Host: host, Fact: fact, HasFact: true})
			continue
		}
		if strings.TrimSpace(host.PinnedAddress) != "" {
			matches = append(matches, Match{Host: host})
			continue
		}
		missingHosts = append(missingHosts, host)
	}

	var extraFacts []state.HostFact
	for k, fact := range factsByKey {
		if _, ok := matched[k]; !ok {
			extraFacts = append(extraFacts, fact)
		}
	}

	if len(pinConflicts) > 0 {
		sort.Strings(pinConflicts)
		return nil, fmt.Errorf("host facts for %s do not match pinned hosts: %s; if the server's address changed (for example after ship migrate), update hosts.pools.<pool>.pin in ship.yml to the address the provider reports. Ship never creates or replaces a pinned host", envName, strings.Join(pinConflicts, "; "))
	}
	if len(missingHosts) == 0 && len(extraFacts) == 0 {
		return matches, nil
	}
	return nil, mismatchError(envName, missingHosts, extraFacts)
}

func mismatchError(envName string, missingHosts []scheduler.Host, extraFacts []state.HostFact) error {
	var missing, extra, hints []string
	for _, host := range missingHosts {
		missing = append(missing, fmt.Sprintf("%s pool=%s", host.Name, host.Pool))
		for _, fact := range extraFacts {
			if strings.TrimSpace(fact.Pool) != host.Pool {
				continue
			}
			if address := Contact(fact); address != "" && ipaddr.Equal(address, host.Name) {
				hints = append(hints, fmt.Sprintf("%s is the address of provisioned host %s; to pin that server, keep host %s in pool %s (count or hosts) and set hosts.pools.%s.pin.%s: %s", address, fact.Name, fact.Name, host.Pool, host.Pool, fact.Name, address))
			}
		}
	}
	for _, fact := range extraFacts {
		extra = append(extra, fmt.Sprintf("%s pool=%s", strings.TrimSpace(fact.Name), strings.TrimSpace(fact.Pool)))
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(hints)
	parts := []string{}
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "extra "+strings.Join(extra, ", "))
	}
	hints = append(hints, fmt.Sprintf("run ship provision plan %s to see which hosts already exist and which servers apply would create; to use an existing server by address, pin it with hosts.pools.<pool>.pin (see %s)", envName, PinDocs))
	return fmt.Errorf("host facts for %s do not match configured hosts: %s; %s", envName, strings.Join(parts, "; "), strings.Join(hints, "; "))
}

// Contact returns the address recorded in a host fact.
func Contact(fact state.HostFact) string {
	if contact := strings.TrimSpace(fact.PublicAddress); contact != "" {
		return contact
	}
	return strings.TrimSpace(fact.IPv4)
}
