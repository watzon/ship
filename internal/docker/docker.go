package docker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Client struct {
	DryRun        bool
	HTTPClient    *http.Client
	CommandRunner CommandRunner
	LogWriter     io.Writer
}

type CommandRunner func(ctx context.Context, name string, args ...string) (string, error)

const (
	LabelManagedBy      = "managed-by"
	LabelManagedByValue = "ship"
	LabelProject        = "project"
	LabelEnvironment    = "environment"
	LabelService        = "service"
	LabelAccessory      = "accessory"
	LabelReplica        = "replica"
	LabelRelease        = "release"
)

type ContainerSummary struct {
	ID     string            `json:"id"`
	Image  string            `json:"image"`
	Names  string            `json:"names"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels,omitempty"`
}

type BuildOptions struct {
	ContextDir     string
	Dockerfile     string
	Tag            string
	AdditionalTags []string
	BuildArgs      map[string]string
	Target         string
	Builder        string
	Buildpack      BuildpackOptions
	Platform       string
	Platforms      []string
	Pull           bool
	NoCache        bool
	NoCacheFilter  []string
	CacheFrom      []string
	CacheTo        []string
	Secrets        []string
	SSH            []string
	SBOM           string
	Provenance     string
	Push           bool
}

type BuildpackOptions struct {
	Builder      string
	Buildpacks   []string
	Env          map[string]string
	Descriptor   string
	Publish      bool
	PullPolicy   string
	TrustBuilder bool
}

func (b BuildpackOptions) Enabled() bool {
	return strings.TrimSpace(b.Builder) != "" ||
		len(b.Buildpacks) > 0 ||
		len(b.Env) > 0 ||
		strings.TrimSpace(b.Descriptor) != "" ||
		b.Publish ||
		strings.TrimSpace(b.PullPolicy) != "" ||
		b.TrustBuilder
}

type RegistryAuth struct {
	Server string          `json:"server"`
	Auth   json.RawMessage `json:"auth"`
}

func (c Client) Available(ctx context.Context) error {
	return c.run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
}

func (c Client) RegistryLoggedIn(ctx context.Context, registry string) error {
	host, candidates, err := registryAuthCandidates(registry)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if credentials, ok := authCredentials(cfg.Auths, candidates); ok {
		if err := c.validateRegistryCredentials(ctx, host, credentials); err != nil {
			return fmt.Errorf("docker credentials for %s rejected by registry: %w", host, err)
		}
		return nil
	}
	if helper := credentialHelper(cfg, candidates); helper != "" {
		credentials, err := c.helperCredentials(ctx, helper, candidates)
		if err != nil {
			return fmt.Errorf("docker credentials for %s unavailable through docker-credential-%s: %w", host, helper, err)
		}
		if err := c.validateRegistryCredentials(ctx, host, credentials); err != nil {
			return fmt.Errorf("docker credentials for %s rejected by registry: %w", host, err)
		}
		return nil
	}
	return fmt.Errorf("no docker credentials configured for %s", host)
}

func (c Client) RegistryAuth(ctx context.Context, registry string) (RegistryAuth, bool, error) {
	host, err := registryAuthHost(registry)
	if err != nil {
		return RegistryAuth{}, false, err
	}
	if isDockerHubOfficialImage(registry, host) {
		return RegistryAuth{}, false, nil
	}
	credentials, err := c.registryCredentials(ctx, host)
	if err != nil {
		return RegistryAuth{}, false, err
	}
	auth, ok, err := registryAuthEntry(credentials)
	if err != nil || !ok {
		return RegistryAuth{}, ok, err
	}
	return RegistryAuth{Server: host, Auth: auth}, true, nil
}

func isDockerHubOfficialImage(value, host string) bool {
	if host != "docker.io" && host != "index.docker.io" {
		return false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	named, _, _ := strings.Cut(value, "@")
	if tagIndex := strings.LastIndex(named, ":"); tagIndex > strings.LastIndex(named, "/") {
		named = named[:tagIndex]
	}
	if !strings.Contains(named, "/") {
		return true
	}
	return strings.HasPrefix(named, "library/")
}

func registryAuthHost(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("registry is required")
	}
	if !strings.Contains(value, "/") {
		name := value
		if base, _, ok := strings.Cut(value, ":"); ok {
			name = base
		}
		if name == "localhost" || strings.Contains(name, ".") {
			host, _, err := registryAuthCandidates(value)
			return host, err
		}
	}
	ref, err := parseImageReference(value)
	if err == nil {
		return ref.authHost(), nil
	}
	host, _, err := registryAuthCandidates(value)
	return host, err
}

