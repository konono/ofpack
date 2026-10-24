# ofpack

Carry a [mise](https://mise.jdx.dev/) toolchain to a disconnected machine.

`ofpack pack` collects active mise tools from your global configuration and
the selected project directory **and its descendants**. It carries each
directory's version selection, the installed binaries, and mise itself in one
`.tar.gz`. `ofpack unpack` restores them without network access.

Built with the Go standard library only.

## Install

```sh
go install github.com/konono/ofpack@latest
ofpack version
```

Make sure the Go binary directory (`$(go env GOPATH)/bin`, or `GOBIN` if set)
is on your `PATH`.

### Use from an AI agent (MCP)

`ofpack mcp serve` exposes the `pack`, `unpack`, and `inspect` tools over local
MCP stdio. Install ofpack v0.2.0 or newer with the
`go install github.com/konono/ofpack@latest` command above. Register the full
binary path so the agent can find it even if its `PATH` differs from your shell:
Check that `"$(command -v ofpack)" help` lists `ofpack mcp serve` first; an
older binary earlier on `PATH` will not provide the server.

```sh
OFPACK_BIN="$(command -v ofpack)"

# Claude Code (user-wide)
claude mcp add --scope user ofpack -- "$OFPACK_BIN" mcp serve

# Codex (user-wide)
codex mcp add ofpack -- "$OFPACK_BIN" mcp serve

# OpenCode v2 (global configuration)
opencode mcp add ofpack --global -- "$OFPACK_BIN" mcp serve

# Gemini CLI (user-wide)
gemini mcp add --scope user ofpack "$OFPACK_BIN" mcp serve
```

OpenCode 1.x uses an interactive installer instead: run `opencode mcp add
ofpack`, choose a local server, and enter the binary path followed by
`mcp serve`. Verify with `claude mcp list`, `codex mcp list`,
`opencode mcp list`, or `gemini mcp list`. See the
[Claude Code](https://code.claude.com/docs/en/mcp),
[Codex](https://developers.openai.com/resources/docs-mcp), and
[OpenCode](https://dev.opencode.ai/v2/docs/cli/commands/) and
[Gemini CLI](https://geminicli.com/docs/tools/mcp-server/) MCP guides for the
client-specific options. Cross-platform `pack` may take much longer than
OpenCode's default MCP timeout; set a longer `mcp.servers.ofpack.timeout` in
OpenCode v2's configuration when using it.

The MCP tools take absolute paths: `pack` needs `project_dir` (and optionally
`output_archive`), while `unpack` and `inspect` need `archive`. `pack` may
download tools and start Podman for a cross-platform build. `unpack` only
extracts files; it never installs RPM or APT packages or restores a Python
`.venv`. Use the explicit CLI commands for those later steps.

## Use

Packing is directory based, like mise itself: run it in the project whose
toolchain you want.

```sh
# on the connected machine
cd ~/work/myapp
ofpack pack -o myapp.ofpack.tar.gz
```

```
Packing the mise tools active in /Users/you/work/myapp
  bun                          1.3.14
  jq                           1.8.1
  node                         22.20.0
  mise 2026.9.10 (darwin/arm64), 4821 files, 612 dirs, 143 symlinks, 284.1 MiB
Wrote /Users/you/work/myapp/myapp.ofpack.tar.gz (96.4 MiB)
```

Copy the archive across. That is the only file you need: an `ofpack` for the
target platform travels inside it as `bin/ofpack`, so the receiving machine
needs nothing but `tar` to get started.

```sh
scp myapp.ofpack.tar.gz user@host:~/
```

On the disconnected machine, from the home directory you copied it into:

```sh
mkdir -p /tmp/ofpack-boot
tar -xzf myapp.ofpack.tar.gz -C /tmp/ofpack-boot bin/ofpack   # just that one member
/tmp/ofpack-boot/bin/ofpack unpack -f myapp.ofpack.tar.gz     # lands in ~/work/myapp/.ofpack
source ~/work/myapp/.ofpack/activate.sh
mise ls --current        # the same tools, same versions
```

Extracting into a temporary directory keeps the bootstrap from overwriting a
`bin/ofpack` of your own in the current directory. The same binary is installed
at `~/work/myapp/.ofpack/bin/ofpack` for later use, so the temporary copy can be
deleted. `--no-unpacker` leaves it out of the archive if you would rather
supply your own.

### Where things land

With no `--dest`, ofpack reproduces the source project's path *relative to the
home directory* under your home, and installs the toolchain in that project's
`.ofpack` subdirectory:

| packed on the source machine | unpacked on the target machine |
| --- | --- |
| `/Users/alice/work/myapp` | `/home/bob/work/myapp/.ofpack` |

That absorbs both a different user name and a different home prefix
(`/Users` vs `/home`). Only the relative part (`work/myapp`) is stored in the
archive; the source absolute path and user name are not recorded.

If the project was packed from a directory outside the home directory, such as
`/work/myapp`, there is nothing sensible to reproduce and `--dest` is
required:

```sh
./ofpack unpack -f myapp.ofpack.tar.gz --dest /work/myapp/.ofpack
```

Pass `--dest DIR` whenever you want to choose the location yourself.

The destination directory *is* `MISE_DATA_DIR`:

```
~/work/myapp/.ofpack/
├── activate.sh            sourceable shell setup (manual use)
├── direnv.envrc           sourced from a project .envrc (direnv use)
├── bin/mise               the mise executable
├── config/config.toml     generated global config, exact versions
├── installs/...           the tool trees
└── ofpack-manifest.json    what this tree came from
```

`activate.sh` and `ofpack-manifest.json` are generated by unpack, but they are
installed through the same conflict check and transaction as everything else,
so they never overwrite an existing file or follow a symlink out of the tree.

An archive contains the **mise environment only** — never your project's source
files — and unpack writes nothing outside the destination directory.

### Activating

`activate.sh` is meant to be sourced, not executed:

```sh
source ~/work/myapp/.ofpack/activate.sh   # bash, zsh
. ~/work/myapp/.ofpack/activate.sh        # sh, dash
```

It exports `MISE_DATA_DIR` and `MISE_CONFIG_DIR` (which must be set *before*
mise starts, so they cannot be passed on the mise command line), sets
`MISE_OFFLINE=1` so mise never reaches for the network, and puts the packed
root selection on `PATH` immediately. It does not install a dynamic mise hook:
that hook would read the current directory's original `mise.toml` and could
replace the packed selection. For automatic directory switching, use direnv.

`ofpack env --dest DIR` prints the same snippet for an already unpacked tree.

### Automatic switching with direnv

Sourcing by hand is fine for one project. With [direnv](https://direnv.net/)
the toolchain can instead appear when you enter the directory and disappear
when you leave, which is what makes several unpacked projects usable side by
side. direnv is an **optional prerequisite** — ofpack does not bundle it, and
everything above works without it.

Set up direnv's hook once, in your shell's rc file:

```sh
eval "$(direnv hook zsh)"     # ~/.zshrc
eval "$(direnv hook bash)"    # ~/.bashrc
```

fish uses its own syntax, in `~/.config/fish/config.fish`:

```fish
direnv hook fish | source
```

Then, per project:

```sh
cd ~/work/myapp
ofpack direnv init
direnv allow
```

`direnv init` checks the environment first and only writes if it matches the
archive (see below); then it writes a one-line `.envrc` pointing at the
unpacked tree:

```sh
source_env '.ofpack/direnv.envrc'
```

For a child directory with its own mise configuration, run `ofpack direnv init`
there too. It finds the root project's `.ofpack`, then points that child's
`.envrc` at its own exact-version selection. A directory without a separate
mise config inherits the nearest selected scope. For example, if the root has
Go 1.25 and `app/mise.toml` selects Go 1.26, the archive contains both
versions; `cd root` selects 1.25 and `cd root/app` selects 1.26 after each
directory has been initialized with direnv. The root `activate.sh` selects
the root snapshot for manual use; it does not switch child scopes on `cd`.

If the project **already has an `.envrc`, ofpack does not touch it** — it
prints the line for you to add where you want it. A destination outside the
project is referenced by absolute path, quoted, so paths with spaces work;
one inside the project is relative. The generated `.ofpack/direnv.envrc`
derives its own location when direnv sources it rather than baking in the
path it was written at, so moving or cloning the project keeps working.

Now two projects can pin different versions and you just `cd`:

```console
$ cd ~/work/projA && go version
go version go1.25.6 darwin/arm64
$ cd ~/work/projB && go version
go version go1.26.4 darwin/arm64
$ cd ~ && go version
go version go1.26.4 darwin/arm64      # whatever your machine had
```

`GOROOT` and the rest of the tool environment follow along, and leaving the
directory restores exactly what you had before.

**Do not also run `mise activate` in the same shell.** Its hook would read
the original project configuration and can replace the packed versions.
direnv manages entering and leaving a directory itself; its generated
`.ofpack/direnv.envrc` installs no mise hook. It uses its own script because
`activate.sh` is for manual, persistent activation.

#### What the direnv environment is, exactly

It is the **snapshot**, not a live mise setup. The generated file clears every
inherited `MISE_*` variable, pins `MISE_DATA_DIR`, `MISE_CONFIG_DIR`,
`MISE_GLOBAL_CONFIG_FILE` and `MISE_OFFLINE` to the unpacked tree, and
evaluates mise with `-C /`.

Each of those is load-bearing. An inherited `MISE_GLOBAL_CONFIG_FILE`
overrides `MISE_CONFIG_DIR` outright — measured, not assumed — so a shell that
already had one would otherwise load a different configuration entirely.
`-C /` stops mise walking up from the project and reading the project's own
`mise.toml` and its parents, which describe the machine the archive came from
rather than what is unpacked here.

**The limit:** config that mise reads regardless of those settings —
`/etc/mise/config.toml` and a `/mise.toml` at the filesystem root — is not
shut out. `MISE_CEILING_PATHS` did not prevent it in testing. Rather than
claim an isolation that cannot be guaranteed, `ofpack direnv init` *measures*
it: it runs the bundled mise exactly as the generated file does and compares
the result against the manifest, reporting anything extra or missing, and
naming those system config files when they exist.

The check runs both commands the generated file depends on — the tool listing
*and* the environment itself — because a mise that lists tools happily but
cannot build an environment would otherwise pass. It runs **before** anything
is written. If the environment does not
match, or cannot be verified, `direnv init` fails and creates no `.envrc` —
an `.envrc` and an invitation to run `direnv allow` would otherwise lend
approval to the wrong toolchain. A tool that mise lists but has not installed
counts as missing, since it is not usable. A clean check is silent.

The generated files refuse in the same way at load time: if mise cannot
produce an environment, `.ofpack/direnv.envrc` ends the evaluation so direnv
applies nothing at all (rather than exporting what it had set so far and
reporting success), and `activate.sh` returns non-zero without touching your
shell further.

A consequence worth knowing: editing the project's `mise.toml` afterwards does
**not** change what direnv loads. It cannot — the tools it names are not in
the archive. To go back to a live mise setup, remove the `.envrc` line and use
`mise activate` as usual.

### Other commands

```sh
ofpack direnv init                     # hook this project up to direnv
ofpack inspect -f myapp.ofpack.tar.gz   # tools, platform, symlink notes
ofpack pack --target linux/amd64       # pack for another OS (see below)
ofpack pack --no-unpacker              # leave bin/ofpack out of the archive
ofpack pack --dry-run                  # what would be packed
ofpack unpack -f a.tar.gz --dry-run    # what would be installed, and where
ofpack pack -C ~/work/myapp            # pack another directory without cd
```

## What is in an archive

| included | excluded |
| --- | --- |
| the install directory of every tool active in the selected root or a descendant scope | tools installed but inactive in every selected scope |
| `installs/.mise` (mise's install catalog, not a cache) | `downloads/`, `http-tarballs/`, `cache/`, `shims/` |
| the mise binary, with symlinks resolved to the real file | anything outside the mise installs root |
| an ofpack for the target, as `bin/ofpack` | — |
| a generated `config.toml` pinning exact versions | your project's source files |
| the names of env vars your mise config sets | **their values** — see below |

The manifest records no absolute source paths: the project location is stored
relative to your home directory, config sources and symlink targets as `~/...`,
and anything outside your home is omitted rather than written down. Ownership
is not recorded either.

`mise ls --current --json` decides what is active in each scope. The selected
root (`cwd` or `-C`) is a boundary: global configuration applies, but local
mise files above that root are excluded. Its descendants are included. Two
different versions of one tool are both stored under
`installs/<tool>/<version>`; generated configs pin the exact selection for
the root and each child scope. `pyproject.toml` has no effect unless
`--uv-project` is specified.

For `ofpack pack -C P`, a global mise config (G) and `P/mise.toml` apply at
`P`. `P/app/mise.toml` adds or overrides selections for `P/app` and below.
A `mise.toml` in the directory above `P` is ignored. If both scopes select
different Go versions, both are packed; the root and child configs select
their own version after unpacking.

The mise binary is dereferenced before archiving: a Homebrew `mise` is a symlink
into the Cellar, and archiving the link would produce a dangling file.

### Secrets

mise config `[env]` entries can hold secrets, so **ofpack never records their
values**. Only the variable names go into the manifest, and unpack reminds you
which ones the source configuration set so you can provide them yourself:

```
The source configuration also set these env vars, which ofpack does not carry
(their values may be secrets) - set them yourself if the tools need them:
  DATABASE_URL, GITHUB_TOKEN
```

## Guarantees

### An archive only unpacks on the platform it was built for

Tool installs are native binaries. An archive records the OS and architecture
it was built on, and unpack refuses to run on anything else:

```
ofpack: archive was packed on darwin/arm64 but this machine is linux/amd64;
mise tools are native binaries and will not run (use --no-arch-check to override)
```

**A macOS pack cannot be unpacked on Linux**, and the override does not change
that — it exists for inspection, not to make foreign binaries run. Copying
macOS tool installs to a Linux machine is not something ofpack will do.

## Packing for another platform

To get a toolchain onto a *different* OS, the tools have to be installed for
that OS in the first place. One command does it:

```sh
cd ~/work/myapp
ofpack pack --target linux/amd64 -o myapp.linux-amd64.ofpack.tar.gz
```

mise runs inside a [Podman](https://podman.io/) container of the target
platform, installs the project's tools there, and streams them back; the
archive is then built here. No Go toolchain, source checkout or helper script
is needed, and no binary of this machine's ever enters the archive.

```
Packing for linux/amd64 in a container (host is darwin/arm64).
The tools are installed inside the container, so the archive holds linux/amd64 binaries.

--- installing tools for the target platform ---
mise ✓ jq@1.8.1  1.1s  jq-linux-amd64
--- collecting ---
building the linux/amd64 unpacker with the local Go toolchain
  jq                           1.8.1
  mise 2026.10.17 (linux/amd64), 1 files, 1 dirs, 0 symlinks, 2.2 MiB
  includes bin/ofpack so the target needs only tar to bootstrap

Wrote /Users/you/work/myapp/myapp.linux-amd64.ofpack.tar.gz (58.9 MiB)
```

Supported targets are `linux/amd64` and `linux/arm64`.

The project should live under your home directory. A Podman machine on macOS
shares only `$HOME` by default, so a project in, say, `/work/myapp` cannot be
bind mounted into the container and the pack fails; share that path with the
Podman machine first if you need it.

Transfer and unpack are the same as for a native archive — one file, and the
Linux `ofpack` is inside it:

```sh
scp myapp.linux-amd64.ofpack.tar.gz user@host:~/
```

```sh
mkdir -p /tmp/ofpack-boot
tar -xzf myapp.linux-amd64.ofpack.tar.gz -C /tmp/ofpack-boot bin/ofpack
/tmp/ofpack-boot/bin/ofpack unpack -f myapp.linux-amd64.ofpack.tar.gz
source ~/work/myapp/.ofpack/activate.sh
```

### How it works, and what it needs

Two containers are involved, and ofpack itself runs in neither:

1. **The tools**, in an image of the *target* platform. mise installs them,
   then a small script hands back one gzipped tar: the install trees, the mise
   binary, and metadata (`mise ls --current --json`, `mise bin-paths`, the
   mise version). The archive — symlink classification, the manifest, the
   generated config — is built from that stream on this machine, which is why
   no target-platform ofpack is needed to pack.
2. **The bundled unpacker**, only when this machine has no Go. ofpack carries
   its own source, embedded, and builds a static `linux/<arch>` binary from
   it. With a Go toolchain of **go1.25 or newer** on `PATH` that happens
   locally in a second and no image is pulled at all; otherwise
   `docker.io/library/golang:1.25-bookworm` is used.

   go1.25 is above the go.mod directive on purpose, and the floor is the same
   for both routes: a binary built by go1.24 segfaults in `runtime.netpoll`
   during unpack when it is run under an emulated amd64, which is how a
   cross-built archive usually gets tried out first. go1.25 is the oldest
   version whose output was verified to unpack correctly there, so a go1.24
   toolchain on `PATH` is passed over in favour of the image rather than
   trusted. Compiling ofpack itself still only needs go1.24. That container runs on
   *this machine's* architecture and cross compiles, because a Go toolchain
   emulating a foreign architecture crashes rather than building. The result
   is checked to be an ELF binary of the right machine type before it goes
   into the archive.

`--ofpack-bin` supplies a prebuilt unpacker and skips step 2 entirely — it is
checked to be an executable of the right format and machine type first, since
bundling the wrong one would break the single-file bootstrap.
`--builder-image` changes the image step 2 uses. `--print-command` prints the
first container's invocation instead of running it; note that this is the
tool-installation step alone, which streams a payload rather than a finished
archive, so it is for inspection rather than a way to pack elsewhere.

Only environment variable **names** cross the container boundary. mise config
`[env]` entries can hold secrets, so the values are filtered out inside the
container and never reach this machine — not the manifest, not the temporary
payload, not memory. Version precedence comes from `mise bin-paths`, which
carries no values at all.

Inside the container ofpack copies *only* mise config files from the selected
root and its descendants (`mise.toml`, `.tool-versions`, and friends), plus
your global mise config. It installs and records the effective selection in
each scope. Local config above the selected root is excluded. A root relying
only on global config still packs; no configuration anywhere is an error.

### glibc compatibility

The container base image decides which glibc the tools are linked against.
The default is `debian:bookworm-slim` (glibc 2.36): its binaries run on any
Linux with **glibc 2.36 or newer**, and fail with
`version 'GLIBC_2.36' not found` on anything older. If your disconnected
machine is older — RHEL 8 and Ubuntu 20.04 both are — pack against a matching
image instead:

```sh
ofpack pack --target linux/amd64 --image docker.io/library/debian:bullseye-slim   # glibc 2.31
ofpack pack --target linux/amd64 --image registry.access.redhat.com/ubi8/ubi      # glibc 2.28
```

Check the target machine with `ldd --version` first. The image also has to
carry whatever shared libraries your tools need at runtime; a `-slim` image is
deliberately minimal, so a tool linking against, say, `libssl` may need a
fuller image.

Emulating a foreign architecture (`linux/amd64` on an Apple Silicon Mac) works
but is slow, and the occasional installer does not run under it. Always finish
with `ofpack inspect` to confirm the platform and the tool list.

### External symlinks are more common under cross packing

A fresh container install has no `~/.cargo` or system Python for a tool to link
against, so a tool that links outside the mise installs root will fail the
symlink check inside the container rather than on your machine. The failure
lists exactly which links and which tools; `--exclude` that tool and install it
on the target by other means, or pick an image that provides what it needs. See
*Nothing breaks silently* above.

### Nothing breaks silently

ofpack refuses to produce an archive that would be broken on arrival. Every
symlink in the packed trees is classified:

| symlink | result |
| --- | --- |
| relative, stays inside the archive | stored unchanged |
| absolute, points inside the archive | stored relocatable, rewritten on unpack |
| points outside the mise installs root | **pack fails** |
| points at a tool this archive does not include | **pack fails** |
| points at something that is already missing | packed, reported as dangling |

Real mise installs do contain all of these. `oc` links into
`http-tarballs/`, `rust/stable` links to `~/.cargo/bin`; both are outside the
installs root and are reported rather than quietly packed:

```
ofpack: 2 symlink(s) cannot be relocated:
  oc/4.22.6 -> /Users/alice/.local/share/mise/http-tarballs/08c2eb... (points outside the mise installs root)
  rust/stable -> /Users/alice/.cargo/bin (points outside the mise installs root)
re-run with --exclude <tool> to drop the affected tool, or --allow-unresolved to
pack anyway (the links will be missing on the target)
```

Pick one:

```sh
ofpack pack --exclude oc --exclude rust      # drop the affected tools
ofpack pack --allow-unresolved               # pack without those links, recorded in the manifest
```

`ofpack inspect` lists them again on the receiving side, and unpack warns about
them, so the gap is visible at every step.

### Nothing is overwritten by accident

By default unpack aborts before writing anything if any destination path
already exists:

```
ofpack: 2 destination path(s) already exist:
  /home/bob/work/myapp/.ofpack/bin/mise
  /home/bob/work/myapp/.ofpack/installs/node/22.20.0
re-run with --force to replace them or --skip-existing to keep them
```

* `--force` replaces them. Replaced items are kept in a backup directory for
  the duration of the run; if anything fails partway, everything is rolled back
  and the originals are restored. A destination path that is a *symlink* is
  replaced as a link — never written through.
* `--skip-existing` keeps them and installs only what is missing.

Unpack also never writes directly into the destination: the payload is
extracted into a staging directory first and moved into place only once it has
been validated in full.

### Archives are treated as untrusted input

An archive is just a tar file, and a tampered one must not be able to write
outside the destination. Extraction rejects:

* absolute paths, `..` components, non-canonical paths, backslashes, NUL bytes;
* symlinks with absolute targets, or relative targets that escape the tree;
* any attempt to write *through* a symlinked parent directory;
* hard links, device nodes, FIFOs and every other entry type — only regular
  files, directories and symlinks are extracted;
* entries whose size does not match the data, and duplicate entries.

The home-relative project path from the manifest is validated the same way
before it is used as a default destination, and because that path comes from
the archive, no existing symlink between your home directory and it is
followed either.

Permissions are carried across masked to the standard bits, so the executable
bit survives while setuid, setgid and sticky are dropped. Ownership is not
recorded at all.

## Carrying a uv Python project

A `.venv` cannot be copied between machines: it holds host-native binaries
and absolute paths, and a macOS one is of no use on Linux at all. So ofpack
does not copy it. With `--uv-project` it packs what is needed to *build* one
on the other side — the target's Python, the target's uv, and every wheel the
project's dependencies need — and `ofpack uv restore` assembles the
environment there, offline.

### Which Python is packed

In `--uv-project` mode, the Python selector comes from, in order:

1. `--python X.Y`, when given;
2. `[tools].python` in the mise configuration along the path from the
   selected root to the directory containing `pyproject.toml`; a nearer
   directory wins. Every file mise reads is considered — `mise.toml`,
   `.mise.toml`, the `.local` variants, `mise/config.toml`,
   `.config/mise.toml` and `.tool-versions` — in the order mise itself
   resolves them, and a quoted `"python"` key is read like a bare one.
   Files above the selected root and in unrelated siblings are ignored. If
   it says `latest` and `requires-python` has a lower bound, ofpack chooses
   that lowest allowed minor.

   A configuration that names a Python ofpack cannot interpret — several
   versions at once, the table form, or an empty value — stops the pack
   rather than falling through to the next source. Choosing a different
   interpreter than the directory asks for would only become apparent on the
   disconnected machine;
3. the lower bound of `pyproject.toml`'s `requires-python`;
4. global mise Python, only when the project supplies neither a scoped
   selector nor a Python lower bound.

`requires-python` is always a compatibility constraint when present: the
Python actually installed for the target is checked against it. The package
resolver then checks the dependencies. A broad lower bound like `>=3.8`
selects Python 3.8 when no scoped selector overrides it; if dependencies
require a newer interpreter, pack stops and asks you to choose one. If no
selector or lower bound exists, ofpack uses global Python if available, or
asks for `--python`. If `requires-python` is absent, a scoped or global mise
selector or `--python` still works.

For example, `mise.toml` can request `python = "latest"` while pyproject
declares `requires-python = ">=3.11"`. ofpack selects Python 3.11, then
checks whether the project dependencies can be downloaded and installed for
that Python. If they cannot, pack fails before writing an archive. A conflicting
range such as `>=3.12,<3.12` also fails before downloading. To choose a newer
minor explicitly, use:

```sh
ofpack pack --uv-project --python 3.12 --target linux/amd64 -o myapp.tar.gz
```

If `requires-python` has no lower bound, `latest` retains its normal mise
meaning and resolves on the target. The pack still checks the resolved
version against `requires-python` and verifies the wheel closure offline.

The container has an isolated mise configuration containing only Python and
uv. Scoped or global mise files only help **select Python**; their other
tools and settings are not installed. ofpack verifies the resulting wheels
in a container without network access before writing the archive.

`-C` (or cwd) sets a search root. ofpack looks below it for exactly one
`pyproject.toml`, skipping generated directories such as `.venv`, `.ofpack`
and `node_modules`. Zero or multiple matches stop with an error; use `-C`
to select the intended Python project. The archive's default destination
follows the directory containing that `pyproject.toml`.

```sh
cd ~/work/myapp
ofpack pack --uv-project --target linux/amd64 -o myapp.tar.gz
```

```
Packing the Python environment for /Users/you/work/myapp, built for linux/amd64.
  python 3.12 (selected by pyproject.toml requires-python lower bound); dependencies from pyproject.toml:
    ansible-core>=2.17,<3.0
    ansible>=10.0
    kubernetes>=29.0
    jmespath>=1.0
  uv.lock pins 32 package(s) exactly
  other mise tools are ignored in --uv-project mode
  the project's own files are not packed; only pyproject.toml, uv.lock and
  requirements.txt were read

--- downloading wheels ---
--- 32 wheels ---

--- checking the closure installs with no network ---
33 distributions installed with no network

  python 3.12.15 / uv 0.13.5
  32 wheels for python 3.12.15, verified offline
Wrote myapp.tar.gz (170.7 MiB)
```

The project checkout has to be on the disconnected machine already — the
archive carries the toolchain, not your source. `uv restore` needs at least
the `pyproject.toml`, and the `uv.lock` too if there was one when it was
packed, since it checks both against what it recorded.

On the disconnected machine, one command builds the environment:

```sh
mkdir -p /tmp/ofpack-boot
tar -xzf myapp.tar.gz -C /tmp/ofpack-boot bin/ofpack
/tmp/ofpack-boot/bin/ofpack unpack -f myapp.tar.gz
cd ~/work/myapp
~/work/myapp/.ofpack/bin/ofpack uv restore
.venv/bin/ansible --version
```

There is no choosing between fallbacks and no copying files by hand: the
Python, uv, and wheels all come out of the archive, and `uv restore` creates
`.venv` and nothing else.

### What is read, and what is not

The dependency files opened are `pyproject.toml`, and `uv.lock` and
`requirements.txt` if they exist. When `--python` is omitted, mise files
from the selected root to the Python project may be read for `[tools].python`
only; global mise configuration is a final fallback. Nothing else — not `.credentials`, not
`.generated-passwords.yml`, not `.envrc`, not inventories or roles — is read,
and none of the project's own files are packed. `.envrc` is left exactly as
it is.

`[project].dependencies` in `pyproject.toml` is what gets installed.
`requirements.txt` is read only to report a difference: in the project this
was built for it lists two of the four dependencies, so installing from it
would have left Ansible out entirely. When the two disagree, ofpack says so
and packs what `pyproject.toml` declares.

A uv archive declares a newer manifest schema than a plain one, so an older
ofpack refuses it outright instead of unpacking it without the wheels and
reporting success.

`uv.lock` is used when present, as **constraints** rather than requirements:
it pins exact versions without dragging in entries for other platforms, and
the project's own entry is excluded since it is not a wheel. A project
without a lock packs fine; versions are then resolved at pack time and frozen
into the wheels that end up in the archive.

### Isolation from your own mise setup

The container writes its own mise configuration listing only Python and uv.
Your global configuration may supply a Python selector as a final fallback;
its other tools are not installed, so a machine with twenty other tools and
unrelocatable symlinks in them packs a clean two-tool environment.
The ordinary `ofpack pack` is unaffected; `--uv-project` is a separate, opt-in
path.

### The closure is proven before the archive is written

A missing wheel discovered on a disconnected machine cannot be fixed there.
So after downloading, ofpack installs the whole closure in a second container
with `--network none`, confirms the network really is absent rather than
assuming it, and runs `pip check`. If that fails the pack fails and no
archive is written. Nothing in the build is allowed to fail quietly — a wheel
that cannot be fetched stops the pack rather than leaving a hole.

### Restoring safely

`uv restore` refuses to build if `pyproject.toml` or `uv.lock` has changed
since the archive was packed: the wheels are the ones the old declaration
needed, so the environment would not be the one you asked for.

`uv restore` also checks that the Python and uv it is about to use really
come from the archive, under `installs/<tool>/<version>`. mise reads
configuration ofpack cannot shut out, so it can otherwise answer with the
machine's own interpreter and build an environment nobody packed.

An existing `.venv` is never replaced silently. `--skip-existing` leaves it
untouched; `--force` builds the new environment in a staging directory beside
it, checks it, and only then swaps it in, putting the old one back if
anything fails. The environment is created with `uv venv --relocatable`,
without which uv writes the staging path into every console script and the
result has a working `python` but a broken `ansible`.

### Not included

Ansible Galaxy collections and `requirements.yml` are **out of scope**. This
carries the Python interpreter, uv, the wheel closure and the project's
`.venv`; collections are a separate concern and are not packed, restored or
checked.

## Carrying operating system packages

A toolchain is not always the whole story: a disconnected machine may also
need RPMs or .debs. With `--packages`, ofpack reads a `packages.txt` beside
the project, resolves the dependency closure where the repositories can be
reached, and carries the package files along.

```sh
cat packages.txt
# one package name per line; blank lines and # comments are ignored
jq
```

`packages.txt` is **only read when `--packages` is given**, so a project can
keep one without every pack picking it up. It holds package names and
nothing else — a version, a path or anything resembling an option is refused
with the line number, because these names are handed to a package manager
running as root.

### Fetching

RPMs are resolved on a machine of the target distribution, since that is
where the repositories and the entitlement are:

```sh
ofpack pack --packages \
  --package-repo rhel-10-for-x86_64-baseos-rpms \
  --package-repo rhel-10-for-x86_64-appstream-rpms \
  -o tools.tar.gz
```

`--package-repo` is **required** and repeatable. ofpack will not fetch from
"whatever is enabled": a broken or unexpected third-party repository should
not be able to fail the pack or supply a package. If one is named that it
cannot use, the error says which, and ofpack never imports a signing key on
its own.

.debs are resolved in a container of the target distribution, so
`--package-base` is required and names the image. `--target` is required
with it: the image says where the closure is resolved, not which
architecture it is for, and without it an archive packed on a Mac would
describe darwin and carry a macOS unpacker.

```sh
ofpack pack --packages --target linux/amd64 \
  --package-base docker.io/library/debian:bookworm-slim -o tools.tar.gz

ofpack pack --packages --target linux/amd64 \
  --package-base docker.io/library/ubuntu:24.04 -o tools.tar.gz
```

RPMs are the other way round: `dnf` resolves against this machine's own
repositories and downloads for its own architecture, so a native fetch
refuses a `--target` that is not this platform rather than mixing
architectures into one archive.

A project with a `packages.txt` and no mise tools produces a **package-only
archive**: no mise binary, no tool tree, and no `activate.sh`, because there
would be nothing for it to activate. Otherwise the packages travel in the
same archive as the toolchain.

### Installing

Unpack puts the package files on disk and stops. Installing them changes the
whole machine, so it is a separate command and never happens on its own:

```sh
ofpack unpack -f tools.tar.gz --dest ~/tools
sudo ofpack packages install --dest ~/tools
```

Four things happen before anything is installed:

* the running system's `/etc/os-release` must match the one the closure was
  resolved for, **including the minor version** — RHEL 10.2 and 10.3 ship
  different builds, so a closure for one is not a closure for the other.
  `--no-os-check` overrides it deliberately;
* every package file is checked against the digest in the manifest, and a
  file the manifest does not list stops the install too. An archive is
  untrusted input and a package is executable content;
* the package manager runs with its repositories disabled — `dnf
  --disablerepo '*' --cacheonly`, and for apt an empty source list — so it
  cannot reach out, whether or not the machine is actually disconnected;
* root is required, and ofpack does not escalate by itself. It tells you to
  re-run it under `sudo` rather than doing it for you.

Package names reach the manager as arguments, never as part of a shell
command, and the container that resolves .debs reads them from a mounted
file rather than having them baked into its script.

### What the closure covers

| manager | how it is resolved | what that means |
| --- | --- | --- |
| dnf | `dnf download --resolve --alldeps` | the whole closure, including packages the target probably already has |
| apt | `apt-get --download-only` with an empty dpkg status | the declared dependency closure of the named packages |

The apt figure deserves the detail. Left to itself, apt downloads only what
the machine it runs on is missing, which would leave a target with a
different base short of exactly the packages the builder happened to have.
Emptying the package status makes it plan as though nothing were installed.
What it still omits are `Essential: yes` packages, which apt treats as always
present — the manifest records that, so the guarantee is written down rather
than assumed.

Ansible Galaxy collections, language package managers and anything else
outside RPM and .deb are not in scope here.

## Known limits

* **Absolute paths baked into file contents are not rewritten.** ofpack fixes
  symlinks; it cannot fix a path compiled into a binary or written into a
  script's shebang. Tools installed through language package managers
  (`npm:`, `pipx:`, `cargo:`) are the usual suspects. Unpacking under the
  default destination keeps the project's relative layout, but a tool that
  hardcoded the *source* home directory will still need reinstalling.
* **Only the installs root is carried.** A tool that depends on something
  elsewhere on the source machine (`~/.cargo`, a system library) is not made
  self-contained by ofpack. Where this is detectable through a symlink, pack
  fails rather than pretending otherwise.
* **Env var values are not carried**, by design — see Secrets above.
* Hard links inside an install tree are extracted as separate files, so an
  archive can be larger on disk than the original.
* **uv could not be run under qemu in testing.** On an Apple Silicon Mac,
  `podman run --platform linux/amd64` with uv 0.13.5 segfaulted
  (`qemu: uncaught target signal 11`) while the same binary ran natively on
  linux/arm64 and on a real x86_64 host. Packing for linux/amd64 from macOS
  is unaffected, because the wheels are downloaded with Python and the
  offline check runs with pip; but the uv path itself is exercised on the
  target rather than during a cross-platform pack. This is one host, one
  emulator and one uv version, not a general statement about uv.

## Releases

Releases are cut by [release-please](https://github.com/googleapis/release-please)
and built by [GoReleaser](https://goreleaser.com/), both from GitHub Actions.
Nothing is published by hand.

### How a release happens

1. Commits land on `main` using [Conventional
   Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, …).
2. `release-please` keeps a release pull request open, with the next version
   and the changelog derived from those commits.
3. Merging that pull request creates the tag and the GitHub release, and
   rewrites the version constant and `.release-please-manifest.json`.
4. GoReleaser builds the binaries **from that tag** and attaches them to the
   release.
5. A verification job downloads what was published and checks it: all four
   platform archives present, checksums matching, and the binary reporting
   the tag's version.

### What is distributed

| | amd64 | arm64 |
| --- | --- | --- |
| linux | `ofpack_linux_amd64.tar.gz` | `ofpack_linux_arm64.tar.gz` |
| darwin | `ofpack_darwin_amd64.tar.gz` | `ofpack_darwin_arm64.tar.gz` |

Plus `checksums.txt`. There is no Windows build: the archive format relies on
unix symlinks and file modes, and unpack is built around them.

Binaries are built with `CGO_ENABLED=0`. A dynamically linked ofpack would
need a matching libc on the disconnected machine, which is precisely what
cannot be assumed there — and the same binary is what gets bundled into an
archive as `bin/ofpack`.

### One version, three places

The version lives in `internal/version/version.go` as a **constant**, not as
an `-ldflags` variable:

```go
// x-release-please-start-version
const Version = "0.1.0"
// x-release-please-end
```

This matters because ofpack compiles a second copy of itself: `pack --target`
builds the `bin/ofpack` that travels inside an archive from source embedded
in the binary. That build cannot receive the linker flags the release build
used, so a version injected at link time was present in the binary you ran
and absent from the one the archive carried, which reported `dev`. A
constant is compiled into both.

CI checks that the constant, the built binary and the release manifest all
agree.

### Repository settings the workflows need

* **`RELEASE_PLEASE_TOKEN`** — a secret holding a token that may open pull
  requests and create releases. The default `GITHUB_TOKEN` is not used for
  this, because releases it creates do not trigger other workflows.
* **`GITHUB_TOKEN`** — provided automatically; GoReleaser and the
  verification job use it to attach and download artifacts.
* **Workflow permissions** — `contents: write` and `pull-requests: write` at
  the workflow level, so release-please can maintain its pull request and
  GoReleaser can upload to the release.

No container images are published; ofpack ships as binaries only.

## Development

```sh
go test ./...
```

The cross-platform path was verified on macOS/arm64 with Podman: packing
`--target linux/amd64`, then bootstrapping the result in a fresh linux/amd64
container with nothing but `tar` and the bundled `bin/ofpack`, and running the
packed tool from it.

The end-to-end tests pack a real tool, unpack it into an isolated `HOME`, and
check that the bundled mise finds and resolves it offline. They need mise and
an already installed tool, so they are opt in:

```sh
OFPACK_E2E_TOOL='jq@1.8.1' go test ./... -run EndToEnd
```
