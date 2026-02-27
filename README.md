# sip

A tiny package manager. Installs binaries from GitHub Releases with zero configuration.

No formulas, no registry, no external dependencies. Single Go file, single binary.

## Install

```
go install github.com/mnmlyw/sip@latest
```

Or build from source:

```
go build -ldflags="-s -w" -o sip .
```

Add `~/.sip/bin` to your `PATH`:

```sh
# bash/zsh
export PATH="$HOME/.sip/bin:$PATH"

# fish
fish_add_path ~/.sip/bin
```

## Usage

```
sip i BurntSushi/ripgrep     # install
sip r ripgrep                # remove
sip u                        # upgrade all
sip l                        # list installed
sip s rip                    # search installed
sip n ripgrep                # info
```

## How it works

sip downloads the latest release from GitHub, picks the right binary for your platform, and symlinks it into `~/.sip/bin`.

```
~/.sip/
  bin/
    rg -> ../pkg/ripgrep/rg
    fd -> ../pkg/fd/fd
  pkg/
    ripgrep/
      .repo       # "BurntSushi/ripgrep"
      .version    # "15.1.0"
      rg          # actual binary
```

No JSON state, no cache — everything is derived from the filesystem.

Asset selection scores each release artifact by OS, architecture, and format, then picks the best match. Supports `GITHUB_TOKEN` for API rate limits.
