package ingress

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
)

func TestGeneratedCaddyfileValidatesWithCaddyBinary(t *testing.T) {
	caddyPath, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("caddy is not installed")
	}
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Health:  config.HealthCheck{HTTP: "/up"},
			Ingress: &config.Ingress{Domains: []string{"example.test"}},
		},
	}}
	file := GenerateCaddyfileFromReplicas(cfg, []Replica{
		{Service: "web", Host: "127.0.0.1", Port: 3000},
	})
	if strings.Contains(file, "handle /_ship/health { respond") {
		t.Fatalf("health handler must be multiline:\n%s", file)
	}
	for _, needle := range []string{
		"  handle /_ship/health {\n",
		"    respond \"ok\" 200\n",
		"  }\n",
	} {
		if !strings.Contains(file, needle) {
			t.Fatalf("missing multiline health block %q:\n%s", needle, file)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, caddyPath, "validate", "--config", path, "--adapter", "caddyfile").CombinedOutput()
	if err != nil {
		t.Fatalf("caddy validate failed: %v\n%s", err, out)
	}
}
