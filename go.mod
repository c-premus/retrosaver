module github.com/c-premus/retrosaver

// The language floor, not the toolchain. It exists for a host whose Go is
// older than the toolchain line below and which cannot download one -- a
// locked-down box, or an offline build. Such a host refuses outright if the
// floor is above its own Go; at or below it, the tree builds and tests cleanly
// under GOTOOLCHAIN=local. GOTOOLCHAIN=auto, the default, still fetches the
// toolchain below. Verified against go1.26.0 (2026-09-08).
//
// THE FLOOR IS NO LONGER SET BY OUR CODE. The only post-1.23 feature in the
// tree is still strings.SplitSeq (internal/window), so 1.24 would do -- but Go
// requires a module's directive to be at least its dependencies', and
// golang.org/x/sys keeps raising its own. x/sys owns this number now:
//
//	x/sys v0.44.0  declares go 1.25.0  ->  floor 1.24 -> 1.25.0  (ae86a88)
//	x/sys v0.48.0  declares go 1.26.0  ->  floor 1.25.0 -> 1.26.0
//
// Neither raise was decided by anyone. postUpdateOptions ['gomodTidy'] in
// renovate.json performs them, the first as a side effect of a SECURITY bump.
// renovate.json's rule barring the `golang` depType stops Renovate PROPOSING a
// raise; it cannot stop `go mod tidy` performing one. Refusing would mean
// pinning x/sys below its security fixes, which is the worse trade -- so the
// policy is to accept the raise and make it visible, not to fight it.
//
// The guard is in CI: ci.yaml pins GO_FLOOR and fails if this line disagrees
// with it, which turns the next silent raise into a red build. Deliberately NOT
// a floor-versus-toolchain comparison -- that stays green through exactly this
// failure (1.25.0 <= 1.27.1 held throughout) and catches nothing.
//
// Four files carry this number and must move in one commit: this line, GO_FLOOR
// in BOTH ci.yaml files, docs/development.md's "Code standards", and
// renovate.json's `golang` depType rule.
go 1.26.0

// What CI and the devcontainer actually build with. Bump this freely; bump the
// line above only when a dependency forces it or the code needs a newer feature.
toolchain go1.27.1

require github.com/godbus/dbus/v5 v5.2.2

// golang.org/x/sys is a direct dependency, imported by internal/watch for the
// inotify syscalls. See docs/development.md: the rule is two pure-Go dependencies, not
// one, because the stdlib syscall package is frozen and points callers here.
require golang.org/x/sys v0.48.0
