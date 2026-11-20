# sing-anytls compatibility fork

This directory is based on `github.com/anytls/sing-anytls` v0.0.13
(upstream commit `479cb5bd490a2f4b1b6e8cd82b821afb392a94c8`). It is kept as a
small local compatibility fork because the upstream stream implementation
accessed `Stream.dieErr` concurrently while an authentication failure closed a
stream. That produced a real `go test -race` report in the node's required
wrong-password and deleted-user tests.

The local changes protect `dieErr` reads and writes and protect the service
user map during authentication/update with `RWMutex`. No protocol or
wire-format behavior is changed. Revisit the replacement when the upstream
project ships equivalent race fixes; until then, keep this directory pinned
to the recorded upstream commit and rerun the node's negative-auth and race
tests before upgrading it.

The fork is distributed under the GPLv3-or-later terms in `LICENSE`. The full
fork source is kept in this repository and is selected by the root module's
`replace github.com/anytls/sing-anytls => ./third_party/sing-anytls`; releases
must keep this source and license alongside the binaries. Do not replace it
with an unreviewed prebuilt AnyTLS binary.