func (c Client) BuildKitSupported(ctx context.Context) error {
	return c.run(ctx, "docker", "buildx", "version")
}

func (c Client) Build(ctx context.Context, contextDir, dockerfile, tag string) error {
	return c.BuildImage(ctx, BuildOptions{ContextDir: contextDir, Dockerfile: dockerfile, Tag: tag})
}

func (c Client) BuildImage(ctx context.Context, opts BuildOptions) error {
	name, args, err := BuildInvocation(opts)
	if err != nil {
		return err
	}
	if c.LogWriter != nil && c.CommandRunner == nil && !c.DryRun {
		return c.stream(ctx, name, args...)
	}
	return c.run(ctx, name, args...)
}

func BuildInvocation(opts BuildOptions) (string, []string, error) {
	if opts.Buildpack.Enabled() {
		args, err := PackBuildCommand(opts)
		return "pack", args, err
	}
	args, err := BuildCommand(opts)
	return "docker", args, err
}

func PackBuildCommand(opts BuildOptions) ([]string, error) {
	contextDir := strings.TrimSpace(opts.ContextDir)
	if contextDir == "" {
		contextDir = "."
	}
	tag := strings.TrimSpace(opts.Tag)
	if tag == "" {
		return nil, fmt.Errorf("image tag is required")
	}
	args := []string{"build", tag, "--path", contextDir}
	for _, additionalTag := range additionalImageTags(tag, opts.AdditionalTags) {
		args = append(args, "--tag", additionalTag)
	}
	if builder := strings.TrimSpace(opts.Buildpack.Builder); builder != "" {
		args = append(args, "--builder", builder)
	}
	for _, buildpack := range sortedNonEmpty(opts.Buildpack.Buildpacks) {
		args = append(args, "--buildpack", buildpack)
	}
	envNames := make([]string, 0, len(opts.Buildpack.Env))
	for name := range opts.Buildpack.Env {
		name = strings.TrimSpace(name)
		if name != "" {
			envNames = append(envNames, name)
		}
	}
	sort.Strings(envNames)
	for _, name := range envNames {
		args = append(args, "--env", name+"="+opts.Buildpack.Env[name])
	}
	if descriptor := strings.TrimSpace(opts.Buildpack.Descriptor); descriptor != "" {
		args = append(args, "--descriptor", descriptor)
	}
	if pullPolicy := strings.TrimSpace(opts.Buildpack.PullPolicy); pullPolicy != "" {
		args = append(args, "--pull-policy", pullPolicy)
	}
	if opts.Buildpack.TrustBuilder {
		args = append(args, "--trust-builder")
	}
	if BuildPublishesImage(opts) {
		args = append(args, "--publish")
	}
	return args, nil
}

func BuildCommand(opts BuildOptions) ([]string, error) {
	contextDir := strings.TrimSpace(opts.ContextDir)
	if contextDir == "" {
		contextDir = "."
	}
	dockerfile := strings.TrimSpace(opts.Dockerfile)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(contextDir, dockerfile)
	}
	tag := strings.TrimSpace(opts.Tag)
	if tag == "" {
		return nil, fmt.Errorf("image tag is required")
	}
	args := []string{"build"}
	if buildxRequired(opts) {
		args = []string{"buildx", "build"}
		if BuildPublishesImage(opts) {
			args = append(args, "--push")
		} else {
			args = append(args, "--load")
		}
		if builder := strings.TrimSpace(opts.Builder); builder != "" {
			args = append(args, "--builder", builder)
		}
	}
	args = append(args, "-f", dockerfile, "-t", tag)
	for _, additionalTag := range additionalImageTags(tag, opts.AdditionalTags) {
		args = append(args, "-t", additionalTag)
	}
	args = append(args, "--label", LabelManagedBy+"="+LabelManagedByValue)
	if opts.Pull {
		args = append(args, "--pull")
	}
	if opts.NoCache {
		args = append(args, "--no-cache")
	}
	if filters := nonEmptyOrdered(opts.NoCacheFilter); len(filters) > 0 {
		args = append(args, "--no-cache-filter", strings.Join(filters, ","))
	}
	if platform := buildPlatforms(opts); platform != "" {
		args = append(args, "--platform", platform)
	}
	if target := strings.TrimSpace(opts.Target); target != "" {
		args = append(args, "--target", target)
	}
	buildArgNames := make([]string, 0, len(opts.BuildArgs))
	for name := range opts.BuildArgs {
		buildArgNames = append(buildArgNames, name)
	}
	sort.Strings(buildArgNames)
	for _, name := range buildArgNames {
		args = append(args, "--build-arg", name+"="+opts.BuildArgs[name])
	}
	for _, spec := range sortedNonEmpty(opts.CacheFrom) {
		args = append(args, "--cache-from", spec)
	}
	for _, spec := range sortedNonEmpty(opts.CacheTo) {
		args = append(args, "--cache-to", spec)
	}
	for _, spec := range sortedNonEmpty(opts.Secrets) {
		args = append(args, "--secret", spec)
	}
	for _, spec := range sortedNonEmpty(opts.SSH) {
		args = append(args, "--ssh", spec)
	}
	if sbom := strings.TrimSpace(opts.SBOM); sbom != "" && sbom != "false" {
		args = append(args, "--sbom", sbom)
	}
	if provenance := strings.TrimSpace(opts.Provenance); provenance != "" && provenance != "false" {
		args = append(args, "--provenance", provenance)
	}
	args = append(args, contextDir)
	return args, nil
}

