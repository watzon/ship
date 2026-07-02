# Plan 005: Make SSH security options operator-overridable and validate ingress health paths

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 29fc466..HEAD -- internal/transport/ internal/config/ internal/ingress/`
> Note: at planning time `internal/config/config.go` and
> `internal/ingress/caddy.go` carried uncommitted changes;
> `internal/transport/ssh.go` was clean. Verify every excerpt below against
> the live code; on mismatch, STOP.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none (independent of plans 001-004)
- **Category**: security
- **Planned at**: commit `29fc466` (dirty working tree), 2026-07-01

## Why this matters

Two small, independent hardening gaps:

**Part A — SSH host-key policy is hardcoded and silently un-overridable.**
Ship connects to freshly provisioned servers with
`StrictHostKeyChecking=accept-new` (trust-on-first-use). That's a reasonable
default for this tool, but it's emitted as the FIRST `-o` argument, and
OpenSSH uses the first occurrence of an option — so an operator who sets
`StrictHostKeyChecking=yes` via their host `ssh_options` config is *silently
ignored*. Secrets env-files and the agent RPC stream travel over this
channel; a security-conscious operator who pre-populates known_hosts must be
able to require strict checking. Same mechanics apply to `BatchMode` and
`ConnectTimeout`.

**Part B — ingress health path values reach the Caddyfile unvalidated.**
Every other user-supplied value written into the generated Caddyfile
(domains, redirects, unhealthy_status) is validated to reject embedded
whitespace/newlines. `ingress.health.path` only gets a "starts with /" check
and the service-level `health.http` fallback gets no check at all, yet both
are printed raw as `health_uri %s` inside the reverse_proxy block — an
embedded newline injects arbitrary Caddyfile directives. Operator-authored
config bounds the threat, but it's an inconsistency with the sibling
validators and the one remaining unescaped path to the Caddyfile.

## Current state

### Part A — `internal/transport/ssh.go` (clean at planning time, 150 lines)

`args()` at ssh.go:95-118:

```go
func (s SSH) args(command string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
	}
	if s.Port > 0 { ... }
	if identityFile := strings.TrimSpace(s.IdentityFile); identityFile != "" { ... }
	if knownHostsFile := s.knownHostsFile(); knownHostsFile != "" { ... }
	if jumpHost := strings.TrimSpace(s.JumpHost); jumpHost != "" { ... }
	for _, option := range s.sortedOptions() {
		args = append(args, "-o", option)
	}
	args = append(args, s.Target(), command)
	return args
}
```

`sortedOptions()` at ssh.go:127-145 returns `key=value` strings from the
`Options map[string]string` field, trimmed and key-sorted. `Options` is
populated from host config (`ssh_options` in ship.yml, carried through
`state.HostFact.SSHOptions`).

Test conventions (`internal/transport/ssh_test.go`): tests stub the `ssh`
binary with a shell script on `PATH` that logs `"$@"` to a file, then assert
on logged args — see `TestSSHUsesConnectionOptions` (ssh_test.go:59-100).

### Part B — `internal/config/config.go` + `internal/ingress/caddy.go` (both had uncommitted changes)

`validateIngressHealth` (config.go:2788), the path check at ~2803:

```go
	if health.Path != "" && !strings.HasPrefix(health.Path, "/") {
		errs = append(errs, fmt.Sprintf("%s.path must start with /", label))
	}
```

Contrast the sibling pattern immediately below (~2807-2813):

```go
	for i, status := range health.UnhealthyStatus {
		...
		if strings.ContainsAny(status, " \t\r\n") {
			errs = append(errs, fmt.Sprintf("%s.unhealthy_status[%d] cannot contain whitespace", label, i))
		}
	}
```

The write site, `internal/ingress/caddy.go:269-276`:

```go
	path := strings.TrimSpace(health.Path)
	if path == "" {
		path = strings.TrimSpace(svc.Health.HTTP)
	}
	if path == "" {
		return
	}
	fmt.Fprintf(b, "    health_uri %s\n", path)
```

(`TrimSpace` strips leading/trailing whitespace only — internal newlines
survive.) `svc.Health.HTTP` is `HealthCheck.HTTP` (config.go:1915 /
struct at 2004 area) and has no whitespace validation anywhere;
`validateServices` starts at config.go:2537 and contains per-service health
checks (e.g. the `Rolling.HealthTimeoutSeconds < 0` check ~30 lines in).
`validateIngressHealth` is invoked from service validation at config.go:2706.

Config test conventions: `TestValidateRejects…` functions in
`internal/config/config_test.go` build a config (YAML literal or struct),
call `Validate()`, and assert the error string contains the expected message.

## Commands you will need

| Purpose   | Command                          | Expected on success |
|-----------|----------------------------------|---------------------|
| Transport | `go test ./internal/transport`   | all pass            |
| Config    | `go test ./internal/config`      | all pass            |
| Ingress   | `go test ./internal/ingress`     | all pass            |
| All       | `go test ./...`                  | all pass            |
| Vet       | `go vet ./...`                   | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `internal/transport/ssh.go`
- `internal/transport/ssh_test.go`
- `internal/config/config.go` (validation functions only)
- `internal/config/config_test.go`

**Out of scope** (do NOT touch):
- `internal/ingress/caddy.go` — the fix is validation-at-config-time, not
  escaping-at-generation-time. (Escaping would silently alter operator
  intent; rejection is the established pattern. Do not add escaping.)
- Changing the *default* host-key policy — `accept-new` stays the default;
  this plan only makes it overridable.
- `internal/agent`, `internal/cli` — no call-site changes are needed.
- Documenting the new override behavior in `docs/` (separate docs pass).

## Git workflow

- Branch: `advisor/005-ssh-hostkey-ingress-health`
- Commits: `fix(transport): let ssh_options override default connection options`
  and `fix(config): reject whitespace in ingress health paths`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Emit SSH default options only when not operator-overridden

In `internal/transport/ssh.go`, rework `args()` so the three defaults are
skipped when the same option key is present in `s.Options`. OpenSSH option
names are case-insensitive, so compare with `strings.EqualFold` against the
trimmed keys of `s.Options`:

```go
func (s SSH) args(command string) []string {
	var args []string
	for _, def := range [][2]string{
		{"BatchMode", "yes"},
		{"StrictHostKeyChecking", "accept-new"},
		{"ConnectTimeout", "15"},
	} {
		if !s.hasOption(def[0]) {
			args = append(args, "-o", def[0]+"="+def[1])
		}
	}
	// ... rest unchanged (Port, IdentityFile, knownHostsFile, JumpHost,
	// sortedOptions loop, Target, command)
}

