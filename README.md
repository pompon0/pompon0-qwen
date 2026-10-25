# qwen-isolated

A rootless Linux launcher for Qwen Code.
It instruments rootless Podman using a minimal mount skeleton rather than an OS
image. Programs and libraries come from read-only host mounts. Podman and pasta
own process isolation, mounts, and networking; the launcher installs a destination
policy before executing Qwen.

## Arch Linux package

The planned AUR package is `qwen-isolated-git`. Its recipe is in
[`packaging/aur`](packaging/aur/README.md); it can be submitted after the GitHub
repository is public. It is not published to the AUR yet.

The package builds from source with Go 1.27.3 or later and installs the launcher
at `/usr/bin/qwen-isolated`. Pacman manages its sandbox dependencies. Qwen Code
must already be installed in the Node prefix selected by your active `node`,
including when using nvm. Run as your ordinary user with working rootless Podman
and pasta networking. A single UID/GID mapping is sufficient for this one-user
rootfs; subordinate ranges remain useful for normal multi-user container images.

## Run in a Git repository

```sh
cd /path/to/project
qwen-isolated isolation-check
qwen-isolated
```

The nearest `.git` directory or file selects the entire writable repository
subtree. This includes the `.git` files used by worktrees and submodules; nested
repositories select their own root. Marker symlinks are rejected. Searching
stops before your home directory or `/`. Starting from a repository subdirectory
preserves that location inside `/workspace`. Directories outside Git repositories
are rejected; no custom approval marker is needed.

## Usage and configuration

* `qwen-isolated [arguments...]` forwards arguments to Qwen.
* `qwen-isolated isolation-check` tests filesystem, privileges, and sample network connections.
* `qwen-isolated help` prints help without starting the sandbox.

### Qwen configuration

Configure the model, endpoint, and authentication in Qwen Code's own settings.
No `OPENAI_*` shell exports are required. Inside the sandbox, the user settings
file is `/home/qwen/.qwen/settings.json`. On the host, this maps to:

```text
~/.local/state/qwen-bwrap/<SHA-256 of absolute repository path>/home/.qwen/settings.json
```

Each repository has a persistent Qwen home. Your host's ordinary
`~/.qwen/settings.json` is not exposed. Edit the sandbox user settings at the
host path above.