func buildxRequired(opts BuildOptions) bool {
	return len(opts.CacheFrom) > 0 ||
		len(opts.CacheTo) > 0 ||
		len(opts.Secrets) > 0 ||
		len(opts.SSH) > 0 ||
		strings.TrimSpace(opts.Builder) != "" ||
		len(opts.NoCacheFilter) > 0 ||
		multiPlatformRequested(opts) ||
		BuildPublishesImage(opts)
}

func BuildPublishesImage(opts BuildOptions) bool {
	return opts.Push ||
		(opts.Buildpack.Enabled() && opts.Buildpack.Publish) ||
		multiPlatformRequested(opts) ||
		buildFlagEnabled(opts.SBOM) ||
		buildFlagEnabled(opts.Provenance)
}

func multiPlatformRequested(opts BuildOptions) bool {
	return len(nonEmptyOrdered(opts.Platforms)) > 0 || strings.Contains(strings.TrimSpace(opts.Platform), ",")
}

func buildPlatforms(opts BuildOptions) string {
	platforms := nonEmptyOrdered(opts.Platforms)
	if len(platforms) > 0 {
		return strings.Join(platforms, ",")
	}
	return strings.TrimSpace(opts.Platform)
}

func buildFlagEnabled(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "false"
}

func nonEmptyOrdered(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func sortedNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func additionalImageTags(primary string, values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{strings.TrimSpace(primary): true}
	for _, value := range sortedNonEmpty(values) {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (c Client) Push(ctx context.Context, tag string) error {
	return c.run(ctx, "docker", "push", tag)
}

func (c Client) ResolveDigest(ctx context.Context, image string) (string, error) {
	ref, err := parseImageReference(image)
	if err != nil {
		return "", err
	}
	if ref.digest != "" {
		return ref.digestRef(ref.digest), nil
	}
	credentials, err := c.registryCredentials(ctx, ref.authHost())
	if err != nil {
		return "", err
	}
	digest, err := c.resolveManifestDigest(ctx, ref, credentials)
	if err != nil {
		return "", err
	}
	return ref.digestRef(digest), nil
}

func (c Client) Pull(ctx context.Context, image string) error {
	return c.run(ctx, "docker", "pull", image)
}

func (c Client) PruneShipImages(ctx context.Context) error {
	return c.run(ctx, "docker", "image", "prune", "-f", "--filter", "label="+LabelManagedBy+"="+LabelManagedByValue)
}

func (c Client) Run(ctx context.Context, name, image, command string, args ...string) error {
	base := []string{
		"run",
		"-d",
		"--restart",
		"unless-stopped",
		"--label",
		LabelManagedBy + "=" + LabelManagedByValue,
		"--name",
		name,
	}
	base = append(base, args...)
	base = append(base, image)
	if command != "" {
		tokens, err := SplitCommand(command)
		if err != nil {
			return fmt.Errorf("command for %q: %w", name, err)
		}
		base = append(base, tokens...)
	}
	return c.run(ctx, "docker", base...)
}

// SplitCommand splits a command string into exec-form argv the way a shell
// word-splits arguments, without shell semantics: no globbing, pipes,
// redirects, `&&`, or variable expansion. This matches how docker-compose
// interprets a string `command:`.
//
// It deliberately does not wrap the result in `sh -c`. Passing it straight
// through as the container's CMD preserves each image's own ENTRYPOINT
// contract and PATH: official images like postgres and mysql ship an
// entrypoint script that branches on argv[0] (e.g. only drops root via gosu
// when $1 == "postgres") and set PATH via ENV. Wrapping everything in
// `sh -lc "<command>"` made argv[0] always "sh" (skipping that branch) and
// `-l` re-sourced /etc/profile, clobbering the image's PATH — together
// producing "postgres: not found" even though the binary was on PATH via
// ENV. A command that genuinely needs shell features can still get them
// explicitly: `command: sh -c "foo && bar"`.
func SplitCommand(command string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	hasToken := false
	var quote rune
	runes := []rune(command)
	flush := func() {
		if hasToken {
			tokens = append(tokens, current.String())
			current.Reset()
			hasToken = false
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(runes) && strings.ContainsRune(`"\$`+"`", runes[i+1]):
				i++
				current.WriteRune(runes[i])
			default:
				current.WriteRune(r)
			}
		case r == '\'', r == '"':
			quote = r
			hasToken = true
		case r == '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("trailing backslash in command")
			}
			i++
			current.WriteRune(runes[i])
			hasToken = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
			hasToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in command", quote)
	}
	flush()
	return tokens, nil
}

// releaseIDBytes is the amount of randomness backing a release ID: 6 bytes
// hex-encode to a 12 character id, short enough to keep container and image
// tag names readable while leaving collisions astronomically unlikely.
const releaseIDBytes = 6

// NewReleaseID returns a short random release identifier. Release ids are a
// global key in Ship's state store (not scoped per environment), so they
// need to be collision-resistant on their own; git revision and creation
// time are tracked separately on the release record instead of being baked
// into the id.
func NewReleaseID() (string, error) {
	buf := make([]byte, releaseIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate release id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func GitShortSHA(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--short=12", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ImageTag(repository, serviceName, releaseTag string) (string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return "", fmt.Errorf("registry repository is required")
	}
	servicePart := sanitizeTagPart(serviceName)
	if servicePart == "" {
		return "", fmt.Errorf("service name is required")
	}
	releasePart := sanitizeTagPart(releaseTag)
	if releasePart == "" {
		return "", fmt.Errorf("release tag is required")
	}
	tag := servicePart + "-" + releasePart
	if len(tag) > 128 {
		return "", fmt.Errorf("image tag for service %q is too long", serviceName)
	}
	return repository + ":" + tag, nil
}

func ImageAliasTags(repository, serviceName string, aliases []string) ([]string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return nil, fmt.Errorf("registry repository is required")
	}
	servicePart := sanitizeTagPart(serviceName)
	if servicePart == "" {
		return nil, fmt.Errorf("service name is required")
	}
	var out []string
	seen := map[string]bool{}
	for _, alias := range aliases {
		aliasPart := sanitizeTagPart(alias)
		if aliasPart == "" {
			return nil, fmt.Errorf("image alias tag for service %q is required", serviceName)
		}
		tag := servicePart + "-" + aliasPart
		if len(tag) > 128 {
			return nil, fmt.Errorf("image alias tag for service %q is too long", serviceName)
		}
		ref := repository + ":" + tag
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, nil
}

func sanitizeTagPart(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return ""
	}
	first := out[0]
	if first == '.' || first == '-' {
		out = "x" + out
	}
	return out
}

func (c Client) StopRemove(ctx context.Context, name string) error {
	if err := c.Stop(ctx, name); err != nil {
		return err
	}
	return c.Remove(ctx, name)
}

// Stop stops a container while preserving it for a later restart.
func (c Client) Stop(ctx context.Context, name string) error {
	if err := c.run(ctx, "docker", "stop", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

// Start starts an existing stopped container with its original configuration.
func (c Client) Start(ctx context.Context, name string) error {
	return c.run(ctx, "docker", "start", name)
}

// Remove removes a stopped container.
func (c Client) Remove(ctx context.Context, name string) error {
	if err := c.run(ctx, "docker", "rm", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

func (c Client) Logs(ctx context.Context, name string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}
	return c.output(ctx, "docker", "logs", "--tail", fmt.Sprint(lines), name)
}

func (c Client) InspectRunning(ctx context.Context, name string) (bool, error) {
	out, err := c.output(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func (c Client) Inspect(ctx context.Context, name string) (json.RawMessage, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("container name is required")
	}
	out, err := c.output(ctx, "docker", "inspect", name)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(strings.TrimSpace(out))
	if !json.Valid(raw) {
		return nil, fmt.Errorf("docker inspect returned invalid JSON for %q", name)
	}
	return raw, nil
}

func (c Client) ListShipContainers(ctx context.Context) ([]ContainerSummary, error) {
	out, err := c.output(ctx, "docker", "ps", "-a", "--filter", "label="+LabelManagedBy+"="+LabelManagedByValue, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var containers []ContainerSummary
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row struct {
			ID     string `json:"ID"`
			Image  string `json:"Image"`
			Names  string `json:"Names"`
			Status string `json:"Status"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse docker ps row: %w", err)
		}
		containers = append(containers, ContainerSummary{
			ID:     row.ID,
			Image:  row.Image,
			Names:  row.Names,
			Status: row.Status,
			Labels: parseDockerLabels(row.Labels),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return containers, nil
}

func (c Client) run(ctx context.Context, name string, args ...string) error {
	_, err := c.output(ctx, name, args...)
	return err
}

func (c Client) output(ctx context.Context, name string, args ...string) (string, error) {
	if c.DryRun {
		return fmt.Sprintf("%s %s", name, strings.Join(args, " ")), nil
	}
	if c.CommandRunner != nil {
		return c.CommandRunner(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		redactedArgs, sensitiveValues := redactDockerArgs(args)
		msg = redactSensitiveText(msg, sensitiveValues)
		return "", fmt.Errorf("%s %s failed: %s", name, strings.Join(redactedArgs, " "), msg)
	}
	return string(out), nil
}

func (c Client) stream(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = c.LogWriter
	cmd.Stderr = c.LogWriter
	if err := cmd.Run(); err != nil {
		redactedArgs, _ := redactDockerArgs(args)
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(redactedArgs, " "), err)
	}
	return nil
}

func redactDockerArgs(args []string) ([]string, []string) {
	redacted := make([]string, 0, len(args))
	var sensitiveValues []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if isSensitiveDockerFlag(arg) && i+1 < len(args) {
			value := args[i+1]
			redacted = append(redacted, arg, redactDockerFlagValue(arg, value))
			sensitiveValues = append(sensitiveValues, sensitiveFragments(value)...)
			i++
			continue
		}
		if flag, value, ok := strings.Cut(arg, "="); ok && isSensitiveDockerFlag(flag) {
			redacted = append(redacted, flag+"="+redactDockerFlagValue(flag, value))
			sensitiveValues = append(sensitiveValues, sensitiveFragments(value)...)
			continue
		}
		redacted = append(redacted, arg)
	}
	return redacted, sensitiveValues
}

func isSensitiveDockerFlag(flag string) bool {
	switch flag {
	case "-e", "--env", "--env-file", "--build-arg", "--secret":
		return true
	default:
		return false
	}
}

func redactDockerFlagValue(flag, value string) string {
	switch flag {
	case "-e", "--env", "--build-arg":
		if name, _, ok := strings.Cut(value, "="); ok && strings.TrimSpace(name) != "" {
			return name + "=<redacted>"
		}
	}
	return "<redacted>"
}

func sensitiveFragments(value string) []string {
	fragments := []string{value}
	if _, secret, ok := strings.Cut(value, "="); ok {
		fragments = append(fragments, secret)
	}
	for _, part := range strings.Split(value, ",") {
		if _, secret, ok := strings.Cut(part, "="); ok {
			fragments = append(fragments, secret)
		}
	}
	return fragments
}

func redactSensitiveText(text string, values []string) string {
	seen := map[string]struct{}{}
	var unique []string
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Slice(unique, func(i, j int) bool {
		return len(unique[i]) > len(unique[j])
	})
	for _, value := range unique {
		text = strings.ReplaceAll(text, value, "<redacted>")
	}
	return text
}

func isNoSuchContainer(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") || strings.Contains(message, "no such object")
}

func parseDockerLabels(value string) map[string]string {
	labels := map[string]string{}
	for _, pair := range strings.Split(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			labels[pair] = ""
			continue
		}
		labels[key] = val
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

type configFile struct {
	Auths       map[string]json.RawMessage `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

func loadConfig() (configFile, error) {
	if raw := strings.TrimSpace(os.Getenv("DOCKER_AUTH_CONFIG")); raw != "" {
		var cfg configFile
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return configFile{}, fmt.Errorf("parse DOCKER_AUTH_CONFIG: %w", err)
		}
		return cfg, nil
	}
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return configFile{}, err
		}
		dir = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return configFile{}, fmt.Errorf("read docker config: %w", err)
	}
	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return configFile{}, fmt.Errorf("parse docker config: %w", err)
	}
	return cfg, nil
}

func registryAuthCandidates(registry string) (string, []string, error) {
	registry = strings.TrimSpace(registry)
	if registry == "" {
		return "", nil, fmt.Errorf("registry is required")
	}
	host := strings.Split(registry, "/")[0]
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		host = "index.docker.io"
	}
	candidates := []string{host, "https://" + host}
	if host == "docker.io" || host == "index.docker.io" {
		candidates = append(candidates, "docker.io", "https://index.docker.io/v1/")
	}
	return host, candidates, nil
}

type registryCredentials struct {
	username      string
	password      string
	identityToken string
}

func registryAuthEntry(credentials registryCredentials) (json.RawMessage, bool, error) {
	if credentials.identityToken != "" {
		raw, err := json.Marshal(map[string]string{"identitytoken": credentials.identityToken})
		return json.RawMessage(raw), true, err
	}
	if credentials.username != "" || credentials.password != "" {
		token := base64.StdEncoding.EncodeToString([]byte(credentials.username + ":" + credentials.password))
		raw, err := json.Marshal(map[string]string{"auth": token})
		return json.RawMessage(raw), true, err
	}
	return nil, false, nil
}

func authCredentials(auths map[string]json.RawMessage, candidates []string) (registryCredentials, bool) {
	for _, candidate := range candidates {
		if raw, ok := auths[candidate]; ok {
			var entry struct {
				Auth          string `json:"auth"`
				IdentityToken string `json:"identitytoken"`
			}
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				continue
			}
			if entry.IdentityToken != "" {
				return registryCredentials{identityToken: entry.IdentityToken}, true
			}
			if entry.Auth != "" {
				decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
				if err != nil {
					continue
				}
				username, password, ok := strings.Cut(string(decoded), ":")
				if !ok {
					continue
				}
				return registryCredentials{username: username, password: password}, true
			}
		}
	}
	return registryCredentials{}, false
}

func credentialHelper(cfg configFile, candidates []string) string {
	for _, candidate := range candidates {
		if helper := cfg.CredHelpers[candidate]; helper != "" {
			return helper
		}
	}
	return cfg.CredsStore
}

func (c Client) helperCredentials(ctx context.Context, helper string, candidates []string) (registryCredentials, error) {
	var lastErr error
	for _, candidate := range candidates {
		cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
		cmd.Stdin = strings.NewReader(candidate)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			var entry struct {
				Username string `json:"Username"`
				Secret   string `json:"Secret"`
			}
			if err := json.Unmarshal(out, &entry); err != nil {
				lastErr = fmt.Errorf("parse credential helper output")
				continue
			}
			if entry.Username == "<token>" && entry.Secret != "" {
				return registryCredentials{identityToken: entry.Secret}, nil
			}
			if entry.Username != "" || entry.Secret != "" {
				return registryCredentials{username: entry.Username, password: entry.Secret}, nil
			}
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg != "" {
			lastErr = fmt.Errorf("%s", msg)
		}
	}
	if lastErr != nil {
		return registryCredentials{}, lastErr
	}
	return registryCredentials{}, fmt.Errorf("credential helper returned no credentials")
}

func (c Client) registryCredentials(ctx context.Context, host string) (registryCredentials, error) {
	_, candidates, err := registryAuthCandidates(host)
	if err != nil {
		return registryCredentials{}, err
	}
	cfg, err := loadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return registryCredentials{}, nil
		}
		return registryCredentials{}, err
	}
	if credentials, ok := authCredentials(cfg.Auths, candidates); ok {
		return credentials, nil
	}
	if helper := credentialHelper(cfg, candidates); helper != "" {
		credentials, err := c.helperCredentials(ctx, helper, candidates)
		if err != nil {
			if anonymousRegistryHost(host) {
				return registryCredentials{}, nil
			}
			return registryCredentials{}, fmt.Errorf("docker credentials for %s unavailable through docker-credential-%s: %w", host, helper, err)
		}
		return credentials, nil
	}
	return registryCredentials{}, nil
}

func anonymousRegistryHost(host string) bool {
	return strings.EqualFold(strings.TrimSpace(host), "ttl.sh")
}

type imageReference struct {
	host       string
	repository string
	reference  string
	digest     string
}

func parseImageReference(image string) (imageReference, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return imageReference{}, fmt.Errorf("image reference is required")
	}
	named, digest, hasDigest := strings.Cut(image, "@")
	if named == "" {
		return imageReference{}, fmt.Errorf("invalid image reference %q", image)
	}
	host := "docker.io"
	remainder := named
	parts := strings.Split(named, "/")
	if len(parts) > 1 && isRegistryHost(parts[0]) {
		host = parts[0]
		remainder = strings.Join(parts[1:], "/")
	}
	repository := remainder
	reference := "latest"
	if tagIndex := strings.LastIndex(remainder, ":"); tagIndex > strings.LastIndex(remainder, "/") {
		repository = remainder[:tagIndex]
		reference = remainder[tagIndex+1:]
	}
	if repository == "" || reference == "" {
		return imageReference{}, fmt.Errorf("invalid image reference %q", image)
	}
	if host == "index.docker.io" {
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if hasDigest {
		digest = strings.TrimSpace(digest)
		if digest == "" {
			return imageReference{}, fmt.Errorf("invalid image digest reference %q", image)
		}
		reference = digest
	}
	return imageReference{host: host, repository: repository, reference: reference, digest: digest}, nil
}

func isRegistryHost(value string) bool {
	return value == "localhost" || strings.ContainsAny(value, ".:") || strings.HasPrefix(value, "[")
}

func (r imageReference) authHost() string {
	return r.host
}

func (r imageReference) apiHost() string {
	if r.host == "docker.io" || r.host == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return r.host
}

func (r imageReference) pullHost() string {
	if r.host == "index.docker.io" {
		return "docker.io"
	}
	return r.host
}

func (r imageReference) manifestURL() string {
	apiHost := r.apiHost()
	scheme := "https"
	if isLocalRegistry(apiHost) {
		scheme = "http"
	}
	return scheme + "://" + apiHost + "/v2/" + escapeRepositoryPath(r.repository) + "/manifests/" + url.PathEscape(r.reference)
}

func (r imageReference) digestRef(digest string) string {
	return r.pullHost() + "/" + r.repository + "@" + digest
}

func (c Client) resolveManifestDigest(ctx context.Context, ref imageReference, credentials registryCredentials) (string, error) {
	digest, err := c.resolveManifestDigestOnce(ctx, http.MethodHead, ref, credentials)
	if err == nil {
		return digest, nil
	}
	if !errors.Is(err, errManifestDigestUnavailable) {
		return "", err
	}
	return c.resolveManifestDigestOnce(ctx, http.MethodGet, ref, credentials)
}

var errManifestDigestUnavailable = errors.New("manifest digest unavailable")

func (c Client) resolveManifestDigestOnce(ctx context.Context, method string, ref imageReference, credentials registryCredentials) (string, error) {
	res, err := c.manifestRequest(ctx, method, ref, credentials)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusUnauthorized {
		challenge, ok := bearerChallenge(res.Header.Values("WWW-Authenticate"))
		if !ok {
			return "", fmt.Errorf("registry returned HTTP 401 resolving %s/%s:%s", ref.pullHost(), ref.repository, ref.reference)
		}
		if challenge["scope"] == "" {
			challenge["scope"] = "repository:" + ref.repository + ":pull"
		}
		token, err := c.registryBearerToken(ctx, challenge, credentials)
		if err != nil {
			return "", err
		}
		res, err = c.manifestRequest(ctx, method, ref, registryCredentials{identityToken: token})
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
	}
	if res.StatusCode == http.StatusMethodNotAllowed && method == http.MethodHead {
		return "", errManifestDigestUnavailable
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("registry returned HTTP %d resolving %s/%s:%s", res.StatusCode, ref.pullHost(), ref.repository, ref.reference)
	}
	digest := strings.TrimSpace(res.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", errManifestDigestUnavailable
	}
	return digest, nil
}

func (c Client) manifestRequest(ctx context.Context, method string, ref imageReference, credentials registryCredentials) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, ref.manifestURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ship")
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.v1+json",
	}, ", "))
	applyAuthorization(req, credentials)
	return c.httpClient().Do(req)
}

