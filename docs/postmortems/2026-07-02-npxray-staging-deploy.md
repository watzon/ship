# Postmortem: npxray staging deploy (2026-07-02)

Staging API deploy via GitHub Actions eventually rolled out new web/worker containers, but external HTTPS traffic stayed down until manual intervention. Root cause was **Ship v0.4.0** (installed by `go install github.com/watzon/ship/cmd/ship@latest` in CI) generating a **Caddyfile that Caddy 2 rejects**. Fixes for that and several related issues already exist on `main` (`ac557d1` and follow-ups) but had not been released at the time of this incident.

Production was untouched (still on Kamal). Web deploy to Vercel succeeded.

---

## What we attempted

| Surface | Method | Result |
| --- | --- | --- |
| Web (`npxray.dev`) | Push `web` `main` → Vercel | Deployed |
| API staging | Push `api` `staging` → CI `ship deploy staging` | CI green, but staging HTTPS down |
| API production | Not attempted | Still Kamal (`kamal-proxy` + old containers) |

---

## Incident timeline

1. **CI quality gate failed** — flaky API test (`engine subprocess timeout`). Not a Ship bug; fixed in `npxray/api`.
2. **CI deploy failed** — `ship deploy staging` rejected config:
   ```
   environment "staging" service "web" image.build or image.ref is required
   environment "staging" service "web" pool is required
   ```
   Staging env had only partial `services.web` overrides (ingress + env). Workaround: duplicated full service blocks under `environments.staging` in `npxray/api/ship.yml`.
3. **CI deploy succeeded** — new containers started (`3810aa6b92fb-…`), but:
   - No HTTPS listener on 443 (Caddy crash-looping)
   - Redis accessory missing/stopped
   - `ship status staging` showed drift (`wrong_release`, `extra` containers)
4. **Manual recovery required**:
   - `ship accessory deploy staging npxray-redis`
   - Fix invalid Caddyfile on disk (multiline health block)
   - Fix Caddy upstream from `npxray-staging:4185` to `web:4185`
   - `docker restart` Caddy + web/worker

---

## Ship bugs and gaps (ordered by severity)

### P0 — Invalid Caddyfile when service has HTTP health check

**Symptom:** Caddy crash-loops; `curl https://api-staging.npxray.dev` → connection refused.

**Bad generated config (v0.4.0):**

```caddyfile
handle /_ship/health { respond "ok" 200 }
```

**Caddy error:**

```
Unexpected next token after '{' on same line, at /etc/caddy/Caddyfile:3
```

**Fix on `main` (`ac557d1`):**

```go
// BEFORE (v0.4.0)
fmt.Fprintf(&b, "  handle /_ship/health { respond \"ok\" 200 }\n")

// AFTER (main)
b.WriteString("  handle /_ship/health {\n")
b.WriteString("    respond \"ok\" 200\n")
b.WriteString("  }\n")
```

**File:** `internal/ingress/caddy.go`

**Why tests missed it:** `TestGeneratedCaddyfileValidatesWithCaddyBinary` generates a config with `Health.HTTP` but did not assert the health block is multiline. The v0.4.0 generator produced single-line syntax that fails `caddy validate`.

**Recommended fixes:**

1. Release **v0.4.1+** including `ac557d1`
2. Extend integration test to assert health block is never single-line
3. Add unit test asserting `handle /_ship/health { respond` never appears in output

---

### P0 — Deploy succeeds even when ingress is broken (v0.4.0)

**Symptom:** GitHub Actions deploy job exits 0 while Caddy is `Restarting (1)`.

**Cause:** `validateCaddyfile()` and `waitForCaddyContainer()` were added in `ac557d1` — not in v0.4.0.

**Relevant code (main only):**

- `internal/deployment/deployment.go` — `validateCaddyfile()` runs `caddy validate` in a one-off container before apply
- `waitForCaddyContainer()` — 5s grace period after start

**Recommended fixes:**

1. Ship with `ac557d1` in the next release
2. Consider failing deploy if Caddy exits during grace window (include log tail in error)
3. Post-deploy smoke: optional hook or built-in check that `:443` is listening

---

### P1 — Co-located ingress uses SSH hostname instead of Docker service alias

**Symptom:** Generated upstream was `reverse_proxy npxray-staging:4185` instead of `reverse_proxy web:4185`.

On a single-box deploy (ingress pool = app pool), Caddy runs in Docker and should target the **Docker network alias** (`web`), not the SSH config hostname.

