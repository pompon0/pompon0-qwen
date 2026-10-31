# AUR packaging

`PKGBUILD` builds `qwen-box-git` from the default branch of
`https://github.com/pompon0/pompon0-qwen`. Change `url` if the public repository
uses a different name. This VCS package needs no release tags: `pkgver()` uses
the upstream commit count and abbreviated commit ID.

The package installs `/usr/bin/qwen-box` and its documentation. Pacman
manages the launcher and its sandbox dependencies, including Node.js 22 or later
and npm. Qwen Code is fetched at runtime by npm inside the sandbox, using
`--qwen-version` (default `latest`) to select a release or dist-tag. npm caches
installations in `~/.cache/qwen-box/npm`, shared only across launcher runs.
The host's `~/.cache/qwen-box` backs the sandbox's entire `~/.cache`, including
Go build and module caches. No existing Qwen installation or nvm runtime is
required. The package does not run npm, initialize Git
repositories, or change user configuration during pacman installation.

Runtime mounting structure:

- Host `~/.local/state/qwen-box/` → sandbox `~/.qwen/`: writable state shared
  across all launcher runs and repositories, without an extra `qwen/` level.
- Selected host `~/.qwen` configuration entries → the same sandbox paths:
  read-only overlays; credential files are seeded into writable launcher copies.
- Host `~/.cache/qwen-box/` → sandbox `~/.cache/`: writable shared caches.
- Selected repository → the same absolute path: writable, except its root
  `.git` is mounted read-only.
- Host `/usr` → sandbox `/usr`: read-only programs and libraries.
- `/tmp/qwen-box-run-*/rootfs/` → sandbox `/`: a temporary skeleton passed
  to Podman with `--rootfs`, without building or downloading an image.
- Fresh tmpfs mounts → sandbox `/tmp` and `/run`: writable temporary files.
- Manual Qwen sessions and ordinary host caches are not mounted. Shared state
  and caches persist; temporary mounts and skeletons are removed after the run.

See the main README for the complete mount layout and cleanup behavior.

Local network exceptions require the launcher's explicit comma-separated
`--localnet host:port,...` flag. Literal IPs and DNS names are accepted; names are
resolved on the host before the whitelist is constructed. Qwen resolves names
independently; resolution is assumed to remain stable after startup.
Each listed port is allowed for both TCP and UDP.
Qwen configuration and forwarded Qwen arguments never grant
exceptions; without the flag, non-public destinations remain blocked.
The blacklist uses fixed private/special-use ranges. The launcher does not
inspect host addresses or routes and does not invoke `ip`; publicly routable
host and LAN addresses follow the normal public-egress policy.

## Before submitting

* Make the GitHub repository public and commit the application source.
* Select an upstream license and add its `LICENSE` file. Replace `unknown` in
  `PKGBUILD` with the matching SPDX identifier. The packaging recipe currently
  records the absence of an upstream license rather than assuming permission.
* Add the maintainer contact address to the `PKGBUILD` header.
* Check that `qwen-box-git` remains available in the AUR and that the
  application is not already in Arch's official repositories.
* Create an AUR account and register an SSH public key.

GitHub hosts the application; the AUR hosts a separate Git repository containing
the packaging files. Uploading to GitHub alone does not register an AUR package.

## Validate and publish

From the application repository, create a separate AUR checkout:

```sh
git -c init.defaultBranch=master clone \
  ssh://aur@aur.archlinux.org/qwen-box-git.git ../qwen-box-aur
cp packaging/aur/PKGBUILD ../qwen-box-aur/
cd ../qwen-box-aur
makepkg --nobuild --nodeps
makepkg --printsrcinfo > .SRCINFO
```

The source-only step fetches the public GitHub repository and replaces the
recipe's bootstrap `r0.g0000000` version with its actual commit-derived version.
It does not install anything. Dependency checks are deferred to the clean chroot.

Build and test the package in a clean Arch chroot using `devtools`:

```sh
extra-x86_64-build
```

The build requires Arch's `go>=2:1.27.3`, including its package epoch. Automatic
toolchain downloads are disabled so the declared build dependency is enforced.
Go caches are kept under `$srcdir`. The package runs unit tests and `go vet`;
it does not start a real sandbox inside the build chroot.
The `prepare()` phase downloads the versions pinned by `go.mod` and `go.sum`
before building.

Inspect the resulting package with `namcap` and confirm its contents with
`bsdtar -tf` before submitting. Regenerate metadata after any recipe changes:

```sh
makepkg --printsrcinfo > .SRCINFO
git add PKGBUILD .SRCINFO
git commit -m "Package qwen-box-git"
git push origin master
```

Only publish packaging files to this AUR checkout, not the Go source tree or
built package archives. Choose a license for the packaging files too; Arch
recommends 0BSD for packaging contributions. Include that license in the AUR
repository separately from the application's license.

After publication, AUR users can install `qwen-box-git` with their preferred
AUR tooling. It will not become available through `pacman -S` alone: that command
uses configured binary repositories, while the AUR distributes build recipes.

## Updates

The VCS source follows the upstream default branch. Users' VCS update tooling
detects upstream commits; don't push AUR commits solely to bump `pkgver`.
When dependencies, build steps, or other packaging details change, update the
recipe, bump `pkgrel` as appropriate, regenerate `.SRCINFO`, and push the changes.
Keep the copy in this repository synchronized with the AUR checkout.

For a future tagged release, add a separate `qwen-box` package using a
specific release archive and its SHA-256 checksum. The `-git` package already
provides and conflicts with that package name.

References:

* [AUR submission guidelines](https://wiki.archlinux.org/title/AUR_submission_guidelines)
* [Go package guidelines](https://wiki.archlinux.org/title/Go_package_guidelines)
* [PKGBUILD manual](https://man.archlinux.org/man/PKGBUILD.5.en)
* [.SRCINFO format](https://wiki.archlinux.org/title/.SRCINFO)