func escapeRepositoryPath(repository string) string {
	parts := strings.Split(repository, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (c Client) validateRegistryCredentials(ctx context.Context, host string, credentials registryCredentials) error {
	endpoint := registryEndpoint(host)
	res, err := c.registryRequest(ctx, http.MethodGet, endpoint, credentials)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	if res.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("registry /v2/ returned HTTP %d", res.StatusCode)
	}

	challenge, ok := bearerChallenge(res.Header.Values("WWW-Authenticate"))
	if !ok {
		return fmt.Errorf("registry /v2/ returned HTTP 401")
	}
	token, err := c.registryBearerToken(ctx, challenge, credentials)
	if err != nil {
		return err
	}
	tokenCredentials := registryCredentials{identityToken: token}
	res, err = c.registryRequest(ctx, http.MethodGet, endpoint, tokenCredentials)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("registry /v2/ bearer validation returned HTTP %d", res.StatusCode)
}

func (c Client) registryRequest(ctx context.Context, method, endpoint string, credentials registryCredentials) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ship-doctor")
	applyAuthorization(req, credentials)
	return c.httpClient().Do(req)
}

func (c Client) registryBearerToken(ctx context.Context, challenge map[string]string, credentials registryCredentials) (string, error) {
	realm := challenge["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry bearer challenge missing realm")
	}
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse registry bearer realm: %w", err)
	}
	query := tokenURL.Query()
	for _, key := range []string{"service", "scope"} {
		if value := challenge[key]; value != "" {
			query.Set(key, value)
		}
	}
	tokenURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ship-doctor")
	applyAuthorization(req, credentials)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return "", fmt.Errorf("registry token service returned HTTP %d", res.StatusCode)
	}
	var tokenResponse struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&tokenResponse); err != nil {
		return "", fmt.Errorf("parse registry token response: %w", err)
	}
	if tokenResponse.Token != "" {
		return tokenResponse.Token, nil
	}
	if tokenResponse.AccessToken != "" {
		return tokenResponse.AccessToken, nil
	}
	return "", fmt.Errorf("registry token service returned no token")
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func applyAuthorization(req *http.Request, credentials registryCredentials) {
	if credentials.identityToken != "" {
		req.Header.Set("Authorization", "Bearer "+credentials.identityToken)
		return
	}
	if credentials.username != "" || credentials.password != "" {
		req.SetBasicAuth(credentials.username, credentials.password)
	}
}

