package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/watzon/ship/internal/config"
)

const digestLength = 12

type Check struct {
	Name    string
	Present bool
	Digest  string
}

type RenderedEnvFile struct {
	Content  string
	Redacted string
	Checks   []Check
	Digests  map[string]string
}

type SourceOptions struct {
	EnvName      string
	ConfigPath   string
	StateDir     string
	EnvFiles     []string
	IdentityFile string
	LookupEnv    func(string) (string, bool)
}

type ScopedRenderedEnvFiles struct {
	Scopes  map[string]RenderedEnvFile
	Checks  []Check
	Digests map[string]string
}

type DigestDiff struct {
	Missing []string
	Changed []string
	Extra   []string
}

func Verify(cfg *config.Config) ([]Check, error) {
	names, err := RequiredNames(cfg)
	if err != nil {
		return nil, err
	}
	checks := make([]Check, 0, len(names))
	var missing []string
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		check := Check{Name: name, Present: ok}
		if ok {
			check.Digest = Digest(value)
		} else {
			missing = append(missing, name)
		}
		checks = append(checks, check)
	}
	if len(missing) > 0 {
		return checks, fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	return checks, nil
}

func RenderEnvFile(cfg *config.Config) (RenderedEnvFile, error) {
	names, err := RequiredNames(cfg)
	if err != nil {
		return RenderedEnvFile{}, err
	}
	checks := make([]Check, 0, len(names))
	digests := make(map[string]string, len(names))
	values := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		check := Check{Name: name, Present: ok}
		if ok {
			check.Digest = Digest(value)
			digests[name] = check.Digest
			values[name] = value
		} else {
			missing = append(missing, name)
		}
		checks = append(checks, check)
	}
	rendered := RenderedEnvFile{
		Checks:  checks,
		Digests: digests,
	}
	if len(missing) > 0 {
		return rendered, fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	content, err := renderValues(names, values, false)
	if err != nil {
		return rendered, err
	}
	redacted, err := renderValues(names, redactedValues(names, digests), true)
	if err != nil {
		return rendered, err
	}
	rendered.Content = content
	rendered.Redacted = redacted
	return rendered, nil
}

func VerifyForEnv(cfg *config.Config, opts SourceOptions) ([]Check, error) {
	rendered, err := RenderForEnv(cfg, opts)
	return rendered.Checks, err
}

func RenderForEnv(cfg *config.Config, opts SourceOptions) (RenderedEnvFile, error) {
	names, err := RequiredNames(cfg)
	if err != nil {
		return RenderedEnvFile{}, err
	}
	values, err := ResolveValues(names, opts)
	if err != nil {
		return RenderedEnvFile{}, err
	}
	return renderEnvFileFromValues(names, values)
}

