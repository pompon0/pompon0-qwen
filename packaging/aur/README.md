# AUR packaging

`PKGBUILD` builds `qwen-isolated-git` from the default branch of
`https://github.com/pompon0/pompon0-qwen`. Change `url` if the public repository
uses a different name. This VCS package needs no release tags: `pkgver()` uses
the upstream commit count and abbreviated commit ID.

The package installs `/usr/bin/qwen-isolated` and its documentation. Pacman
manages the launcher and its sandbox dependencies. Qwen Code remains an external
prerequisite: its CLI must be installed within the Node prefix selected by
`node` on the user's `PATH`. The launcher supports a user-managed nvm runtime;
Arch's `nodejs` is therefore an optional dependency rather than a forced second
Node installation. The package does not run npm, initialize Git repositories, or
change user configuration during installation.

## Before submitting

* Make the GitHub repository public and commit the application source.
* Select an upstream license and add its `LICENSE` file. Replace `unknown` in
  `PKGBUILD` with the matching SPDX identifier. The packaging recipe currently
  records the absence of an upstream license rather than assuming permission.
* Add the maintainer contact address to the `PKGBUILD` header.
* Check that `qwen-isolated-git` remains available in the AUR and that the
  application is not already in Arch's official repositories.
* Create an AUR account and register an SSH public key.

GitHub hosts the application; the AUR hosts a separate Git repository containing
the packaging files. Uploading to GitHub alone does not register an AUR package.

## Validate and publish

From the application repository, create a separate AUR checkout:

```sh
git -c init.defaultBranch=master clone \
  ssh://aur@aur.archlinux.org/qwen-isolated-git.git ../qwen-isolated-aur
cp packaging/aur/PKGBUILD ../qwen-isolated-aur/
cd ../qwen-isolated-aur
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
git commit -m "Package qwen-isolated-git"
git push origin master
```

Only publish packaging files to this AUR checkout, not the Go source tree or
built package archives. Choose a license for the packaging files too; Arch
recommends 0BSD for packaging contributions. Include that license in the AUR
repository separately from the application's license.

After publication, AUR users can install `qwen-isolated-git` with their preferred
AUR tooling. It will not become available through `pacman -S` alone: that command
uses configured binary repositories, while the AUR distributes build recipes.

## Updates

The VCS source follows the upstream default branch. Users' VCS update tooling
detects upstream commits; don't push AUR commits solely to bump `pkgver`.
When dependencies, build steps, or other packaging details change, update the
recipe, bump `pkgrel` as appropriate, regenerate `.SRCINFO`, and push the changes.
Keep the copy in this repository synchronized with the AUR checkout.

For a future tagged release, add a separate `qwen-isolated` package using a
specific release archive and its SHA-256 checksum. The `-git` package already
provides and conflicts with that package name.

References:

* [AUR submission guidelines](https://wiki.archlinux.org/title/AUR_submission_guidelines)
* [Go package guidelines](https://wiki.archlinux.org/title/Go_package_guidelines)
* [PKGBUILD manual](https://man.archlinux.org/man/PKGBUILD.5.en)
* [.SRCINFO format](https://wiki.archlinux.org/title/.SRCINFO)
