# qwen-box

A rootless Linux launcher for Qwen Code.
It runs rootless Podman with an explicitly assembled root filesystem and no
container image. Programs and libraries come from read-only host mounts. Podman and pasta
own process isolation, mounts, and networking; the launcher installs a destination
policy before executing Qwen.

## Arch Linux package

The planned AUR package is `qwen-box-git`. Its recipe is in
[`packaging/aur`](packaging/aur/README.md); it can be submitted after the GitHub
repository is public. It is not published to the AUR yet.

The package builds from source with Go 1.27.3 or later and installs the launcher
at `/usr/bin/qwen-box`. Pacman manages its sandbox dependencies, including
Node.js 22 or later and npm. Qwen Code is installed by npm at runtime inside the
sandbox; no existing Qwen installation or nvm setup is required. Run as your
ordinary user with working rootless Podman and pasta networking. A single UID/GID
mapping is sufficient for this one-user rootfs; subordinate ranges remain useful
for normal multi-user container images.

## Run in a Git repository

```sh
cd /path/to/project
qwen-box isolation-check
qwen-box
```

The nearest `.git` directory or file selects the repository subtree. The project
is writable, with its root `.git` directory or file mounted read-only to protect
repository metadata. This also covers the `.git` files used by worktrees and
submodules; nested repositories select their own root. Metadata referenced by a
`.git` file outside the selected repository is not automatically exposed.
Marker symlinks are rejected. Searching stops before `/`. Starting from a
repository subdirectory preserves the same resolved absolute path inside the
sandbox. Directories outside Git repositories are rejected; no custom approval
marker is needed.

## Usage and configuration

* `qwen-box [options] [qwen] [-- arguments...]` forwards arguments after `--` to Qwen.
* `qwen-box isolation-check` tests filesystem, privileges, and sample network connections.
* `qwen-box help` prints help without starting the sandbox.
* `--qwen-version VERSION` selects a published npm version or dist-tag; put it before `--`.
* `--localnet host:port,...` explicitly allows local TCP and UDP endpoints; omit it for no local exceptions.

Launcher flags may appear before or after the optional mode, but must precede
`--`. The launcher recognizes `--qwen-version`, `--localnet`, and `--help`/`-h`. Model and
endpoint flags belong to Qwen and must follow `--`. Unknown launcher flags and
extra positional arguments before `--` are rejected. Arguments after `--` are
forwarded unchanged and cannot select a launcher mode or change the endpoint
used to configure the firewall.

```sh
qwen-box -- --prompt "Review this repository"
qwen-box -- --help  # Qwen help
qwen-box --help     # launcher help
```

### Qwen version and installation

The default version is `latest`. Select a specific release or npm dist-tag at startup:

```sh
qwen-box                              # latest
qwen-box --qwen-version 0.26.1         # specific release
qwen-box --qwen-version latest qwen -- --model your-model
qwen-box --qwen-version 0.26.1 isolation-check
```

The launcher runs `npm exec --package=@qwen-code/qwen-code@VERSION -- qwen`
after installing the firewall. npm resolves tags against the registry at startup
and installs missing packages in the sandbox's `~/.cache/npm`. On the host,
these files live in `~/.cache/qwen-box/npm`. Downloads are shared across
repositories and launches; npm manages the cache. A new `latest` release is
selected on the next launch. Version numbers and dist-tags are accepted; local
paths, Git URLs, and alternate packages are not.

The entire sandbox `~/.cache` is backed by the host's `~/.cache/qwen-box`,
shared across launcher executions and repositories. Your ordinary host cache is
not mounted. XDG variables are neither set nor inherited; tools use their normal
defaults derived from `HOME`, including `~/.cache`. npm uses its `npm`
subdirectory. Go uses `go-build` for build results and `go/pkg/mod` for
downloaded modules, with `GOPATH` set to the cache's `go` subdirectory.
`GOCACHE` and `GOMODCACHE` are neither set nor inherited; Go derives their defaults
from `HOME` and `GOPATH`. Tools that
use `~/.cache` directly also use the isolated cache. Cache paths from the host
environment are not inherited. Existing host cache files are left untouched.

