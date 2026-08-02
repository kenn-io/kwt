# Install kwt

kwt supports macOS, Linux, and Windows. Git is required. tmux is required for
workspace launch and the `kwt tmux` commands, but the worktree-oriented CLI can
still be used without it.

## Install with Go

If you have Go 1.26 or newer, install the latest tagged version:

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
```

Go places the binary in `$(go env GOPATH)/bin` by default. Make sure that
directory is on `PATH`, then confirm the installation:

```sh
kwt version
```

To pin an exact version, replace `latest` with a semantic-version tag:

```sh
go install go.kenn.io/kwt/cmd/kwt@v0.3.0
```

## Download a release archive

For tags published through the current release pipeline, the
[GitHub Releases](https://github.com/kenn-io/kwt/releases) page provides
archives for:

- macOS on Apple silicon and Intel;
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