From inside the Caddy container, `npxray-staging` resolves to `127.0.1.1`, so health checks fail with `connection refused` and external requests return 503 (`no upstreams available`), even when the app is healthy on `127.0.0.1:4185`.

**Fix:** `ac557d1` — `dockerUpstreamServices()` + `upstreamHost()` in `internal/ingress/caddy.go`.

**Test already exists:** `TestGenerateCaddyfileUsesServiceNameForCoLocatedIngress` — but only on unreleased `main`.

**Recommended:** Release + add an end-to-end test that co-located ingress generates `web:PORT` not `<ssh-hostname>:PORT`.

---

### P1 — `ship plan` promises accessory "ensure" but `ship deploy` does not deploy accessories

**Symptom:** After Kamal → Ship cutover, Redis was **Exited**; `ship accessory status staging npxray-redis` → `missing`.

**Cause:** `internal/planner/planner.go` adds plan steps:

```
- accessory npxray-redis: ensure redis:7-alpine on pool app
```

…but `deployCmd` in `internal/cli/root.go` never calls accessory deploy. Accessories are a **separate** command (`ship accessory deploy`).

**Impact:** Web/worker started with `NPXRAY_JOBS_REDIS_URL=redis://npxray-redis:6379` but nothing was listening → `ECONNREFUSED` → Caddy returned 503 even after ingress fix.

**Recommended fixes (pick one):**

1. **Auto-ensure accessories during deploy** (preferred — matches plan output)
2. **Remove accessory lines from plan** and document accessories as a prerequisite
3. **Fail deploy** if required accessories are missing/unhealthy (doctor-style gate)

---

### P1 — Environment service overrides shallow-replace instead of deep-merge (v0.4.0)

**Symptom:** Partial `environments.staging.services.web` overrides caused deploy validation failure.

**Fix on `main`:** `155d3a0` / `1af471b` — `TestResolveEnvironmentDeepMergesPartialServiceOverrides` now passes.

**Recommended:** Release + document that env overrides **merge** into root `services` (post-release behavior).

---

### P2 — `ship agent upgrade` fails from `go install` binary on macOS

**Symptom:**

```
local ship binary is darwin/arm64 but remote host needs linux/amd64;
install a matching release binary or run ship from a checkout with Go installed
```

**Context:** Staging agent stuck at `ship version 0.4.0 protocol=1-2`.

**Fix on `main`:** `de46d1c` — `internal/shipbinary` cross-compiles from module root or downloads release tarball.

**Why it still failed:** Command was run from `npxray/api`, not the Ship checkout — `moduleRoot()` returns `errNoModuleRoot`, and no release asset exists for unreleased fixes.

**Recommended:**

1. **Cut a release** with GitHub release assets (`ship_*_linux_amd64.tar.gz`)
2. CI should pin a **released** Ship version, not `@latest` from a stale tag
3. `ship agent upgrade` error should mention: *"run from ship repo checkout"* or *"install release X"*

---

### P2 — Deploy drift / confusing status after rollout

**Symptom:**

```
drift detected  missing=2  wrong_release=2  extra=2
environment staging  release aa7945f… (healthy)   # stale pointer
containers running: 3810aa6b92fb-…                 # new release
```

CI deploy marked release healthy while local `ship status` compared against old desired state.

**Recommended fixes:**

1. After deploy, `ship status` should read **remote** `/var/lib/ship/current` as source of truth when local `.ship/` is stale
2. Rolling deploy should stop labeling old containers as "desired" once new ones are healthy
3. Document that CI runners have **ephemeral** local state — only remote host state matters

---

### P2 — Accessory redeploy invalidates cached Redis connections

**Symptom:** After `ship accessory deploy npxray-redis`, web containers still connected to old IP `172.19.0.3`; new Redis was `172.19.0.5`.

**Recommended:** When an accessory is redeployed, Ship should **restart dependent services** on the same network (or document this as a required manual step).

---

## What is not a Ship bug

| Issue | Owner | Resolution |
| --- | --- | --- |
| API timeout test flake in CI | `npxray/api` | Race timeout in `dispatcher.ts` |
| Staging `ship.yml` incomplete overrides | `npxray/api` | Full service blocks added (workaround until Ship deep-merge is released) |
| Production still on Kamal | Ops | `ship agent install production` + cutover not done |
| Local deploy secrets missing | Ops | Only `RESEND_API_KEY` in env; CI injects full set |