Installation and package scripts run with the sandbox's normal restrictions.
Package discovery uses the empty `/opt/qwen-box-npm` directory as its npm
prefix, preserving the project working directory without using project-local
Qwen packages. Node.js and npm come from the read-only host `/usr` mount. Qwen
arguments are forwarded unchanged after npm's `--` separator.

### Qwen configuration

Configure the model, endpoint, and authentication in your normal host
`~/.qwen/settings.json`. No `OPENAI_*` shell exports are required.

- `HOME` keeps the resolved host home path; the entire host home is not mounted.
- Sandbox `~/.qwen` uses the shared writable host directory
  `~/.local/state/qwen-box` directly, with no extra `qwen/` level.
- Existing host `~/.qwen/settings.json`, `.env`, `QWEN.md`, `commands/`, `skills/`,
  and `rules/` are mounted read-only at the same paths inside sandbox `.qwen`.
  Edit these inputs on the host. Configuration entries must not be symlinks.
- Host `oauth_creds.json` and `mcp-oauth-tokens.json` are copied from `~/.qwen`
  into launcher state with mode 0600 only when the destination files are absent.
  Host credentials must be regular files. Qwen can refresh the copies without
  changing host files; later launches preserve refreshed tokens.
- After authenticating again on the host, remove the relevant credential copy
  directly from `~/.local/state/qwen-box/` to seed it on the next launch.
- Sessions, logs, temporary files, and other Qwen runtime writes persist in
  shared launcher state. Manual host Qwen sessions are not mounted.
- If host configuration is absent, Qwen can create launcher-owned configuration.
  Qwen settings and credentials never grant local network access; use the explicit
  launcher `--localnet` flag for firewall exceptions.
- The host `.qwen` directory must not be a symlink. New launcher state and cache
  directories use mode 0700; existing directory permissions are unchanged.

### Persistent and temporary storage

Mounts below use `host source → sandbox destination`; `~` means the resolved
host home path.

- Selected repository → same absolute path: writable, with the original host
  working directory preserved.
- Repository's root `.git` → same path: read-only overlay on the project mount.
- `~/.local/state/qwen-box/` → `~/.qwen/`: writable state shared across all
  launcher executions and repositories.
- Selected host `~/.qwen/` configuration entries → the same paths inside
  sandbox `.qwen`: read-only overlays on the writable launcher state.
- `~/.cache/qwen-box/` → `~/.cache/`: the entire writable cache directory,
  shared across all launcher executions and repositories.
- `/usr` and selected host name-service/certificate paths → the same paths:
  read-only host programs, libraries, and system configuration.
- `/tmp/qwen-box-run-*/rootfs/` → `/`: a read-only skeleton for one launch.
- Fresh Podman tmpfs mounts → `/tmp` and `/run`: writable temporary storage,
  discarded with the container.
- The launcher removes `/tmp/qwen-box-run-*` on normal exit, errors, or
  handled cancellation. A crash or `SIGKILL` can leave it behind.
- Host `XDG_*` and `TMPDIR` variables do not relocate launcher storage. Ordinary
  host caches and manual Qwen sessions remain separate.
- All home-relative paths above are derived from the resolved host home:
  shared state and cache sources, `.qwen` configuration and mount destinations,
  sandbox `HOME`, XDG paths, npm/Go cache paths, and the generated passwd entry.
  State and cache paths are computed when needed rather than stored separately
  in `Config`.
- Legacy state/cache directories and the previous nested `qwen/` layout are
  left untouched. Only the current paths above are used as mount sources.

### Model configuration example

Configure an Ollama model in the host's `~/.qwen/settings.json`:

