`build.ps1` puts `modavd-proxyd.gz` here (gitignored): `cmd/modavd-proxyd` built for linux/amd64 and gzipped by
`internal/tools/gzipfile`. `internal/guest` embeds this directory, so both exes carry the guest daemon of the
per-app proxy. A build without `build.ps1` (`go build`, `go test`) has none: `guest.ProxyDaemon` then returns
`ErrNoProxyDaemon`.
