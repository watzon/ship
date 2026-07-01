package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunAddsShipManagedLabel(t *testing.T) {
	var gotName string
	var gotArgs []string
	client := Client{CommandRunner: func(ctx context.Context, name string, args ...string) (string, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return "container-id\n", nil
	}}
	if err := client.Run(context.Background(), "ship_web_1", "example/web:1", "", "-p", "3000:3000"); err != nil {
		t.Fatal(err)
	}
	if gotName != "docker" {
		t.Fatalf("command = %q", gotName)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--label managed-by=ship") {
		t.Fatalf("docker run args missing managed label: %#v", gotArgs)
	}
}

func TestListShipContainersParsesDockerPSJSONLines(t *testing.T) {
	client := Client{CommandRunner: func(ctx context.Context, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "label=managed-by=ship") {
			t.Fatalf("docker ps args missing label filter: %#v", args)
		}
		return `{"ID":"abc","Image":"example/web:1","Names":"ship_web_1","Status":"Up 1 second","Labels":"managed-by=ship,project=demo"}` + "\n", nil
	}}
	containers, err := client.ListShipContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers = %+v", containers)
	}
	got := containers[0]
	if got.ID != "abc" || got.Names != "ship_web_1" || got.Labels["project"] != "demo" {
		t.Fatalf("container = %+v", got)
	}
}

func TestReleaseTagUsesGitSHAAndTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	got := ReleaseTag(context.Background(), now, func(context.Context) (string, error) {
		return "abc123def456\n", nil
	})
	want := "abc123def456-20260630T183456.123456789Z"
	if got != want {
		t.Fatalf("release tag = %q, want %q", got, want)
	}
}

func TestReleaseTagFallsBackToTimestampWhenGitUnavailable(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 34, 56, 987654321, time.UTC)
	got := ReleaseTag(context.Background(), now, func(context.Context) (string, error) {
		return "", errors.New("no git")
	})
	want := "20260630T123456.987654321Z"
	if got != want {
		t.Fatalf("release tag = %q, want %q", got, want)
	}
}

func TestImageTagUsesServiceAndRelease(t *testing.T) {
	got, err := ImageTag("ghcr.io/acme/demo", "web", "abc123-20260630T123456.000000000Z")
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/acme/demo:web-abc123-20260630T123456.000000000Z"
	if got != want {
		t.Fatalf("image tag = %q, want %q", got, want)
	}
}