For example, configure an Ollama model in that settings file:

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
provider requiring a real API key, Qwen can load it from the sandbox home's
`~/.qwen/.env` file using the variable named by `envKey`.
See [Qwen Code authentication](https://github.com/QwenLM/qwen-code/blob/main/docs/users/configuration/auth.md)
for provider and credential configuration.

Host environment variables are cleared. As an optional alternative, the launcher
forwards `OPENAI_BASE_URL`, `OPENAI_API_KEY`, and `OPENAI_MODEL` when already set,
and uses the host `TERM`.

### Network access

Public IPv4 egress is allowed. Host addresses, directly connected networks,
private/special IPv4 ranges, and IPv6 egress are blocked. DNS uses `1.1.1.1`.
The launcher derives an exact TCP firewall exception from the selected model's
`baseUrl` in the sandbox's user settings. The Ollama example above automatically
permits `192.168.1.50:11434`; no separate firewall environment variable is needed.
Selection uses `security.auth.selectedType` and `model.name`, with `model.baseUrl`
to distinguish duplicate model IDs. Model-related CLI overrides and forwarded
environment variables are also considered. Unselected providers grant no access.

Use a literal LAN IPv4 address in `baseUrl`. HTTP and HTTPS default to ports 80
and 443 when no port is specified. Hostnames use normal filtered networking;
the launcher does not resolve them on the host to grant LAN exceptions. Loopback,
link-local, IPv6, and sandbox-subnet endpoints cannot receive an exception.

Automatic exceptions read the sandbox's user settings, not repository-supplied
settings. Put the endpoint directly in `baseUrl`; endpoint variable references
can use the settings `env` field or forwarded environment. `.env` files can still
supply credentials to Qwen but are not read by the launcher's endpoint resolver.
JSON comments in settings are supported. Invalid or ambiguous endpoint settings
abort startup; repair the sandbox user settings at the host path above.

The exception overrides destination blocks for only the selected address and TCP
port. It is fixed at startup: relaunch after changing endpoints or switching to
a model on a different LAN server. Additional IPv4 prefixes can be blocked with
`QWEN_ISOLATED_DENY='203.0.113.0/24,198.51.100.0/24'`.
Relaunch after LAN/VPN route changes.

## Code layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI entry point, signals, and exit status |
| `cli.go` | Cobra commands, argument validation, model overrides, and help |
| `config.go` | Git repository discovery, tool discovery, state paths, environment |
| `network.go` | Host IPv4 addresses, connected routes, and prefix normalization |
| `qwen_config.go` | Qwen user settings and selected model endpoint resolution |
| `policy.go` | nftables destination policy and exact model-server TCP exception |
| `sandbox.go` | Minimal rootfs, explicit host mounts, and Podman command arguments |
| `supervisor.go` | Podman setup, workload execution, cancellation, and container cleanup |
| `check.go` | Native Go isolation diagnostics executed inside the sandbox |
| `packaging/aur/` | AUR recipe, metadata, and publishing workflow |

Commands, argument validation, and help are defined with Cobra; installation
needs only the executable. Qwen endpoint flags are inspected with `pflag` for
firewall configuration.
The optional positional mode must come first and defaults to `qwen` when omitted
or when the first argument starts with `-`. Use an explicit `qwen` mode before a
positional Qwen prompt. The remaining arguments are
forwarded unchanged to Qwen. `--help` and `-h` remain Qwen flags; use
`help` for launcher help.
The launcher is mounted read-only inside the sandbox only for `isolation-check`.
There is no custom namespace/TAP helper or embedded network stack.

## Isolation boundary

The launcher runs a rootless Podman container with `--rootfs` pointing to a
fresh mount skeleton. It contains directory mount points, symlinks, and generated
`/etc` files, with no downloaded image or OS installation. Host `/usr` and the
active Node prefix are mounted read-only; project and per-repository home mounts
remain writable. Podman supplies private PID/IPC/UTS/network namespaces, an init
process, a read-only root filesystem, and its standard seccomp restrictions.
All workload capabilities are dropped and privilege gains are disabled.

Initially only a trusted `sleep` process runs. A trusted `podman exec --privileged`
invocation loads nftables rules inside the container's network namespace, then
Qwen is started by an ordinary `podman exec` with no capabilities. This setup
privilege belongs to the rootless namespace and never requires host root. Failure
to install the policy aborts startup. The policy permits public IPv4 and one exact
model-server TCP exception; private, host, directly connected, extra denied, and
IPv6 destinations remain blocked. No host firewall rules are changed.

The launcher always removes its named container after the workload exits, fails,
or is canceled. Podman owns the pasta/runtime processes and namespace teardown.

The project and sandbox home are writable. `/usr`, the Node installation, and
selected system configuration are read-only. Temporary mounts are ephemeral.
Qwen can delete project files and transmit data to allowed internet destinations.
Allowed internet services can relay traffic elsewhere. Podman applies its
standard seccomp profile; the launcher sets no resource quotas. Remote MCP
services have their own scope.

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover policy, project selection, environment filtering, sandbox plans,
and Podman startup/failure/cancellation handling. For actual kernel/network isolation,
run the installed executable's `isolation-check` mode on a configured Linux host.

A live integration test creates and removes a rootless container using local test
servers, checks filesystem/privilege isolation, and verifies the model TCP-port
exception without downloading an image:

```sh
POMPON_PODMAN_INTEGRATION=1 go test -run '^TestPodmanIntegration$' -v .
```
