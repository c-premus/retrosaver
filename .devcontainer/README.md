# Development container

This directory contains the VS Code Dev Container configuration for consistent development environments.

## Scope: what this container can and cannot do

**The container cannot run or integration-test `retrosaver`.** The daemon talks to
`org.gnome.Mutter.IdleMonitor` on the user's *session* D-Bus, needs `$DISPLAY` and a live
XWayland server, drives `wmctrl`/`xdotool`/`unclutter` against real X11 windows, calls
`loginctl lock-session`, and installs a `systemd --user` unit. None of that exists inside a
container, and no amount of socket forwarding makes it a faithful test of a GNOME session.

Use this container for:

- Building: `CGO_ENABLED=0 go build ./...`
- Static analysis: `go vet ./...`, `shellcheck tests/smoke.sh`
- Unit tests: `go test ./...` — `internal/config` and `internal/modules` are covered against
  fixture directories, since those are the parts with no desktop dependency
- Packaging: `nfpm pkg --packager deb`
- Authoring, git, and the Claude/Codex CLIs

Real verification is the manual procedure in the project specification (§8) and must be run
on the host, in an actual GNOME/Wayland session.

## What is a Dev Container?

A development container provides a fully configured development environment that runs in Docker. Everyone on the team gets the same tools, extensions, and settings.

## Configuration

The `devcontainer.json` file defines:

- **Base image** - The Docker image to use
- **Features** - Additional tools to install, such as Go or Node.js
- **Extensions** - VS Code extensions to install automatically
- **Settings** - VS Code settings for the container
- **Post-create commands** - Scripts to run after container creation

## Host networking (critical for OAuth)

This devcontainer uses **host network mode** (`--network=host`), which is essential for OAuth flows in Windows WSL2 environments.

### Why host networking?

```json
"runArgs": ["--network=host"]
```

With host networking, the container shares the host's network stack directly:

| Mode | localhost:3000 in container | localhost:3000 on host |
|------|----------------------------|------------------------|
| **Bridge (default)** | Container only | Requires port forwarding |
| **Host** | Same as host | Same as container |

**Benefits:**
- OAuth callbacks work correctly (redirect to `localhost:3000` goes to your app)
- No port mapping confusion between container and host
- Services are accessible exactly where you expect them

### The OAuth problem

OAuth flows redirect browsers to callback URLs like `http://localhost:3000/callback`. With default bridge networking:

1. Your app runs on container's `localhost:3000`
2. VS Code forwards container port 3000 to host port 3000
3. OAuth provider redirects browser to `localhost:3000`
4. The forwarded port may not handle the callback correctly
5. OAuth fails with connection errors or timeouts

**Host networking eliminates this problem entirely** - there's no forwarding layer to interfere.

### Disable port auto-forwarding

With host networking, VS Code's port forwarding is unnecessary and can cause conflicts. This devcontainer disables it:

```json
// Disable auto-detection for all ports
"forwardPorts": [],
"otherPortsAttributes": {
    "onAutoForward": "ignore"
},
"portsAttributes": {
    "3000-9999": { "onAutoForward": "ignore" },
    "10000-65535": { "onAutoForward": "ignore" }
}
```

VS Code settings reinforce this:

```json
"settings": {
    "remote.autoForwardPorts": false,
    "remote.restoreForwardedPorts": false,
    "remote.autoForwardPortsSource": "output"
}
```

### Important: clicking links in the terminal

VS Code has **two** port forwarding mechanisms:

1. **Auto-detection** - Respects `onAutoForward: "ignore"` settings
2. **User-initiated** - Clicking `localhost` links in terminal **ignores all settings**

If you click a `localhost:3000` link in the VS Code terminal, it may still attempt forwarding. Instead:

- Copy the URL and paste directly in your browser
- Or use the browser already open from OAuth initiation

### WSL2-specific notes

In WSL2 environments:

- Host networking means the container uses WSL2's network stack
- `localhost` from Windows browser reaches WSL2 (and thus your container)
- No additional Windows firewall rules needed for localhost
- External network access works through WSL2's NAT

## Usage

### VS Code

1. Install the "Dev Containers" extension
2. Open the project folder
3. Click "Reopen in Container" when prompted (or use Command Palette)

### GitHub Codespaces

This configuration works automatically with GitHub Codespaces. Create a codespace from the repository and the environment is ready.

### Command line

```bash
# Build and start the container
devcontainer up --workspace-folder .

# Execute a command in the container
devcontainer exec --workspace-folder . bash
```

## Customization

Edit `devcontainer.json` to:

- Change the base image
- Add development tools
- Configure VS Code extensions
- Set environment variables
- Mount additional volumes

## Documentation

- [VS Code Dev Containers](https://code.visualstudio.com/docs/devcontainers/containers)
- [Dev Container Specification](https://containers.dev/)
- [GitHub Codespaces](https://github.com/features/codespaces)
