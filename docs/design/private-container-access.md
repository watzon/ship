# Private container access implementation checklist

Ship provides local TCP access to accessories and application services through SSH. The first release includes service replica selection. A session connects to one running container and stays tied to that container until the session ends.

The checklist below tracks verified work. The command and protocol contracts define the implemented behavior.

- [x] Add target types, Docker identity validation, and focused target tests.
- [x] Complete the agent stream protocol and its lifecycle tests.
- [x] Add SSH process streaming and pass transport tests with the race detector.
- [x] Complete both public commands and pass CLI tests.
- [x] Complete the disposable integration fixture and operating documentation.
- [x] Pass the full repository checks and report the optional integration result.

Verification completed on 2026-09-08 with Go 1.26.6:

- `./scripts/ci-local.sh` passed module verification, formatting, vet, race tests, vulnerability checks, and CLI build checks.
- `SHIP_PROXY_INTEGRATION=1 go test ./internal/cli -run '^TestProxyIntegration$' -count=1 -timeout 15m -v` passed all nine integration cases. These cover private Postgres and HTTP access, replica selection, concurrent clients, both TCP half-close directions, interruption cleanup, container restart and replacement, and SSH jump-host routing.
- CLI builds passed for Linux and macOS on amd64 and arm64.

The integration test used a disposable Docker fixture. It did not use a live deployment. The minimum Go version was raised to 1.26.6 to resolve standard-library vulnerabilities reported by the repository's verification script.

**The command contract is explicit.**

```sh
ship accessory proxy production postgres 15432:5432
ship proxy production web 18080:8080 --replica 2
ship proxy production web.2 18080:8080
ship --dry-run accessory proxy production postgres 15432:5432
```

| Input or behavior | First-release contract |
| --- | --- |
| `ENV` | Required. Resolve configuration and host facts for this environment. |
| `NAME` | One configured accessory. |
| `SERVICE` | A configured service, `service.N`, or an exact managed container name copied from `ship ps`. |
| `LOCAL:REMOTE` | Required. Two decimal TCP ports in the range 1 through 65535. Reject addresses, ranges, UDP suffixes, and additional mappings. |
| `--replica N` | Service command only. Default to replica 1, consistent with `exec`. An explicit value must be positive. Reject conflicts with `service.N` or an exact container name. |
| Listener | Bind `127.0.0.1` only. One port can accept multiple concurrent connections. |
| Lifetime | Run in the foreground. Ctrl-C closes the listener, connections, and child processes. Intentional interruption exits successfully. |
| Target changes | Stop on container restart, replacement, or network endpoint change. Tell the user to run the command again. |
| `--dry-run` | Validate local inputs and show the requested mapping, known placement, and candidate hosts. Make no SSH calls, open no listener, and write no state. Mark live identity and reachability checks as pending. |
| Output | Show environment, service or accessory, replica when applicable, host, short container ID, release when applicable, and local mapping. Send status and errors to stderr. Dry-run output goes to stdout. |

Do not infer the remote port from `ports`, `publish`, an image name, or Docker `EXPOSE`. A port mapping is explicit even when the image has a conventional port.

The first release does not include a VPN, private DNS, UDP, automatic failover, background sessions, multiple port mappings in one command, arbitrary remote hosts, public local binds, or database credential management. It does not create private communication between servers.

**The existing code provides discovery and authentication.**

