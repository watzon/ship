package scheduler

import (
	"testing"

	"github.com/watzon/ship/internal/config"
)

func TestPlaceServicesRoundRobinByPool(t *testing.T) {
	cfg := &config.Config{
		Services: map[string]config.Service{
			"web": {Pool: "web", Scale: 5, Image: config.ImageSpec{Ref: "image"}},
		},
	}
	env := config.Environment{Hosts: config.HostsConfig{Pools: map[string]config.Pool{
		"web": {Hosts: []string{"web-b", "web-a"}},
	}}}
	placements, err := PlaceServices(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, placement := range placements {
		got = append(got, placement.Host.Name)
	}
	want := []string{"web-a", "web-b", "web-a", "web-b", "web-a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("placement[%d] = %q, want %q; all=%v", i, got[i], want[i], got)
		}
	}
}

func TestHostsForEnvironmentAppliesSSHConfigCascade(t *testing.T) {
	env := config.Environment{
		SSH: config.SSHConfig{
			Port:           22,
			IdentityFile:   "~/.ssh/root",
			KnownHostsFile: ".ship/known_hosts",
			JumpHost:       "bastion.example.com",
			Options:        map[string]string{"ControlMaster": "auto", "ControlPersist": "60s"},
		},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count: 1,
				SSH: config.SSHConfig{
					Port:         2222,
					IdentityFile: "~/.ssh/web",
					Options:      map[string]string{"ControlPersist": "5m", "ServerAliveInterval": "30"},
				},
			},
		}},
	}

	hosts := HostsForEnvironment(env)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	host := hosts[0]
	if host.SSHPort != 2222 || host.IdentityFile != "~/.ssh/web" || host.KnownHostsFile != ".ship/known_hosts" || host.JumpHost != "bastion.example.com" {
		t.Fatalf("host ssh config = %+v", host)
	}
	if host.SSHOptions["ControlMaster"] != "auto" || host.SSHOptions["ControlPersist"] != "5m" || host.SSHOptions["ServerAliveInterval"] != "30" {
		t.Fatalf("host ssh options = %+v", host.SSHOptions)
	}
}
