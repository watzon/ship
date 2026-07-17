# Plan 003: Wait for Azure lifecycle operations and report cleanup failures

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/provider/azure/azure.go internal/provider/azure/azure_test.go`
> If either file changed, compare the deletion and HTTP helper excerpts below with the live code. Stop if Azure long-running-operation handling has already been introduced or deletion order changed.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

Azure resource deletion is asynchronous. The current provider treats any 2xx response, including `202 Accepted`, as completion, immediately tries to delete the VM's NIC and public IP, discards both errors, and reports success. Dependent resources can remain allocated and billable while Ship's VM inventory no longer reveals them. The provider must follow Azure's operation URL until terminal success and must return deterministic companion-resource cleanup failures.

## Current state

- `internal/provider/azure/azure.go` contains the complete Azure REST client and provider implementation.
- `internal/provider/azure/azure_test.go` uses an `httptest` fake API; extend it with stateful operation responses rather than live Azure tests.
- Follow the repository's polling convention in `internal/provider/hetzner/hetzner.go:337-369`: bounded context, configurable short interval for tests, terminal status handling, and context-aware timers.

Deletion discards errors (`internal/provider/azure/azure.go:226-239`):

```go
func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("virtual machine name is required")
	}
	if strings.TrimSpace(c.SubscriptionID) == "" || strings.TrimSpace(c.ResourceGroup) == "" {
		return fmt.Errorf("azure subscription_id and resource_group are required")
	}
	name := host.ID
	if err := c.DeleteVirtualMachine(ctx, name); err != nil {
		return err
	}
	_ = c.DeleteNetworkInterface(ctx, networkInterfaceName(name))
	_ = c.DeletePublicIPAddress(ctx, publicIPName(name))
	return nil
}
```

The HTTP helper accepts all 2xx as complete (`internal/provider/azure/azure.go:442-454`):

```go
resp, err := client.Do(req)
if err != nil {
	return err
}
defer resp.Body.Close()
data, _ := io.ReadAll(resp.Body)
if resp.StatusCode < 200 || resp.StatusCode >= 300 {
	return fmt.Errorf("azure %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
}
if out == nil || len(strings.TrimSpace(string(data))) == 0 {
	return nil
}
return json.Unmarshal(data, out)
```

The fake API currently returns Accepted for all deletes and only checks calls (`internal/provider/azure/azure_test.go:323-330`):

```go
case r.Method == http.MethodDelete && strings.Contains(path, "/providers/Microsoft.Compute/virtualMachines/"):
	api.deletes = append(api.deletes, "vm:"+pathBase(path))
	w.WriteHeader(http.StatusAccepted)
case r.Method == http.MethodDelete && strings.Contains(path, "/providers/Microsoft.Network/networkInterfaces/"):
	api.deletes = append(api.deletes, "nic:"+pathBase(path))
	w.WriteHeader(http.StatusAccepted)
```

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Azure tests | `go test ./internal/provider/azure -count=1` | exit 0 |
| Provider suite | `go test ./internal/provider/... -count=1` | exit 0 |
| Format check | `gofmt -l internal/provider/azure/azure.go internal/provider/azure/azure_test.go` | no output |
| Vet | `go vet ./internal/provider/azure` | exit 0 |
| Full race suite | `go test -race ./...` | exit 0 |

## Scope

**In scope**:

- `internal/provider/azure/azure.go`
- `internal/provider/azure/azure_test.go`
- `plans/README.md` for status only

**Out of scope**:

- Other cloud providers.
- Azure authentication, resource naming, creation request bodies, or config schema.
- Retrying arbitrary failed HTTP requests.
- Hiding cleanup failures as warnings.
- Deleting shared virtual networks, subnets, or security groups.
- Live Azure tests or credentials.

## Git workflow

- Branch: `advisor/003-azure-lifecycle-operations`
- Commit message: `fix(azure): wait for resource deletion`
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Add failing lifecycle tests

Extend the fake Azure API so a VM delete returns `202 Accepted` plus an operation URL header. The operation endpoint must return a nonterminal state once and `Succeeded` on the next poll. Reject NIC deletion while the VM operation is nonterminal.

Add tests proving:

- NIC and public-IP deletion do not begin before VM deletion reaches `Succeeded`.
- A `Failed` or `Canceled` operation returns an error containing the resource and operation status.
- Context timeout/cancellation stops polling.
- NIC deletion failure is returned and public-IP cleanup is still attempted.
- Public-IP deletion failure is returned.
- Multiple cleanup failures are reported in deterministic NIC-then-public-IP order.
- A missing companion resource response that Azure represents as 404 is treated idempotently only for delete operations.

Use tiny injected poll intervals; never sleep real seconds in tests.

**Verify**: run the new focused tests before implementation → they fail for the missing wait/error propagation.

### Step 2: Preserve response metadata from Azure requests

Refactor the private HTTP layer so mutating callers can inspect status code, response headers, and body without duplicating auth or error handling. Keep the existing decoded-JSON convenience for synchronous GET/list calls.

Requirements:

- Read and close response bodies exactly once.
- Preserve current redacted error behavior; never include bearer tokens.
- Return enough metadata to locate Azure's operation URL from the standard async-operation or location header used by the actual response.
- Resolve relative operation URLs against the configured API base safely.
- Do not make `do` recursively poll itself.

**Verify**: `go test ./internal/provider/azure -run 'Test.*(Request|Token|List|Create)' -count=1` → exit 0; existing request tests pass.

### Step 3: Implement bounded Azure operation polling

Add client fields for an injectable poll interval and operation timeout, following the Hetzner client pattern. Production defaults must be finite and suitable for VM deletion; tests override them with short durations.

Implement a private `waitOperation` helper that:

1. Uses the caller context plus the default timeout only when the caller has no earlier deadline.
2. GETs the operation URL with the existing bearer authentication.
3. Accepts case-insensitive terminal statuses `Succeeded`, `Failed`, and `Canceled` from Azure's operation JSON.
4. Returns nil only for `Succeeded`.
5. Includes Azure's error code/message in the returned error when present, without leaking credentials.
6. Honors `Retry-After` when valid; otherwise uses the injected interval.
7. Uses a context-aware timer and stops it on cancellation.

If the live API response shape or header differs from the fake assumption, stop and confirm it from Microsoft's Azure Resource Manager long-running-operation documentation before coding around it.

**Verify**: `go test ./internal/provider/azure -run 'Test.*Operation' -count=1` → exit 0.

### Step 4: Wait after each asynchronous delete

Update `DeleteVirtualMachine`, `DeleteNetworkInterface`, and `DeletePublicIPAddress` so each waits when Azure returns an operation URL. A synchronous 200/204 remains complete. A 202 response without any documented operation location must return an explicit error; do not guess completion.

Keep delete operations idempotent: an Azure 404 for a named companion resource means it is already absent, while other non-2xx responses remain errors.

**Verify**: `go test ./internal/provider/azure -count=1` → exit 0.

### Step 5: Aggregate companion cleanup errors

After VM deletion reaches terminal success, attempt NIC cleanup and public-IP cleanup even if the first fails. Return nil only when both are absent/deleted. Combine errors in stable order with resource names so operators can clean up manually.

Do not delete the public IP before the NIC operation reaches terminal success. Preserve dry-run behavior: no HTTP mutation and no polling.

**Verify**: `go test ./internal/provider/azure -run 'Test.*Delete' -count=1` → exit 0; failure tests prove all eligible cleanup attempts run.

### Step 6: Run all gates

Format changed files, run the Azure and provider suites, then run race and vet.

**Verify**: `gofmt -l internal/provider/azure/azure.go internal/provider/azure/azure_test.go` → no output; `go vet ./internal/provider/azure` → exit 0; `go test -race ./...` → exit 0.

## Test plan

Model new tests after `TestListFiltersTagsAndDeleteRemovesCompanionResources` and the existing stateful fake API in `internal/provider/azure/azure_test.go`.

Required cases:

- synchronous deletion success;
- 202 → running → succeeded;
- failed operation with Azure error details;
- canceled operation;
- timeout/cancellation;
- 202 without operation URL;
- NIC failure plus successful public-IP attempt;
- NIC and public-IP failure aggregation;
- 404 companion resource idempotency;
- dry-run performs no request.

No live credentials or network access.

## Done criteria

- [ ] A 202 VM/NIC/public-IP delete is not considered complete until its operation succeeds.
- [ ] Dependent cleanup begins only after the preceding resource is fully deleted.
- [ ] NIC and public-IP failures are both surfaced deterministically.
- [ ] Delete remains idempotent for already-absent companion resources.
- [ ] Polling has finite defaults, injectable test timing, and context cancellation.
- [ ] Tokens never appear in errors or test output.
- [ ] `gofmt -l` prints nothing for changed files.
- [ ] `go vet ./internal/provider/azure` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No source files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Azure deletion responses in current code/tests already use a different completed-operation mechanism.
- Correct polling requires a new external dependency.
- The actual Azure operation response cannot be authenticated through the existing bearer-token path.
- Companion resource names are not guaranteed to be Ship-owned names derived from the VM name.
- Fixing deletion requires changing Azure config schema or resource naming.
- A live-cloud test appears necessary to determine basic HTTP semantics; use official docs and the fake instead.

## Maintenance notes

- Future Azure PUT/PATCH operations should reuse the same long-running-operation helper instead of accepting 202 as completion.
- Reviewers should scrutinize timeout precedence, `Retry-After`, relative operation URLs, and deterministic multi-error output.
- Do not generalize this into a cross-provider polling abstraction; Azure operation semantics are provider-specific.