---

## Commits on `main` to include in next release

These were on `main` but **not in v0.4.0** at the time of the incident:

| Commit | What it fixes |
| --- | --- |
| `ac557d1` | Caddyfile health block, `caddy validate`, co-located docker upstream, `waitForCaddyContainer` |
| `155d3a0` | Deep-merge env service overrides |
| `1af471b` | Env override merge semantics |
| `de46d1c` | Platform-aware agent binary resolution |
| `bf9c0fa` | Atomic state writes (hosts.json, ingress, secrets) |

**Suggested release: v0.4.1** (or v0.5.0 if signaling an ingress-breaking fix).

---

## Diagnostic playbook

```bash
# 1. Config validity
cd api && ship doctor staging && ship plan staging

# 2. Remote state
ssh root@npxray-staging "cat /var/lib/ship/current"
ssh root@npxray-staging "cat /var/lib/ship/ingress/staging.Caddyfile"
ssh root@npxray-staging "docker ps -a --format '{{.Names}} {{.Status}}'"

# 3. Ingress
ssh root@npxray-staging "docker logs --tail 20 ship_npxray-api_staging_caddy"
caddy validate --config /path/to/Caddyfile --adapter caddyfile

# 4. App health (bypass ingress)
ssh root@npxray-staging "curl -fsS http://127.0.0.1:4185/v1/health"

# 5. Accessories
ship accessory status staging npxray-redis

# 6. End-to-end
curl -fsS https://api-staging.npxray.dev/v1/health

# 7. Agent version drift
ssh root@npxray-staging "/usr/local/bin/ship version"
```

**Red flags:**

- Caddy `Restarting (1)` + `Unexpected next token after '{'` → pre-`ac557d1` Caddyfile generator
- `accessory … missing` → need `ship accessory deploy` (deploy doesn't do it today)
- `ECONNREFUSED …6379` after redis redeploy → restart web/worker
- `reverse_proxy npxray-staging:4185` in Caddyfile → pre-`ac557d1` upstream naming
- Caddy logs `no upstreams available` + health checker hitting `127.0.1.1:4185` → wrong upstream hostname

---

## Action items

### Release blockers (before next npxray deploy)

- [ ] Tag and release **v0.4.1** with `ac557d1` + deep-merge commits
- [ ] Publish `linux/amd64` (+ `arm64`) release tarballs for agent upgrade / CI
- [ ] Extend `TestGeneratedCaddyfileValidatesWithCaddyBinary` to assert health block is multiline

### Next sprint

- [ ] Auto-ensure accessories during `ship deploy` (or fail deploy if missing)
- [ ] Restart dependent services when accessory container is recreated
- [ ] Pin consumer CI to explicit Ship version (`go install …@v0.4.1`) not floating `@latest`
- [ ] Improve `ship status` when local `.ship/` state lags remote host

### Docs

- [ ] Clarify in `docs/deploy-and-operate.md`: accessories are **not** deployed by `ship deploy` today
- [ ] Document co-located ingress upstream naming (`web:port` vs SSH hostname)
- [ ] Document `ship agent upgrade` requirements from non-checkout installs

---

## Resolution state (end of incident)

| Check | Status |
| --- | --- |
| `https://npxray.dev` | Up (Vercel) |
| `https://api.npxray.dev/v1/health` | Up (Kamal/production) |
| `https://api-staging.npxray.dev/v1/health` | Up after manual Caddyfile + Redis + container restarts |
| Staging agent version | `0.4.0` (needs upgrade after release) |

Manual Caddyfile on staging (interim fix until Ship v0.4.1+ redeploy):

```caddyfile
api-staging.npxray.dev {
  encode zstd gzip
  handle /_ship/health {
    respond "ok" 200
  }
  reverse_proxy web:4185 {
    lb_policy round_robin
    lb_try_duration 5s
    fail_duration 30s
    max_fails 1
    unhealthy_status 5xx
    health_uri /v1/health
  }
}
```

---

## Bottom line

The deploy did not fail randomly. **Ship v0.4.0 has a confirmed Caddyfile generation bug** that kills ingress, combined with **accessories not being ensured during deploy** and **unreleased fixes sitting on `main`**. Cutting **v0.4.1**, pinning CI to it, running `ship agent upgrade staging`, and redeploying should make the next npxray staging deploy routine.