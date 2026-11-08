`build.ps1` puts `modavd-proxyd` here (gitignored): `cmd/modavd-proxyd` built for linux/amd64. `internal/guest`
embeds this directory, so both exes carry the guest daemon of the per-app proxy. It goes in uncompressed: a large
compressed blob inside an exe is what antivirus models take for a dropper's payload. A build without `build.ps1`
(`go build`, `go test`) has none: `guest.ProxyDaemon` then returns `ErrNoProxyDaemon`.
