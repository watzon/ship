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
