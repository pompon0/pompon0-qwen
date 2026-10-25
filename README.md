# qwen-isolated

A rootless Linux launcher for Qwen Code.
The launcher, trusted setup helper, and isolation checks are implemented in Go.
The executable uses `utils/scope.Scope` for structured concurrency and
`golang.org/x/sys/unix` for Linux system calls.

## Arch Linux package

The planned AUR package is `qwen-isolated-git`. Its recipe is in
[`packaging/aur`](packaging/aur/README.md); it can be submitted after the GitHub
repository is public. It is not published to the AUR yet.

The package builds from source with Go 1.27.3 or later and installs the launcher
at `/usr/bin/qwen-isolated`. Pacman manages its sandbox dependencies. Qwen Code
must already be installed in the Node prefix selected by your active `node`,
including when using nvm. Run as your ordinary user; user namespaces must be enabled.

## Run in a Git repository

```sh
cd /path/to/project
qwen-isolated --check
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
* `qwen-isolated --shell [bash arguments...]` runs a sandboxed shell.
* `qwen-isolated --check` tests filesystem, privileges, and sample network connections.
* `qwen-isolated --isolation-help` prints help without starting the sandbox.

### Qwen configuration

Configure the model, endpoint, and authentication in Qwen Code's own settings.
No `OPENAI_*` shell exports are required. Inside the sandbox, the user settings
file is `/home/qwen/.qwen/settings.json`. On the host, this maps to:

```text
~/.local/state/qwen-bwrap/<SHA-256 of absolute repository path>/home/.qwen/settings.json
```

Each repository has a persistent Qwen home. Your host's ordinary
`~/.qwen/settings.json` is not exposed. Use `qwen-isolated --shell` to access the
sandbox home; there, `~/.qwen/settings.json` refers to the file above.

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
link-local, IPv6, and slirp-subnet endpoints cannot receive an exception.

Automatic exceptions read the sandbox's user settings, not repository-supplied
settings. Put the endpoint directly in `baseUrl`; endpoint variable references
can use the settings `env` field or forwarded environment. `.env` files can still
supply credentials to Qwen but are not read by the launcher's endpoint resolver.
JSON comments in settings are supported. Invalid or ambiguous endpoint settings
abort startup; use `--shell` to repair them. Shell mode grants no model exception.

The exception overrides destination blocks for only the selected address and TCP
port. It is fixed at startup: relaunch after changing endpoints or switching to
a model on a different LAN server. Additional IPv4 prefixes can be blocked with
`QWEN_ISOLATED_DENY='203.0.113.0/24,198.51.100.0/24'`.
Relaunch after LAN/VPN route changes.

## Code layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI entry point, signals, and launch sequence |
| `config.go` | Git repository discovery, tool discovery, state paths, environment |
| `network.go` | Typed IPv4 parsing, host network discovery, prefix normalization |
| `qwen_config.go` | Qwen user settings and selected model endpoint resolution |
| `policy.go` | Destination policy and nftables rendering |
| `sandbox.go` | Invocation modes, mounts, workload, temporary configuration |
| `supervisor.go` | Startup handshake, child processes, timeouts, cleanup |
| `helpers.go` | Private Go helper modes, firewall setup, and workload startup gate |
| `check.go` | Native Go isolation diagnostics executed inside the sandbox |
| `help.txt` | CLI help |
| `packaging/aur/` | AUR recipe, metadata, and publishing workflow |

Help is embedded at build time; installation needs only the executable.
The launcher re-executes itself for trusted setup and mounts itself read-only
inside the sandbox only for `--check`. Bash is used only for `--shell`.
Configuration, policy, sandbox plans, and process supervision are
separate so each can be changed without redesigning the rest of the launcher.

## Isolation boundary

Trusted setup installs nftables inside a private user/network namespace before
attaching host-side slirp4netns. Bubblewrap starts the workload in a child user
namespace, with all capabilities dropped and further user namespaces disabled.
Qwen shares the filtered network but cannot change its firewall. Setup failures
abort startup; losing slirp terminates the sandbox. No host firewall changes or
host root privileges are required.

The project and sandbox home are writable. `/usr`, the Node installation, and
selected system configuration are read-only. Temporary mounts are ephemeral.
Qwen can delete project files and transmit data to allowed internet destinations.
This is destination filtering, not protection against internet relays, a VM,
a resource quota, or a syscall allowlist. Remote MCP services have their own scope.

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover policy, project selection, environment filtering, sandbox plans,
and process startup/failure handling. For actual kernel/network isolation,
run the installed executable's `--check` mode on a configured Linux host.