func RenderScopedForEnv(cfg *config.Config, opts SourceOptions) (ScopedRenderedEnvFiles, error) {
	scopes, err := RequiredScopes(cfg)
	if err != nil {
		return ScopedRenderedEnvFiles{}, err
	}
	allNames := map[string]struct{}{}
	for _, names := range scopes {
		for _, name := range names {
			allNames[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(allNames))
	for name := range allNames {
		names = append(names, name)
	}
	sort.Strings(names)
	values, err := ResolveValues(names, opts)
	if err != nil {
		return ScopedRenderedEnvFiles{}, err
	}
	out := ScopedRenderedEnvFiles{
		Scopes:  map[string]RenderedEnvFile{},
		Digests: map[string]string{},
	}
	checksByName := map[string]Check{}
	for _, name := range names {
		checksByName[name] = Check{Name: name, Present: true, Digest: Digest(values[name])}
	}
	for scope, scopeNames := range scopes {
		rendered, err := renderEnvFileFromValues(scopeNames, values)
		if err != nil {
			return ScopedRenderedEnvFiles{}, err
		}
		out.Scopes[scope] = rendered
		for name, digest := range rendered.Digests {
			out.Digests[scope+":"+name] = digest
		}
	}
	for _, name := range names {
		out.Checks = append(out.Checks, checksByName[name])
	}
	return out, nil
}

func ResolveValues(names []string, opts SourceOptions) (map[string]string, error) {
	values := map[string]string{}
	if opts.EnvName != "" {
		storeValues, err := ReadStore(opts)
		if err != nil {
			return nil, err
		}
		for name, value := range storeValues {
			values[name] = value
		}
	}
	for _, file := range opts.EnvFiles {
		fileValues, err := ReadDotenv(file)
		if err != nil {
			return nil, err
		}
		for name, value := range fileValues {
			values[name] = value
		}
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var missing []string
	for _, name := range names {
		if value, ok := lookup(name); ok {
			values[name] = value
		}
		if _, ok := values[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	for _, name := range names {
		if err := validateEnvFileValue(name, values[name]); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func RequiredNames(cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	seen := map[string]struct{}{}
	names := make([]string, 0, len(cfg.Secrets))
	for _, raw := range cfg.Secrets {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if err := validateName(name); err != nil {
			return nil, err
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func RequiredScopes(cfg *config.Config) (map[string][]string, error) {
	scopes := map[string][]string{}
	if cfg == nil {
		return scopes, nil
	}
	shared, err := RequiredNames(cfg)
	if err != nil {
		return nil, err
	}
	for serviceName, svc := range cfg.Services {
		names, err := mergeSecretNames(shared, svc.Secrets)
		if err != nil {
			return nil, err
		}
		if len(names) > 0 {
			scopes["service-"+serviceName] = names
		}
	}
	for accessoryName, acc := range cfg.Accessories {
		names, err := mergeSecretNames(shared, acc.Secrets)
		if err != nil {
			return nil, err
		}
		if len(names) > 0 {
			scopes["accessory-"+accessoryName] = names
		}
	}
	return scopes, nil
}

func RequiredNamesForScope(cfg *config.Config, scope string) ([]string, error) {
	scopes, err := RequiredScopes(cfg)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), scopes[scope]...), nil
}

func mergeSecretNames(groups ...[]string) ([]string, error) {
	seen := map[string]struct{}{}
	var names []string
	for _, group := range groups {
		for _, raw := range group {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if err := validateName(name); err != nil {
				return nil, err
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:digestLength]
}

func DigestMap(checks []Check) map[string]string {
	digests := make(map[string]string, len(checks))
	for _, check := range checks {
		if check.Present && check.Digest != "" {
			digests[check.Name] = check.Digest
		}
	}
	return digests
}

func Diff(local, release map[string]string) DigestDiff {
	var diff DigestDiff
	for name, localDigest := range local {
		releaseDigest, ok := release[name]
		switch {
		case !ok:
			diff.Missing = append(diff.Missing, name)
		case releaseDigest != localDigest:
			diff.Changed = append(diff.Changed, name)
		}
	}
	for name := range release {
		if _, ok := local[name]; !ok {
			diff.Extra = append(diff.Extra, name)
		}
	}
	sort.Strings(diff.Missing)
	sort.Strings(diff.Changed)
	sort.Strings(diff.Extra)
	return diff
}

func (d DigestDiff) Empty() bool {
	return len(d.Missing) == 0 && len(d.Changed) == 0 && len(d.Extra) == 0
}

func RemoteEnvFilePath(envName, scope string) string {
	return path.Join(config.RemoteStateDir, "secrets", safePathPart(envName), safePathPart(scope)+".env")
}

func ExampleFile(cfg *config.Config) string {
	names, err := RequiredNames(cfg)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s=\n", name)
	}
	return b.String()
}

func renderEnvFileFromValues(names []string, values map[string]string) (RenderedEnvFile, error) {
	checks := make([]Check, 0, len(names))
	digests := make(map[string]string, len(names))
	for _, name := range names {
		value := values[name]
		digest := Digest(value)
		digests[name] = digest
		checks = append(checks, Check{Name: name, Present: true, Digest: digest})
	}
	content, err := renderValues(names, values, false)
	if err != nil {
		return RenderedEnvFile{}, err
	}
	redacted, err := renderValues(names, redactedValues(names, digests), true)
	if err != nil {
		return RenderedEnvFile{}, err
	}
	return RenderedEnvFile{
		Content:  content,
		Redacted: redacted,
		Checks:   checks,
		Digests:  digests,
	}, nil
}

func renderValues(names []string, values map[string]string, redacted bool) (string, error) {
	var b strings.Builder
	for _, name := range names {
		value := values[name]
		if !redacted {
			if err := validateEnvFileValue(name, value); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}
	return b.String(), nil
}

func StorePath(opts SourceOptions) string {
	stateDir := opts.StateDir
	if strings.TrimSpace(stateDir) == "" {
		if strings.TrimSpace(opts.ConfigPath) != "" {
			stateDir = filepath.Join(filepath.Dir(opts.ConfigPath), config.LocalStateDir)
		} else {
			stateDir = config.LocalStateDir
		}
	}
	envName := opts.EnvName
	if envName == "" {
		envName = "default"
	}
	return filepath.Join(stateDir, "secrets", safePathPart(envName)+".age")
}

func RecipientsPath(opts SourceOptions) string {
	return strings.TrimSuffix(StorePath(opts), ".age") + ".recipients"
}

func InitStore(opts SourceOptions, recipientText string) error {
	recipient, err := parseRecipient(recipientText)
	if err != nil {
		return err
	}
	if err := WriteStoreWithRecipients(opts, map[string]string{}, []age.Recipient{recipient}); err != nil {
		return err
	}
	return WriteRecipients(opts, []string{strings.TrimSpace(recipientText)})
}

func ReadStore(opts SourceOptions) (map[string]string, error) {
	path := StorePath(opts)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]string{}, nil
	} else if err != nil {
		return nil, err
	}
	identityFile := strings.TrimSpace(opts.IdentityFile)
	if identityFile == "" {
		identityFile = os.Getenv("SHIP_SECRETS_IDENTITY_FILE")
	}
	if identityFile == "" {
		return nil, fmt.Errorf("secrets identity file is required to read %s", path)
	}
	identity, err := readIdentity(identityFile)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	reader, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		return nil, fmt.Errorf("decrypt secrets %s: %w", path, err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if len(strings.TrimSpace(string(plain))) == 0 {
		return values, nil
	}
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, fmt.Errorf("decode secrets %s: %w", path, err)
	}
	for name, value := range values {
		if err := validateName(name); err != nil {
			return nil, err
		}
		if err := validateEnvFileValue(name, value); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func WriteStore(opts SourceOptions, values map[string]string, recipientText string) error {
	recipientsText := []string{recipientText}
	if strings.TrimSpace(recipientText) == "" {
		var err error
		recipientsText, err = ReadRecipients(opts)
		if err != nil {
			return err
		}
	}
	recipients := make([]age.Recipient, 0, len(recipientsText))
	for _, text := range recipientsText {
		recipient, err := parseRecipient(text)
		if err != nil {
			return err
		}
		recipients = append(recipients, recipient)
	}
	return WriteStoreWithRecipients(opts, values, recipients)
}

func ReadRecipients(opts SourceOptions) ([]string, error) {
	data, err := os.ReadFile(RecipientsPath(opts))
	if err != nil {
		return nil, fmt.Errorf("read secrets recipients: %w", err)
	}
	var recipients []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		recipients = append(recipients, line)
	}
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no age recipients configured in %s", RecipientsPath(opts))
	}
	return recipients, nil
}

func WriteRecipients(opts SourceOptions, recipients []string) error {
	var b strings.Builder
	for _, recipient := range recipients {
		recipient = strings.TrimSpace(recipient)
		if recipient == "" {
			continue
		}
		if _, err := parseRecipient(recipient); err != nil {
			return err
		}
		fmt.Fprintln(&b, recipient)
	}
	path := RecipientsPath(opts)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func WriteStoreWithRecipients(opts SourceOptions, values map[string]string, recipients []age.Recipient) error {
	if len(recipients) == 0 {
		return fmt.Errorf("at least one age recipient is required")
	}
	for name, value := range values {
		if err := validateName(name); err != nil {
			return err
		}
		if err := validateEnvFileValue(name, value); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipients...)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	path := StorePath(opts)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, encrypted.Bytes(), 0o644)
}

func SetStoredSecret(opts SourceOptions, recipientText, name, value string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateEnvFileValue(name, value); err != nil {
		return err
	}
	values, err := ReadStore(opts)
	if err != nil {
		return err
	}
	values[name] = value
	return WriteStore(opts, values, recipientText)
}

func UnsetStoredSecret(opts SourceOptions, recipientText, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	values, err := ReadStore(opts)
	if err != nil {
		return err
	}
	delete(values, name)
	return WriteStore(opts, values, recipientText)
}

func ReadDotenv(filename string) (map[string]string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", filename, i+1)
		}
		name := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if len(value) >= 2 {
			quote := value[0]
			if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
				value = value[1 : len(value)-1]
			}
		}
		if err := validateName(name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", filename, i+1, err)
		}
		if err := validateEnvFileValue(name, value); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", filename, i+1, err)
		}
		values[name] = value
	}
	return values, nil
}

func parseRecipient(value string) (age.Recipient, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("age recipient is required")
	}
	recipient, err := age.ParseX25519Recipient(value)
	if err != nil {
		return nil, fmt.Errorf("parse age recipient: %w", err)
	}
	return recipient, nil
}

func readIdentity(filename string) (age.Identity, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		identity, err := age.ParseX25519Identity(line)
		if err == nil {
			return identity, nil
		}
	}
	return nil, fmt.Errorf("no age identity found in %s", filename)
}

func redactedValues(names []string, digests map[string]string) map[string]string {
	values := make(map[string]string, len(names))
	for _, name := range names {
		values[name] = "<redacted:" + digests[name] + ">"
	}
	return values
}

func validateName(name string) error {
	for i, r := range name {
		if i == 0 {
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return fmt.Errorf("invalid secret name %q", name)
			}
			continue
		}
		if r != '_' &&
			(r < 'A' || r > 'Z') &&
			(r < 'a' || r > 'z') &&
			(r < '0' || r > '9') {
			return fmt.Errorf("invalid secret name %q", name)
		}
	}
	return nil
}

func validateEnvFileValue(name, value string) error {
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("secret %s contains a NUL byte and cannot be rendered in a Docker env file", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("secret %s contains a newline and cannot be rendered exactly in a Docker env file", name)
	}
	return nil
}

func safePathPart(value string) string {
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
	out := strings.Trim(b.String(), ".-_")
	if out == "" {
		return "unknown"
	}
	return out
}
