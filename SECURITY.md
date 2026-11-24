# Security policy

`survival-drive` downloads files from the internet, extracts archives, copies to raw volumes and
runs two local HTTP servers. Bugs in any of those paths can be security-relevant, even though the
tool is not a network service.

## Supported versions

| Version | Supported |
|---|---|
| `main` and the latest release tag | yes |
| older releases | best effort, fixes land on `main` first |

There is no guaranteed response time and no bug bounty.

## Reporting a vulnerability

**Please do not open a public issue.** Use GitHub's private vulnerability reporting:

1. Go to <https://github.com/threex0/survival-drive/security/advisories/new>.
2. Describe the issue, the affected version or commit, and how to reproduce it.

If private reporting is unavailable, open a minimal public issue asking for a private contact
channel, without disclosing the details.

Please include:

- the version or commit,
- the command or HTTP request that triggers it,
- the impact you believe it has,
- a proof of concept if one exists.

## In scope

- archive extraction that escapes the image directory (zip-slip, symlinks, absolute paths)
- path traversal through catalogue, profile or lock fields
- checksum or signature bypasses: accepting a file that does not match its pinned size or sha256
- command or argument injection from profile/catalogue data
- unsafe handling of HTTP redirects, ranges or mirror responses during `fetch`
- the launcher's local HTTP API (it should bind loopback only and refuse cross-origin writes)
- anything that makes a drive appear intact when it is not

## Out of scope

- vulnerabilities in the downloaded content, models or servers (report those upstream)
- the licences or accuracy of third-party content
- attacks that require the attacker to already control the build machine or the target drive
- denial of service against your own machine by downloading very large files you asked for

## What the tool already assumes

- A lock file is trusted input. Do not build from a lock you did not produce or verify.
- `fetch` verifies size and sha256 before a file enters the cache; `verify` re-reads every byte in
  a full check.
- `burn` targets a volume you name explicitly; `FORMAT_DRIVE.cmd` erases it and asks for the drive
  letter first.
- The launcher binds its dashboard and both servers to `127.0.0.1`.
