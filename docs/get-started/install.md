# Install kwt

Choose a Go install, a release archive, or a source build. kwt supports:

- macOS 13 or newer, Linux, and Windows;
- Git 2.20 or newer;
- Git 2.31 or newer for `kwt doctor`, `kwt prune --expired`, and
  `kwt prune --merged`;
- tmux 2.1 or newer for workspace launch and `kwt tmux`; and
- Go 1.27 or newer for `go install` and source builds.

The worktree-oriented CLI can still be used without tmux.

## Install with Go

Install v0.5.0 with:

```sh
go install go.kenn.io/kwt/cmd/kwt@v0.5.0
```

Go places the binary in `$(go env GOPATH)/bin` by default. Make sure that
directory is on `PATH`, then confirm the installation:

```sh
kwt version
```

Use `@latest` when you want Go to select the newest tagged release:

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
```

## Download a release archive

The [GitHub Releases](https://github.com/kenn-io/kwt/releases) page provides
v0.5.0 archives for:

- macOS 13 or later on Apple silicon and Intel;
- Linux on ARM64 and AMD64; and
- Windows on ARM64 and AMD64.

Each release includes `checksums.txt`. Verify the archive before extracting it,
then place `kwt` (or `kwt.exe`) somewhere on `PATH`. The older `v0.3.0` tag
predates packaged release artifacts and remains available through `go install`.

## Build from source

```sh
git clone https://github.com/kenn-io/kwt.git
cd kwt
make build
./kwt version
```

Use `make install` to install the current checkout into the first directory in
`GOPATH`. Set `INSTALL_DIR` to choose another location:

```sh
make INSTALL_DIR="$HOME/.local/bin" install
```

## Add shell integration

kwt can generate completions for Bash, Zsh, Fish, and PowerShell. For example:

=== "Zsh"

    ```sh
    source <(kwt completion zsh)
    ```

=== "Bash"

    ```sh
    source <(kwt completion bash)
    ```

=== "Fish"

    ```fish
    kwt completion fish | source
    ```

=== "PowerShell"

    ```powershell
    kwt completion powershell | Out-String | Invoke-Expression
    ```

When `cd.launch_shell = false`, the same generated script also installs the
wrapper that lets `kwt cd` change the current shell's directory.

Continue with the [quickstart](quickstart.md) to create and open your first
workspace.