```json
{
  "modelProviders": {
    "openai": [{
      "id": "your-model",
      "baseUrl": "http://192.168.1.50:11434/v1",
      "envKey": "OLLAMA_API_KEY"
    }]
  },
  "env": {
    "OLLAMA_API_KEY": "ollama"
  },
  "security": {
    "auth": { "selectedType": "openai" }
  },
  "model": { "name": "your-model" }
}
```

The `ollama` value is a placeholder for a server without authentication. For a
provider requiring a real API key, Qwen can load it from the host's shared
`~/.qwen/.env` file using the variable named by `envKey`.
See [Qwen Code authentication](https://github.com/QwenLM/qwen-code/blob/main/docs/users/configuration/auth.md)
for provider and credential configuration.

Host environment variables are cleared. As an optional alternative, the launcher
forwards `OPENAI_BASE_URL`, `OPENAI_API_KEY`, and `OPENAI_MODEL` when already set,
and uses the host `TERM`.

### Network access

Public IPv4 and IPv6 egress is allowed. A fixed blacklist blocks private and
special-use address ranges in both families, including loopback, link-local,
CGNAT, multicast, and reserved/documentation ranges. Host interfaces and routes
are not inspected: public addresses remain allowed even when assigned to the
host or used on a directly connected network. nftables interval sets use
`auto-merge` to handle overlapping and adjacent ranges; the launcher does not
sort or collapse blacklist prefixes. DNS
uses `1.1.1.1` and `2606:4700:4700::1111`. Public IPv6 connectivity depends on
the host's IPv6 routes; the launcher does not disable either address family.

Local access is controlled only by the launcher's explicit `--localnet` flag:

- Use comma-separated `IP:port` or `dns:port` endpoints, with brackets around IPv6.
  Each entry allows both TCP and UDP on the specified port.
- For the Ollama configuration above, run
  `qwen-box --localnet=192.168.1.50:11434`.
- For multiple servers, use
  `qwen-box --localnet=192.168.1.50:11434,[fd12::50]:8000 -- --prompt "Review this"`.
- For hostnames, use `qwen-box --localnet=ollama.lan:11434`.
- Entries must include a numeric nonzero port. URLs, CIDRs, scoped IPv6
  addresses, and empty entries are rejected. `--localnet=` explicitly allows none.
- DNS entries are resolved on the host before constructing the whitelist or
  creating the pod. All returned IPv4 and IPv6 addresses are allowed on the
  specified port for both TCP and UDP. Lookup failure aborts startup without a partial whitelist.
- Only the explicitly listed or resolved IP addresses and ports bypass blocks. Other
  ports remain blocked. Repeating the flag replaces the previous list.
- Qwen configuration, credentials, forwarded environment variables, and arguments
  after `--` cannot grant exceptions. The launcher does not parse model settings.
- Qwen resolves DNS names independently using the sandbox's public DNS servers;
  resolution is assumed to remain stable after startup.
- Exceptions are fixed at startup. Relaunch to pick up DNS changes or a new list.
- Loopback and link-local addresses are not translated into host access. The
  virtual subnets are `10.0.2.0/24` and `fd00:2::/64`; guest addresses differ from
  host addresses so services on the host are not shadowed by the guest.

The well-known NAT64 prefix follows the equivalent IPv4 destination blocks.
IPv6 control traffic needed for neighbor/router discovery and path-MTU errors
is allowed without granting TCP/UDP access to blocked destinations.

## Code layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI entry point, launcher flags, command validation, help, signals, and exit status |
| `config.go` | Git repository discovery, tool discovery, state paths, environment |
| `network.go` | Explicit localnet parsing and DNS resolution |
| `tools.go` | Host tool commands, npm version selection, shared configuration inputs, and credential seeding |
| `policy.go` | nftables destination policy and explicit localnet TCP/UDP exceptions |
| `sandbox.go` | Minimal rootfs, explicit host mounts, and Podman command arguments |
| `supervisor.go` | Podman setup, workload execution, cancellation, and container cleanup |
| `check.go` | Native Go isolation diagnostics executed inside the sandbox |
| `packaging/aur/` | AUR recipe, metadata, and publishing workflow |

The launcher registers version, localnet, and help flags with `pflag`, selects
the optional mode from launcher positional arguments, and passes only arguments
after `--` to Qwen without inspecting them.
The launcher is mounted read-only inside the sandbox only for `isolation-check`.
There is no custom namespace/TAP helper or embedded network stack.

## Isolation boundary

The launcher runs a rootless Podman container with `--rootfs` pointing to a
fresh mount skeleton. It contains directory mount points, symlinks, and generated
`/etc` files, with no built or downloaded image or OS installation. The launcher
owns this directory and its cleanup; Podman uses it as an externally managed
root filesystem. The host's `/` is not mounted as the container root. Host
`/usr`, including Node.js and npm, is mounted read-only; the project, shared
launcher cache, and shared launcher Qwen state remain writable. Podman supplies
private PID/IPC/UTS/network namespaces, an init process, a read-only root
filesystem, and its standard seccomp restrictions.
All workload capabilities are dropped and privilege gains are disabled.

The launcher generates `/etc/hosts` with localhost entries,
`/etc/resolv.conf` with its fixed public DNS servers, `/etc/hostname` with
`qwen-box`, and minimal `/etc/passwd` and `/etc/group` entries for `qwen`.
The passwd entry and `HOME` use the resolved host home path. `/bin`, `/sbin`,
`/lib`, and `/lib64` are symlinks into the read-only `/usr` mount. Selected host
name-service files and certificate directories are also mounted read-only.

Podman mounts fresh writable tmpfs filesystems over `/tmp` and `/run` with
`nosuid,nodev`. These hide the corresponding skeleton directories and do not
expose the host's temporary files. The root filesystem is read-only, so directory
permissions alone would not make those paths writable. These tmpfs filesystems
are separate from the launcher's `/tmp/qwen-box-run-*` directory and
disappear when the container is removed.

Initially only a trusted `sleep` process runs. A trusted `podman exec --privileged`
invocation loads nftables rules inside the container's network namespace, then
Qwen is started by an ordinary `podman exec` with no capabilities. This setup
privilege belongs to the rootless namespace and never requires host root. Failure
to install the policy aborts startup. The policy permits public IPv4/IPv6 and exact
explicit `--localnet` TCP/UDP exceptions; the fixed non-public ranges remain
blocked in both address families. Public host/LAN addresses are not blocked.
No host firewall rules are changed.

The launcher attempts to remove its named container after the workload exits,
fails, or is canceled. Podman owns the pasta/runtime processes and namespace
teardown. Abrupt termination such as `SIGKILL` bypasses the launcher's cleanup.

The project, shared launcher cache, and shared launcher Qwen state are writable. The
project's root `.git` directory or file, selected host Qwen configuration,
`/usr`, and selected system configuration are read-only. Temporary mounts are ephemeral.
Qwen can delete project files and transmit data to allowed internet destinations.
Allowed internet services can relay traffic elsewhere. Podman applies its
standard seccomp profile; the launcher sets no resource quotas. Remote MCP
services have their own scope.

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover policy, project selection, environment filtering, credential seeding,
sandbox plans, and Podman startup/failure/cancellation handling. The
`isolation-check` mode checks actual filesystem/network isolation, writable
project/cache/Qwen state, and Node.js and selected Qwen version execution on a
configured Linux host.

A live integration test creates and removes a rootless container using local test
servers, checks filesystem/privilege isolation, and verifies explicit
IPv4/IPv6 localnet TCP/UDP-port exceptions. It also verifies the preserved
working directory, read-only Git metadata and host settings, writable session
state, independent credential replacement, and persistent isolated caches. It
uses a test executable inside the container and does not download Qwen, build an
image, or exercise a complete Qwen conversation. Test-only IPv4/IPv6 mappings
reach host-loopback servers through private addresses, avoiding dependence on
the host's LAN addressing or a global IPv6 route:

```sh
POMPON_PODMAN_INTEGRATION=1 go test -run '^TestPodmanIntegration$' -v .
```
