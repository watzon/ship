# Plan 004: Lock in the secrets scoping contract with tests

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 29fc466..HEAD -- internal/secrets/`
> Note: at planning time `internal/secrets/secrets.go` carried uncommitted
> changes (the scoped-secrets behavior below IS that in-flight work);
> `secrets_test.go` did not. This plan only ADDS tests, so it is safe to run
> against the working tree — but every excerpt below must match the live
> code first. On mismatch, STOP.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW (test-only — no production code changes)
- **Depends on**: none
- **Category**: tests
- **Planned at**: commit `29fc466` (dirty working tree), 2026-07-01

## Why this matters

`internal/secrets` is the layer that decides *which secret values reach which
containers*. Its scoping API — `RequiredScopes` / `RenderScopedForEnv` — has
8 production call sites (deploy, promote, rollback, accessory paths in
`internal/cli/root.go`) and **zero test coverage**: no test file in the repo
mentions either function. The package also recently changed behavior:
top-level `secrets:` names are no longer auto-injected into every
service/accessory — each service must opt in via its own `secrets:` list
(this matches the documented example in
`skills/ship/references/config-mapping.md`, where `SESSION_SECRET` appears in
both the root list and each consuming service's list). That is a
secret-delivery semantic with no regression guard. Multi-recipient encryption
and `UnsetStoredSecret` are similarly untested. These tests freeze the
contract so future refactors can't silently change which containers get
which secrets.

## Current state

All in `internal/secrets/secrets.go` (working-tree state):

- `RequiredScopes` (secrets.go:258-282) — builds scope→names purely from
  per-service and per-accessory lists; root `cfg.Secrets` is NOT merged in:

  ```go
  func RequiredScopes(cfg *config.Config) (map[string][]string, error) {
  	scopes := map[string][]string{}
  	...
  	for serviceName, svc := range cfg.Services {
  		names, err := mergeSecretNames(svc.Secrets)
  		...
  		scopes["service-"+serviceName] = names
  	}
  	for accessoryName, acc := range cfg.Accessories {
  		names, err := mergeSecretNames(acc.Secrets)
  		...
  		scopes["accessory-"+accessoryName] = names
  	}
  	return scopes, nil
  }
  ```

  (Scopes with zero names are omitted — the `if len(names) > 0` guard.)

- `RenderScopedForEnv` (secrets.go:137-188) — resolves the union of all
  scope names once via `ResolveValues`, renders one env file per scope,
  filters to `onlyScopes` when given, and namespaces digests as
  `scope+":"+name` in the aggregate `Digests` map.

- `mergeSecretNames` (secrets.go:292-313) — trims, validates, dedupes, sorts.

- `UnsetStoredSecret` (secrets.go:598-608) — read store, `delete`, write back.

- `WriteStoreWithRecipients` (secrets.go:548-581) — encrypts the JSON value
  map to N age recipients.

- Existing test conventions (`internal/secrets/secrets_test.go`, 164 lines):
  plain stdlib tests; env-var based tests use `t.Setenv`; store-based tests
  build a real age identity in a `t.TempDir()` — the exemplar to model on is
  `TestRenderForEnvUsesEncryptedStoreDotenvAndEnvPrecedence`
  (secrets_test.go:108-152): generate identity via
  `age.GenerateX25519Identity()`, write `identity.txt`, build
  `SourceOptions{EnvName, StateDir, IdentityFile, EnvFiles}`, `InitStore`,
  `SetStoredSecret`, then assert on rendered content.

- The documented opt-in model:
  `skills/ship/references/config-mapping.md` (lines ~88-108) shows a config
  where the root `secrets:` list contains `SESSION_SECRET` and the `web`
  service *also* lists `SESSION_SECRET` under its own `secrets:` — i.e.
  root list = "names that must exist", service list = "names this service's
  container receives".

## Commands you will need

| Purpose  | Command                                        | Expected on success |
|----------|------------------------------------------------|---------------------|
| Tests    | `go test ./internal/secrets`                   | all pass            |
| One test | `go test ./internal/secrets -run TestRequiredScopes -v` | PASS      |
| Vet      | `go vet ./internal/secrets`                    | exit 0              |

## Scope

**In scope** (the only file you should modify):
- `internal/secrets/secrets_test.go`

**Out of scope** (do NOT touch):
- `internal/secrets/secrets.go` — this plan documents behavior, it does not
  change it. If a test you write fails, that's a finding to report, not code
  to fix (see STOP conditions).
- `internal/cli/root.go` call sites; `internal/config`.

## Git workflow

- Branch: `advisor/004-secrets-scoping-tests`
- Commit: `test(secrets): cover scoped rendering, store round-trips, and multi-recipient encryption`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: `RequiredScopes` contract tests

Add `TestRequiredScopesBuildsPerServiceAndAccessoryScopes`:

- Config fixture (build the `*config.Config` struct literal directly, as the
  existing tests do): root `Secrets: []string{"ROOT_ONLY", "SHARED"}`;
  service `web` with `Secrets: []string{"SHARED", "WEB_KEY", "WEB_KEY", " "}`;
  service `worker` with no secrets; accessory `db` with
  `Secrets: []string{"DB_PASS"}`.
- Assert: scopes map has exactly keys `service-web` and `accessory-db`
  (no `service-worker` — empty scopes omitted); `service-web` ==
  `["SHARED", "WEB_KEY"]` (deduped, sorted, blank dropped); **`ROOT_ONLY`
  appears in no scope** — this is the assertion that locks in the opt-in
  model.
- Add a second case: a service secret named `"1BAD"` returns an error
  (`validateName` rejects leading digit).

**Verify**: `go test ./internal/secrets -run TestRequiredScopes -v` → PASS.

### Step 2: `RenderScopedForEnv` tests

Add `TestRenderScopedForEnvRendersAndFiltersScopes`, modeled directly on
`TestRenderForEnvUsesEncryptedStoreDotenvAndEnvPrecedence` (same
identity/`InitStore`/`SetStoredSecret` setup):

- Store secrets `WEB_KEY=web-value`, `DB_PASS=db-value`.
- Config: service `web` (secrets `WEB_KEY`), accessory `db` (secrets `DB_PASS`).
- Call `RenderScopedForEnv(cfg, opts)` with no scope filter. Assert:
  - `Scopes["service-web"].Content == "WEB_KEY=web-value\n"` and the db scope
    file does NOT contain `WEB_KEY` (scope isolation — the load-bearing
    assertion).
  - `Digests` keys are `"service-web:WEB_KEY"` and `"accessory-db:DB_PASS"`.
  - `Checks` covers both names with `Present: true`.
  - `Scopes["service-web"].Redacted` does not contain `web-value`.
- Call again with `RenderScopedForEnv(cfg, opts, "service-web")` and assert
  only that scope is present in `Scopes`.

**Verify**: `go test ./internal/secrets -run TestRenderScopedForEnv -v` → PASS.

### Step 3: Store round-trip and multi-recipient tests

- `TestUnsetStoredSecretRemovesValue`: `InitStore` → `SetStoredSecret(NAME)` →
  `UnsetStoredSecret(NAME)` → `ReadStore` returns a map without NAME (and no
  error when unsetting a name that was never stored).
- `TestWriteStoreSupportsMultipleRecipients`: generate two identities;
  `WriteStoreWithRecipients(opts, values, []age.Recipient{r1, r2})`; then
  `ReadStore` succeeds with identity 1's file AND (fresh `SourceOptions` with
  the other identity file) with identity 2's file, returning equal maps.

**Verify**: `go test ./internal/secrets -v` → all tests PASS.

## Test plan

This plan *is* a test plan: 4 new test functions in
`internal/secrets/secrets_test.go` (steps 1-3), all following the existing
`t.TempDir()` + real-age-identity style of secrets_test.go:108. No mocks —
the package's tests exercise real age encryption already; keep that.

Final verification: `go test ./internal/secrets` → all pass;
`go test ./...` → all pass (nothing else should be affected).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go test ./internal/secrets` exits 0
- [ ] `grep -c "func Test" internal/secrets/secrets_test.go` ≥ 10 (6 existing + 4 new)
- [ ] `grep -n "RequiredScopes\|RenderScopedForEnv" internal/secrets/secrets_test.go` → matches exist
- [ ] `git status` shows only `internal/secrets/secrets_test.go` modified (plus `plans/README.md`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any excerpt above doesn't match `internal/secrets/secrets.go` on disk
  (in-flight code may have evolved).
- A test you wrote per this spec FAILS: that means the documented contract
  and the code disagree (e.g. root secrets ARE injected into scopes, or
  scope files leak names). Do not adjust the assertion to match the code —
  report the mismatch; it's exactly what these tests exist to catch, and the
  maintainer must rule on intent.
- You need to modify `secrets.go` to make anything testable.

## Maintenance notes

- The opt-in scoping model (root `secrets:` = required names; per-service
  `secrets:` = what that container receives) is currently documented only in
  the agent-facing `skills/ship/references/config-mapping.md`, not in the
  human docs (`docs/configuration/README.md`). A docs follow-up should state
  it explicitly — flagged separately in the plans index.
- If a future feature reintroduces shared/global secret injection, these
  tests will fail loudly — that's intended; update the contract consciously.
- Reviewer focus: assertions that prove *absence* (scope isolation,
  `ROOT_ONLY` in no scope) are the valuable ones; presence assertions alone
  would pass under the old auto-inject behavior too.
