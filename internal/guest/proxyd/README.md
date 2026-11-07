`build.ps1` puts `modavd-proxyd.gz` here (gitignored): `cmd/modavd-proxyd` built for linux/amd64 and gzipped by
`internal/tools/gzipfile`. `internal/guest` embeds this directory, so both exes carry the guest daemon of the
per-app proxy. A build without `build.ps1` (`go build`, `go test`) has none: `guest.ProxyDaemon` then returns
`ErrNoProxyDaemon`.

ARM64 packages may embed `modavd-proxyd-arm64.gz` from the same `cmd/modavd-proxyd` source built for linux/arm64. Selection follows the guest ABI and verifies the ELF class, endianness and machine before publishing a host copy. A missing ARM64 payload is reported explicitly; the x86_64 payload is never used as fallback. No generated payload is tracked in Git.