func TestBuildCommandIncludesArgsTargetAndPlatform(t *testing.T) {
	got, err := BuildCommand(BuildOptions{
		ContextDir: "services/web",
		Dockerfile: "Dockerfile.prod",
		Tag:        "ghcr.io/acme/demo:web-release",
		BuildArgs: map[string]string{
			"RAILS_ENV": "production",
			"VERSION":   "release",
		},
		Target:   "runtime",
		Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"build",
		"-f", filepath.Join("services", "web", "Dockerfile.prod"),
		"-t", "ghcr.io/acme/demo:web-release",
		"--label", "managed-by=ship",
		"--platform", "linux/amd64",
		"--target", "runtime",
		"--build-arg", "RAILS_ENV=production",
		"--build-arg", "VERSION=release",
		"services/web",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("build args = %#v, want %#v", got, want)
	}
}

func TestBuildCommandPreservesAbsoluteDockerfile(t *testing.T) {
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	got, err := BuildCommand(BuildOptions{
		ContextDir: "services/web",
		Dockerfile: dockerfile,
		Tag:        "ghcr.io/acme/demo:web-release",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"build",
		"-f", dockerfile,
		"-t", "ghcr.io/acme/demo:web-release",
		"--label", "managed-by=ship",
		"services/web",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("build args = %#v, want %#v", got, want)
	}
}

func TestRunFailureRedactsSensitiveDockerArgs(t *testing.T) {
	secret := "literal-secret-value"
	err := Client{}.run(
		context.Background(),
		"false",
		"build",
		"--build-arg", "TOKEN="+secret,
		"--env=PASSWORD="+secret,
		"--env-file", "/tmp/"+secret,
		"--secret", "id=key,src=/tmp/"+secret,
	)
	if err == nil {
		t.Fatal("expected command failure")
	}
	message := err.Error()
	if strings.Contains(message, secret) {
		t.Fatalf("docker command failure leaked secret: %v", err)
	}
	for _, needle := range []string{
		"--build-arg TOKEN=<redacted>",
		"--env=PASSWORD=<redacted>",
		"--env-file <redacted>",
		"--secret <redacted>",
	} {
		if !strings.Contains(message, needle) {
			t.Fatalf("redacted error %q missing %q", message, needle)
		}
	}
}

func TestBuildImageStreamsLogsWhenWriterConfigured(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"), `#!/bin/sh
echo build stdout
echo build stderr >&2
exit 0
`)
	t.Setenv("PATH", dir)
	var logs strings.Builder
	err := Client{LogWriter: &logs}.BuildImage(context.Background(), BuildOptions{
		ContextDir: ".",
		Dockerfile: "Dockerfile",
		Tag:        "example/web:release",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	if !strings.Contains(text, "build stdout") || !strings.Contains(text, "build stderr") {
		t.Fatalf("streamed logs = %q", text)
	}
}

func TestPruneShipImagesUsesManagedLabelFilter(t *testing.T) {
	var got []string
	client := Client{CommandRunner: func(ctx context.Context, name string, args ...string) (string, error) {
		got = append([]string{name}, args...)
		return "", nil
	}}
	if err := client.PruneShipImages(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "image", "prune", "-f", "--filter", "label=managed-by=ship"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("prune args = %#v, want %#v", got, want)
	}
}

func TestResolveDigestReturnsImmutableReferenceForTag(t *testing.T) {
	t.Setenv("DOCKER_AUTH_CONFIG", "{}")
	digest := "sha256:" + strings.Repeat("a", 64)
	host := newDigestRegistry(t, digest)

	got, err := Client{}.ResolveDigest(context.Background(), host+"/acme/web:release")
	if err != nil {
		t.Fatal(err)
	}
	want := host + "/acme/web@" + digest
	if got != want {
		t.Fatalf("digest ref = %q, want %q", got, want)
	}
}

func TestResolveDigestKeepsExistingDigestReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	got, err := Client{HTTPClient: &http.Client{Transport: failingRoundTripper{t: t}}}.ResolveDigest(context.Background(), "ghcr.io/acme/web@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/acme/web@" + digest
	if got != want {
		t.Fatalf("digest ref = %q, want %q", got, want)
	}
}

func TestRegistryLoggedInUsesDockerAuthConfigForRegistryHost(t *testing.T) {
	host := newBearerRegistry(t, "u", "s")
	auth := base64.StdEncoding.EncodeToString([]byte("u:s"))
	t.Setenv("DOCKER_AUTH_CONFIG", fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, auth))

	err := Client{}.RegistryLoggedIn(context.Background(), host+"/acme/example")
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistryLoggedInRejectsInvalidAuthConfig(t *testing.T) {
	host := newBearerRegistry(t, "u", "s")
	auth := base64.StdEncoding.EncodeToString([]byte("u:expired-secret"))
	t.Setenv("DOCKER_AUTH_CONFIG", fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, auth))

	err := Client{}.RegistryLoggedIn(context.Background(), host+"/acme/example")
	if err == nil {
		t.Fatal("expected registry rejection")
	}
	if strings.Contains(err.Error(), "expired-secret") {
		t.Fatalf("registry error leaked secret: %v", err)
	}
}

func TestRegistryLoggedInDoesNotAssumeLatestTag(t *testing.T) {
	restoreEnv(t, "DOCKER_AUTH_CONFIG")
	if err := os.Setenv("DOCKER_AUTH_CONFIG", `{}`); err != nil {
		t.Fatal(err)
	}

	err := Client{}.RegistryLoggedIn(context.Background(), "ghcr.io/acme/example")
	if err == nil {
		t.Fatal("expected missing credentials error")
	}
	if strings.Contains(err.Error(), ":latest") {
		t.Fatalf("registry check assumed latest tag: %v", err)
	}
}

func TestRegistryLoggedInUsesCredentialHelper(t *testing.T) {
	host := newBearerRegistry(t, "u", "s")
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker-credential-shiptest"), `#!/bin/sh
if [ "$1" = "get" ]; then
  cat >/dev/null
  echo '{"Username":"u","Secret":"s"}'
  exit 0
fi
exit 2
`)
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_AUTH_CONFIG", fmt.Sprintf(`{"credHelpers":{%q:"shiptest"}}`, host))

	err := Client{}.RegistryLoggedIn(context.Background(), host+"/acme/example")
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCredentialsAllowAnonymousTTLWhenHelperHasNoEntry(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker-credential-osxkeychain"), `#!/bin/sh
exit 1
`)
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_AUTH_CONFIG", `{"credsStore":"osxkeychain"}`)

	credentials, err := Client{}.registryCredentials(context.Background(), "ttl.sh")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.username != "" || credentials.password != "" || credentials.identityToken != "" {
		t.Fatalf("credentials = %+v, want anonymous", credentials)
	}
}

func newBearerRegistry(t *testing.T, username, password string) string {
	t.Helper()

	const token = "issued-token"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if r.Header.Get("Authorization") == "Bearer "+token {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ship-test"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			gotUsername, gotPassword, ok := r.BasicAuth()
			if !ok || gotUsername != username || gotPassword != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"token":"`+token+`"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func newDigestRegistry(t *testing.T, digest string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/v2/acme/web/manifests/release" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

type failingRoundTripper struct {
	t *testing.T
}

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Fatal("unexpected registry request")
	return nil, errors.New("unexpected request")
}

func restoreEnv(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
