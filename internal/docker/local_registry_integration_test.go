package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalRegistryIntegrationBuildPushResolvePull(t *testing.T) {
	if os.Getenv("SHIP_LOCAL_REGISTRY_INTEGRATION") != "1" {
		t.Skip("set SHIP_LOCAL_REGISTRY_INTEGRATION=1 to run the local Docker registry integration test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := Client{}
	if err := client.Available(ctx); err != nil {
		t.Skipf("docker daemon is unavailable: %v", err)
	}

	name := "ship-test-registry-" + sanitizeTagPart(strings.ReplaceAll(t.Name(), "/", "-"))
	_, _ = exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_, _ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", name).CombinedOutput()
	})
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "-p", "127.0.0.1::5000", "registry:2").CombinedOutput(); err != nil {
		t.Fatalf("start registry: %v\n%s", err, out)
	}
	portOut, err := exec.CommandContext(ctx, "docker", "port", name, "5000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	host := strings.TrimSpace(string(portOut))
	if host == "" {
		t.Fatal("registry port was not published")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := host + "/ship/integration:release-1"
	t.Setenv("DOCKER_AUTH_CONFIG", "{}")

	if err := client.BuildImage(ctx, BuildOptions{ContextDir: dir, Dockerfile: "Dockerfile", Tag: tag}); err != nil {
		t.Fatal(err)
	}
	if err := client.Push(ctx, tag); err != nil {
		t.Fatal(err)
	}
	digestRef, err := client.ResolveDigest(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digestRef, host+"/ship/integration@sha256:") {
		t.Fatalf("digest ref = %q", digestRef)
	}
	if err := client.Pull(ctx, digestRef); err != nil {
		t.Fatal(err)
	}
	rollbackRef, err := client.ResolveDigest(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackRef != digestRef {
		t.Fatalf("rollback digest = %q, want %q", rollbackRef, digestRef)
	}
}
