package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

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