| Existing code | Reuse |
| --- | --- |
| `internal/cli/shared.go`: `environmentContext`, `resolvedHostsForEnvironment`, `sshForHost` | Environment configuration, host facts, and SSH authentication settings. |
| `internal/cli/shared.go`: `resolveServiceReplica`, `mergeResolvedReplica` | Existing service and replica syntax. Preserve an exact container-name constraint separately because the current resolver discards the release portion. |
| `internal/deployment/deployment.go`: `InspectObservedOnHosts` | Current container observations across resolved hosts. |
| `internal/cli/accessory_commands.go`: accessory observation and topology checks | Accessory labels, saved placement, and duplicate detection. |
| `internal/deployment/deployment.go`: `DockerNetworkName`, `DockerNetworkDriver` | The expected environment network. |
| `internal/docker/docker.go`: `Inspect` | Container and network metadata. Parse only the fields required for this feature. |
| `internal/agent/rpc.go`: `SupportedMethods` | Discover support without raising the minimum version for unrelated RPC methods. |
| `internal/transport/ssh.go` | OpenSSH authentication, host-key handling, ports, jump hosts, and custom connection options. |

The current SSH command methods buffer output and add a default ten-minute timeout. Add a separate stream method. Keep existing command behavior intact.

The default environment network is a Docker bridge on each server. The host can reach container ports on that bridge without publishing them. Docker documents this in its [bridge network reference](https://docs.docker.com/engine/network/drivers/bridge/).

Ship's existing `ports: [5432]` generates `-p 5432:5432`. It publishes a server port. A private-only database must omit both `ports` and `publish`. The proxy must not change either setting. When the selected port already has a published binding, report that binding and explain that the proxy does not remove it. Do not claim that an existing deployment is private merely because a tunnel is active.

**Select the observed container before opening the listener.**

1. Validate the arguments, environment, configured target, and enabled bridge network.
2. Resolve host facts and collect managed-container observations from the environment hosts. A failed host observation is an error, because discovery is incomplete.
3. Match Ship ownership, project, environment, target kind, and target name. For services, also match the selected replica. Use existing label normalization rules.
4. Confirm running state through Docker inspection. Do not treat a human-readable `docker ps` status as authoritative. Reject paused, restarting, dead, or stopped targets.
5. For a service, require one running match. Do not use scheduler predictions or require a local current-release file. If a rollout has two running releases for the same replica, report both and require an exact container name from `ship ps`. Never choose the newest release by sorting IDs.
6. For an exact service container name, preserve the complete name as a filter. Still verify its labels, service, replica, and running state.
7. For an accessory, retain saved-placement and topology checks when saved placement exists. Refuse a conflicting host or multiple managed copies. When placement is absent, accept only one observed, running, eligible container. Do not create placement state or select the first eligible host.
8. Ask the selected agent to resolve and probe a filtered proxy target. Expand any short observed ID to the full container ID. Use the full ID after this point.
9. Bind the local listener and print the ready message only after the remote probe succeeds. Never retry a failed bind on another port.

Fresh checkouts can therefore use live observations without a local release file. Existing accessory placement remains a constraint when it is available. The command does not repair stale placement or host inventory.

**Use an agent stream to verify the connection before forwarding data.**

```text
Local TCP client
    |
127.0.0.1:LOCAL in ship
    |
OpenSSH command session
    |
ship agent proxy on the selected host
    |
verified TCP socket to the selected container:REMOTE
```

A fixed `ssh -L` destination can outlive the container that owns an IP address. A watcher detects a change only after it occurs. Per-connection inspection followed by `ssh -W` still separates inspection from the remote connection attempt.

The recommended implementation makes the agent open the remote TCP socket. The agent verifies the container immediately before and after that connection. It forwards application data only after both checks pass. This requires an updated agent and a small stream protocol, beyond the initial feasibility sketch.

Pin the full container ID, `StartedAt`, restart count, sandbox ID, network ID, endpoint ID, selected IP address, and expected Ship labels. Inspect the actual network and require the `bridge` driver. Resolve a single literal address on the expected network, preferring IPv4 and accepting IPv6 when IPv4 is absent. Reject missing identity fields and unspecified, multicast, or loopback destinations.

Docker defines the sandbox as the container's network stack and provides identifiers for the network and endpoint. It also reports the last container start time. See the [Docker Engine specification](https://docs.docker.com/reference/api/engine/version/v1.52.yaml).

For each accepted local connection, the agent must:

1. Validate the request and inspect the pinned full container ID.
2. Verify the running container and network identity against the pinned target.
3. Dial the inspected container address and requested port with a deadline. The agent derives the destination from Docker. It does not accept an unchecked address from the client.
4. Inspect the same full ID again and compare all pinned fields.
5. On mismatch or inspection failure, close the socket and return a typed error. Forward zero application bytes in either direction.
6. On success, send a ready response and begin the relay. Preserve any server greeting until this point.

The expected protection covers normal Docker restart, replacement, and network reconnection behavior. It relies on Docker metadata and an established TCP socket remaining connected to its original peer. This is an engineering inference to test, not isolation from a privileged administrator who changes host routing or Docker internals.

An identity check every five seconds gives prompt notice when an idle session loses its target. Give that check a five-second deadline. A changed identity or failed check closes the complete proxy session. The watcher improves notification; the per-connection checks control whether application data can pass.

Do not re-resolve to a new container during a session. A new release or moved accessory requires a new command. A configuration edit alone does not change an active session's pinned target.

**Keep control messages separate from application bytes.**

Add a `proxy_target` RPC for filtered target resolution, optional TCP probing, and validation of an expected target. Return a proxy stream version with the filtered target. The same validation functions serve the RPC and the stream command. Do not return environment variables, mounts, secrets, or raw inspect JSON in the new result.

The preflight probe uses the same pre-dial and post-dial identity checks as a stream. It closes its socket without exchanging application data and returns the verified identity. Each later connection repeats the checks. A successful probe is not a lease on the address.

Use the existing `SupportedMethods` result to check for `proxy_target`. An older agent fails with a clear instruction to run the existing `ship agent upgrade ENV` command. Do not upgrade agents automatically. Keep the current JSON RPC protocol compatible. Give the new stream its own version, initially 1.

Add a hidden `ship agent proxy` command beside `ship agent rpc`. It serves one connection over stdin and stdout. It opens no server listener and acquires no deployment lock. The agent stream uses the existing SSH execution authority and Docker access. It is not a separate limited-access role.

Start with a newline-terminated JSON request and response, each limited to 16 KiB. The request contains the stream version, pinned target, and remote port. The response contains success or a typed error. The client sends no application data before success. Any bytes buffered after the response belong to the following frames and must be preserved.

After success, use a frame header containing a one-byte type and a four-byte unsigned big-endian payload length:

| Frame | Rule |
| --- | --- |
| `DATA` | At most 64 KiB. Copy bytes unchanged. Reject zero-length data frames. |
| `FIN` | Zero payload bytes. Close the TCP write direction and continue the reverse direction. Reject subsequent data in that direction. |
| `ERROR` | At most 4 KiB of a typed, sanitized error. End the connection. |

Reject unknown types, unsupported versions, oversized lengths, truncated frames, duplicate FIN frames, and invalid transitions. Validate lengths before allocation. Keep error output bounded. Serialize frame writes so data and error frames cannot interleave.

Use frames to distinguish a TCP half-close from the loss of the SSH session. Pipe EOF before a completed exchange means cancellation or failure. It closes the remote TCP socket. This avoids reliance on the interaction between SSH stdout, stderr, and channel EOF.

Each local TCP connection gets one SSH command process. Do not add multiplexing, compression, a shared SSH master, or an embedded SSH implementation in this release. Measure the connection setup cost in the integration test and document it as suitable for interactive database and service access.

**Own process and socket cleanup.**

The SSH stream method must expose streaming stdin and stdout, bounded stderr capture, cancellation, and a single process wait. Reuse connection argument construction, but enforce foreground operation and no TTY for these sessions.

Clear inherited local and remote forwards. Disable connection sharing for proxy stream processes so the command owns their lifetime. Preserve identity files, SSH agent use, host-key settings, jump hosts, proxy commands, and other compatible connection options. Detect explicit conflicting stream options and report them instead of silently accepting settings that can corrupt the stream. Keep these overrides out of ordinary SSH commands.

OpenSSH uses the first obtained value for most options. Place mandatory stream settings correctly and avoid duplicate case-insensitive option keys. The [OpenSSH configuration reference](https://man.openbsd.org/ssh_config) defines the precedence and forwarding options.

Use a 30-second aggregate setup deadline for each remote stream, a five-second TCP dial deadline, and a 30-second preflight deadline. Remove setup deadlines after readiness. There is no default idle timeout and no ten-minute session limit. Use SSH server-alive checks to detect a lost transport.

Limit each proxy command to 64 active connections, including connections still in setup. Close excess connections with a bounded diagnostic. Keep memory proportional to the connection limit and fixed frame buffers. Do not queue an unbounded number of connections or subprocesses.

A normal TCP disconnect or connection refusal affects that connection. Identity changes, malformed protocol data, failed ongoing identity checks, or command cancellation close the whole listener and all active connections. A setup timeout closes the failed connection; the identity watcher decides whether the target remains usable. Never silently reconnect an established database session.

Register signal cancellation for these commands. On shutdown, close the listener first, cancel child contexts, close active sockets and pipes, and wait for all workers and SSH children. Bound the shutdown wait and terminate children that do not exit. Do not hold an environment lock for the lifetime of an interactive tunnel.

Apply a 30-second write stall timeout to TCP writes and SSH pipe writes. Reset it when bytes make progress. This timeout applies only while output is pending; an idle connection remains open. Cancellation must close the connection and stream descriptors even when a blocked TCP write prevents the frame reader from observing SSH EOF. Use a separate cancellation path instead of relying on that reader for cleanup. Bound local child shutdown to five seconds, then terminate and reap the child.

Complete both FIN directions before normal stream termination. Test this state machine with real TCP sockets, not only byte buffers.

**Implement the work in verifiable steps.**

| Step | Changes | Proof before the next step |
| --- | --- | --- |
| 1. Target and port contracts | Add focused proxy target types and Docker metadata parsing. Add port parsing and service/accessory selection tests. | Tests reject ambiguous targets, wrong scope, incomplete network identity, and invalid ports. Fixtures cover a fresh checkout and saved accessory placement. |
| 2. Agent validation | Add `proxy_target` and shared pre-dial/post-dial verification. Add the hidden command entry point. | A controlled dial hook changes the container identity at each boundary. No test peer receives application bytes after a mismatch. |
| 3. Stream protocol | Add versioned handshake, bounded framing, and bidirectional relay in focused agent files. | Binary round trips, server greetings, both half-close directions, malformed frames, and cancellation pass. Fuzz the small frame parser. |
| 4. SSH process transport | Add a stream method and shared connection-argument helper in `internal/transport`. | Existing SSH tests pass. New tests prove option precedence, stderr bounds, setup deadlines, and process cleanup. |
| 5. Public commands | Add `internal/cli/proxy_commands.go` and register it in `root.go` and `accessory_commands.go`. Share session control while keeping target selection explicit. | Both commands work with fake agents and real local sockets. Replica syntax, exact names, dry-run, interruption, connection limits, and errors pass. |
| 6. Integration and documentation | Add a disposable SSH/Docker integration fixture. Update command help, operations docs, development docs, and the bundled Ship skill reference where needed. | Private Postgres and the sample HTTP service work through the complete CLI and agent path. The repository's full verification passes. |

Keep target types, protocol helpers, and the agent client in focused `internal/agent/proxy*.go` files. Keep Docker inspection parsing in `internal/docker` if both RPC and stream handlers need it. Keep SSH process handling in `internal/transport`. Do not import CLI or deployment orchestration code into the agent. Use narrow injected functions for Docker inspection, dialing, and process creation rather than a new general plugin framework.

Prefer a read-only shared target selector over copying the complete `exec` or accessory command implementations. Do not refactor existing commands unless a small extraction directly removes duplication required by this feature.

**The release checks must exercise the failure boundaries.**

| Boundary | Required checks |
| --- | --- |
| Target selection | Unknown environment or target, stopped target, wrong project labels, missing host observation, conflicting saved accessory host, duplicate accessories, duplicate service releases, replica default and conflicts, exact-name preservation. |
| Network | Default and custom-named bridges, IPv4 and IPv6 metadata, missing address, disabled network, non-bridge driver, rejected identity fields, existing published binding notice. |
| Identity | Replacement between initial observation and stream start, replacement during dial, restart with the same ID, endpoint change without restart, and reuse of the original IP by another container. |
| Protocol | Arbitrary binary payloads, data larger than a frame, early server greeting, both half-closes, simultaneous close, EOF mid-frame, invalid lengths, version mismatch, and bounded errors. |
| Lifecycle | More than ten minutes represented through controllable timeout tests, cancellation during every setup phase, Ctrl-C after readiness, SSH loss, blocked peer, 64 concurrent connections, excess connection rejection, and no surviving process after shutdown. |
| Compatibility | Older agent, unchanged ordinary SSH behavior, configured identity and known-hosts files, jump host, compatible custom options, cleared inherited forwards, and disabled proxy session sharing. |
| Privacy | No automatic `ports`, `publish`, ingress, firewall, or deployment changes. No raw inspect payload, credentials, or application bytes in diagnostics. Dry-run opens no socket and performs no SSH operation. |

Default tests must remain hermetic. Use fake Docker responses, loopback TCP servers, and test helper processes. A fake SSH executable proves argument construction and process control, but it does not prove OpenSSH channel behavior.

Add an opt-in `SHIP_PROXY_INTEGRATION=1` test that uses a disposable Linux SSH/Docker fixture. Exercise the actual ship CLI, OpenSSH client, and remote agent. Reuse `testdata/sample-app` for HTTP. Use an unpublished Postgres container with a test-only credential. Verify a SQL query and an HTTP response, concurrent clients, and target replacement. Assert that the database and application container ports have no published bindings. Test that an outside container cannot access them directly on an ordinary isolated bridge.

The fixture must use temporary SSH keys and known-hosts files, unique container and network names, bounded startup waits, and cleanup even on failure. It needs no cloud credentials and must never target an existing deployment. Support Docker Desktop by running the SSH/Docker host inside the disposable Linux fixture; do not assume the developer's macOS host can route to Docker Desktop bridge addresses.

Add the integration gate to Linux CI with its explicit opt-in variable. Keep `go test ./...` independent of Docker and external registries. Use a real SSH test for jump-host routing and forced stream settings. Keep the TCP half-close tests independent of Postgres so that they can expose stream errors directly.

Run focused tests during implementation, then run:

```sh
./scripts/ci-local.sh
SHIP_PROXY_INTEGRATION=1 go test ./internal/cli -run TestProxyIntegration -count=1
```

The existing full gate verifies modules and formatting, runs vet and race tests, scans vulnerabilities, and builds the CLI. Also confirm that the feature still builds for the existing Linux and macOS release targets. Do not weaken the gate if a failure occurs.

**The implementation is complete when both public commands pass the real transport test.**

Completion requires a working unpublished Postgres connection, a working selected HTTP service replica, reliable interruption, rejection of changed container identities before application data passes, and readable documentation of the agent upgrade requirement. Report separately any integration coverage that could not run.

The main remaining implementation risks are frame lifecycle correctness, Docker identity behavior during network changes, and disposable integration fixture portability. Resolve those with the focused tests before broad command integration. If the identity tests invalidate the pre-dial/post-dial model, revisit that mechanism before release. Do not replace the checks with a periodic watcher and retain the same guarantee.