func registryEndpoint(host string) string {
	apiHost := host
	if host == "docker.io" || host == "index.docker.io" {
		apiHost = "registry-1.docker.io"
	}
	scheme := "https"
	if isLocalRegistry(apiHost) {
		scheme = "http"
	}
	return scheme + "://" + apiHost + "/v2/"
}

func isLocalRegistry(host string) bool {
	withoutPort := host
	if strings.HasPrefix(withoutPort, "[") {
		if end := strings.Index(withoutPort, "]"); end >= 0 {
			withoutPort = strings.Trim(withoutPort[:end+1], "[]")
		}
	} else if parsedHost, _, found := strings.Cut(host, ":"); found {
		withoutPort = parsedHost
	}
	return withoutPort == "localhost" || withoutPort == "127.0.0.1" || withoutPort == "::1"
}

func bearerChallenge(headers []string) (map[string]string, bool) {
	for _, header := range headers {
		value := strings.TrimSpace(header)
		if len(value) < len("bearer") || !strings.EqualFold(value[:len("bearer")], "bearer") {
			continue
		}
		return parseAuthParams(strings.TrimSpace(value[len("bearer"):])), true
	}
	return nil, false
}

func parseAuthParams(value string) map[string]string {
	params := map[string]string{}
	for value != "" {
		value = strings.TrimLeft(value, " ,")
		key, rest, ok := strings.Cut(value, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		rest = strings.TrimLeft(rest, " ")
		if strings.HasPrefix(rest, `"`) {
			rest = rest[1:]
			end := strings.Index(rest, `"`)
			if end < 0 {
				break
			}
			params[key] = rest[:end]
			value = rest[end+1:]
			continue
		}
		end := strings.Index(rest, ",")
		if end < 0 {
			params[key] = strings.TrimSpace(rest)
			break
		}
		params[key] = strings.TrimSpace(rest[:end])
		value = rest[end+1:]
	}
	return params
}
