# sing-anytls compatibility fork

This directory is based on `github.com/anytls/sing-anytls` v0.0.13
(upstream commit `479cb5bd490a2f4b1b6e8cd82b821afb392a94c8`). It is kept as a
small local compatibility fork because the upstream stream implementation
accessed `Stream.dieErr` concurrently while an authentication failure closed a
stream. That produced a real `go test -race` report in the node's required
wrong-password and deleted-user tests.

The local change protects `dieErr` reads and writes with an `RWMutex`. No
protocol or wire-format behavior is changed. Revisit the replacement when the
upstream project ships an equivalent race fix.
