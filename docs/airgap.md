# Airgapped and no-egress installs

Agent installs normally download `ship_<version>_<os>_<arch>.tar.gz` from
GitHub Releases when the operator machine's platform differs from the host's.
When the operator machine (or CI runner) cannot reach GitHub, point Ship at a
local source instead. An explicit override is authoritative: if it is wrong
(missing file, wrong architecture, failed checksum), the command fails loudly
rather than falling back to a network download.

## Option 1: a release mirror directory

Mirror the assets once from a machine with access:

```bash
VERSION=v0.4.2
mkdir -p ship-releases && cd ship-releases
BASE="https://github.com/watzon/ship/releases/download/${VERSION}"
curl -fsSL -O "${BASE}/checksums.txt"
for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
  curl -fsSL -O "${BASE}/ship_${VERSION#v}_${target}.tar.gz"
done
```

Then run any command that places agent binaries with the mirror:

```bash
ship provision apply production --agent-release-dir /path/to/ship-releases
ship agent upgrade production --agent-release-dir /path/to/ship-releases
```

Ship picks the asset matching each host's platform, verifies it against the
mirrored `checksums.txt` (which is required — a mirror without it is
rejected), and confirms the extracted binary's architecture before uploading.

## Option 2: a single prebuilt binary

When every host shares one platform, point directly at a binary or release
tarball built for it:

```bash
ship agent upgrade production --agent-binary /path/to/ship_0.4.2_linux_amd64.tar.gz
```

The binary's architecture is still verified against each host; a mismatch
fails the command.

## Environment variables

`SHIP_AGENT_BINARY` and `SHIP_AGENT_RELEASE_DIR` are the environment
equivalents of the flags (flags win when both are given). Set exactly one —
setting both is an error.

## Notes

- The bytes are pushed to hosts over SSH stdin; hosts never need outbound
  network access for agent installs, with or without these overrides.
- The mirror checksum verification is transfer integrity (corruption,
  truncation, wrong file), not provenance — mirror from a source you trust.