func (s SSH) hasOption(name string) bool {
	for key := range s.Options {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}
```

Preserve the existing behavior in every other respect (ordering of the
remaining args, dry-run strings, etc.).

**Verify**: `go test ./internal/transport` → all pass (existing tests cover
the default emission indirectly via arg-log assertions).

### Step 2: Tests for the override behavior

In `internal/transport/ssh_test.go`, modeled on
`TestSSHUsesConnectionOptions` (stub `ssh` script logging `"$@"`):

- `TestSSHDefaultsEmittedWithoutOverrides`: no `Options`; logged args contain
  `BatchMode=yes`, `StrictHostKeyChecking=accept-new`, `ConnectTimeout=15`.
- `TestSSHOptionsOverrideDefaultConnectionOptions`: `Options:
  map[string]string{"StrictHostKeyChecking": "yes", "connecttimeout": "30"}`;
  logged args contain `StrictHostKeyChecking=yes` and `connecttimeout=30`,
  and do NOT contain `accept-new` or `ConnectTimeout=15`; `BatchMode=yes`
  still present. (The lowercase key also proves case-insensitive matching.)

**Verify**: `go test ./internal/transport -v` → all PASS including the 2 new tests.

### Step 3: Validate ingress health path and service health.http

In `internal/config/config.go`:

1. In `validateIngressHealth` (config.go:2788), after the `must start with /`
   check, add:

   ```go
   if strings.ContainsAny(health.Path, " \t\r\n") {
   	errs = append(errs, fmt.Sprintf("%s.path cannot contain whitespace", label))
   }
   ```

2. In `validateServices` (config.go:2537), inside the per-service loop where
   other `svc.Health` / rolling checks live, add the equivalent check for the
   service-level HTTP health path (used as the Caddyfile `health_uri`
   fallback):

   ```go
   if strings.ContainsAny(svc.Health.HTTP, " \t\r\n") {
   	errs = append(errs, fmt.Sprintf("%sservice %q health.http cannot contain whitespace", prefix, name))
   }
   ```

   Match the exact label/prefix formatting used by the neighboring error
   strings in that loop (read them and mirror; the `%sservice %q` shape above
   is indicative, not gospel).

**Verify**: `go test ./internal/config` → all pass (existing tests must not
break — valid paths like `/up` contain no whitespace).

### Step 4: Validation tests

In `internal/config/config_test.go`, following the `TestValidateRejects…`
convention:

- `TestValidateRejectsIngressHealthPathWithWhitespace`: config whose
  `ingress.health.path` is `"/up\nevil directive"` → `Validate()` error
  containing `cannot contain whitespace`.
- `TestValidateRejectsServiceHealthHTTPWithWhitespace`: same for
  `health.http`.

**Verify**: `go test ./internal/config -run TestValidateRejects -v` → PASS,
then `go test ./...` → all pass.

## Test plan

- 2 transport tests (Step 2) — stub-ssh arg-log pattern from
  `TestSSHUsesConnectionOptions`.
- 2 config validation tests (Step 4) — `TestValidateRejects…` pattern.
- Full suite as regression gate: `go test ./...`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go test ./...` exits 0; 4 new tests exist and pass
- [ ] With `Options{"StrictHostKeyChecking":"yes"}`, generated args contain no `accept-new` (proven by the Step 2 test)
- [ ] `grep -n "cannot contain whitespace" internal/config/config.go` shows the two new checks
- [ ] `go vet ./...` exits 0
- [ ] `git status` shows changes only in the 4 in-scope files (plus `plans/README.md`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any excerpt doesn't match the live code (config.go/caddy.go were mid-edit
  at planning time; ssh.go was clean but confirm anyway).
- An existing test asserts the defaults are ALWAYS present in a case where an
  override is also set (would mean some caller depends on the old
  first-wins behavior — report it).
- The Step 3 config checks break existing fixtures (would mean real configs
  in tests carry whitespace paths — report which, don't weaken the check).
- You find `health_uri` written from any third source besides
  `health.Path`/`svc.Health.HTTP` (plan assumed two).

## Maintenance notes

- The `accept-new` default is deliberate (Ship provisions fresh hosts whose
  keys can't be known in advance). A future hardening step could capture the
  host key at provision time via the cloud API/console where available and
  pre-populate known_hosts — out of scope here, recorded as a direction idea.
- If new `-o` defaults are ever added to `args()`, they must go through the
  same "skip if operator-set" path; a reviewer should reject a raw prepend.
- If ingress ever grows more free-form string directives (custom headers
  etc.), each needs the same whitespace validation before hitting the
  Caddyfile — grep `Fprintf(b, "` in `internal/ingress/caddy.go` during
  review of such features.
