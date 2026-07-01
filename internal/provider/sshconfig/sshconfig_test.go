package sshconfig

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Provider{}

func TestHostsParsesOpenSSHConfigForPoolAliases(t *testing.T) {
	files := map[string]string{
		"/home/me/.ssh/config": `
Host web-prod
  HostName 203.0.113.10
  User deploy
  Port 2222
  IdentityFile ~/.ssh/web
  UserKnownHostsFile ~/.ssh/known_hosts_ship
  ProxyJump bastion
  IdentitiesOnly yes

Host worker-*
  HostName %h.internal
  User worker

Include conf.d/*.conf
`,
		"/home/me/.ssh/conf.d/defaults.conf": `
Host *
  User root
  ForwardAgent no
  ServerAliveInterval 30
`,
	}
	env := testEnvironment()
	prov := testProvider(env, files)

	hosts, err := prov.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-prod" || hosts[0].PublicAddress != "203.0.113.10" || hosts[0].User != "deploy" || hosts[0].SSHPort != 2222 {
		t.Fatalf("host[0] = %+v", hosts[0])
	}
	if hosts[0].IdentityFile != "~/.ssh/web" || hosts[0].KnownHostsFile != "~/.ssh/known_hosts_ship" || hosts[0].JumpHost != "bastion" {
		t.Fatalf("host[0] ssh = %+v", hosts[0])
	}
	if hosts[0].SSHOptions["IdentitiesOnly"] != "yes" || hosts[0].SSHOptions["ForwardAgent"] != "no" || hosts[0].SSHOptions["ServerAliveInterval"] != "30" {
		t.Fatalf("host[0] options = %+v", hosts[0].SSHOptions)
	}
	if hosts[1].Name != "worker-a" || hosts[1].PublicAddress != "worker-a.internal" || hosts[1].User != "worker" {
		t.Fatalf("host[1] = %+v", hosts[1])
	}
}

func TestHostsRespectNegatedPatternsAndFirstValueWins(t *testing.T) {
	files := map[string]string{"/home/me/.ssh/config": `
Host web-* !web-skip
  User deploy
  HostName %h.example.com

Host web-prod
  User should-not-win

Host *
  User root
  HostName fallback.example.com
`}
	env := config.Environment{
		Provider: config.ProviderConfig{SSHConfig: &config.SSHConfigInventory{Path: "~/.ssh/config"}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Hosts: []string{"web-prod", "web-skip"}},
		}},
	}
	prov := testProvider(env, files)

	hosts, err := prov.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if hosts[0].Name != "web-prod" || hosts[0].User != "deploy" || hosts[0].PublicAddress != "web-prod.example.com" {
		t.Fatalf("host[0] = %+v", hosts[0])
	}
	if hosts[1].Name != "web-skip" || hosts[1].User != "root" || hosts[1].PublicAddress != "fallback.example.com" {
		t.Fatalf("host[1] = %+v", hosts[1])
	}
}

func TestReconcileTreatsSSHConfigHostsAsExisting(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, map[string]string{"/home/me/.ssh/config": `
Host web-prod
  HostName 203.0.113.10
  User deploy
`})

	result, err := prov.Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 2 || len(result.Existing) != 2 || len(result.Created) != 0 || len(result.Extra) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Desired[0].Location != "ssh_config" || result.Existing[0].PublicAddress != "203.0.113.10" {
		t.Fatalf("result = %+v", result)
	}
	if result.Desired[0].User != "deploy" {
		t.Fatalf("desired user = %q, want deploy", result.Desired[0].User)
	}
	if result.Existing[0].Labels[provider.LabelProject] != "demo" || result.Existing[0].Labels["tier"] != "edge" {
		t.Fatalf("labels = %+v", result.Existing[0].Labels)
	}
}

func TestCredentialChecksDoNotRequireCloudCredentials(t *testing.T) {
	checks := (Provider{}).CredentialChecks(func(string) (string, bool) {
		t.Fatal("ssh_config provider should not read environment credentials")
		return "", false
	})
	if len(checks) != 1 || checks[0].Required || !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testProvider(env config.Environment, files map[string]string) Provider {
	return Provider{
		Env: env,
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return []byte(data), nil
			}
			return nil, errors.New("unexpected file: " + path)
		},
		Glob: func(pattern string) ([]string, error) {
			var matches []string
			for path := range files {
				ok, err := filepath.Match(pattern, path)
				if err != nil {
					return nil, err
				}
				if ok {
					matches = append(matches, path)
				}
			}
			return matches, nil
		},
		HomeDir: func() (string, error) {
			return "/home/me", nil
		},
	}
}

func testEnvironment() config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{SSHConfig: &config.SSHConfigInventory{Path: "~/.ssh/config"}},
		Hosts: config.HostsConfig{
			Labels: map[string]string{"tier": "edge"},
			Pools: map[string]config.Pool{
				"web":    {Hosts: []string{"web-prod"}},
				"worker": {Hosts: []string{"worker-a"}},
			},
		},
	}
}

func TestTokensForLineRejectsUnterminatedQuotes(t *testing.T) {
	_, err := tokensForLine(`Host "web`)
	if err == nil || !strings.Contains(err.Error(), "unterminated quote") {
		t.Fatalf("expected unterminated quote error, got %v", err)
	}
}
