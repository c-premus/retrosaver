module github.com/c-premus/retrosaver

// The language floor, not the toolchain. The only post-1.23 feature in the
// tree is strings.SplitSeq (internal/window), so 1.24 is the true minimum.
//
// This matters on a host whose Go is older than the toolchain line below and
// which cannot download one -- a locked-down box, or an offline build. With
// `go 1.26` here such a host refuses outright; with `go 1.24` it builds and
// tests cleanly under GOTOOLCHAIN=local (verified against go1.24.12).
// GOTOOLCHAIN=auto, the default, still fetches the toolchain below.
go 1.24

// What CI and the devcontainer actually build with. Bump this freely; bump the
// line above only when the code starts using a newer language feature.
toolchain go1.26.7

require github.com/godbus/dbus/v5 v5.2.2

// golang.org/x/sys is a direct dependency, imported by internal/watch for the
// inotify syscalls. See docs/development.md: the rule is two pure-Go dependencies, not
// one, because the stdlib syscall package is frozen and points callers here.
require golang.org/x/sys v0.27.0
