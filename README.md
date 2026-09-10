# Jarvis

A voice interface to ongoing work on Spark. One Go daemon, a browser client,
and Mux's existing terminal ownership. See [DESIGN.md](DESIGN.md) for boundaries
and [RESEARCH.md](RESEARCH.md) for the source research.

## Run

Requires Mux protocol v9 with `inspect`, `observe` and `input` commands.

```sh
nix run .#jarvisd
```

Open <http://localhost:19743>. Read the access token locally from
`~/.local/state/jarvis/access-token` and enter it in the browser. The token is
never printed by the daemon. For a systemd deployment the token is at
`/var/lib/jarvis/access-token`.

For a Mac browser, forward the loopback service:

```sh
ssh -N -L 19743:127.0.0.1:19743 spark
```

The browser can list, inspect, follow and send a line to an existing terminal.
Sending uses a recently displayed snapshot and refuses changed terminal state.
PTY input shares the application's composer; inspect it before sending.

Voice requires an OpenAI project key with GPT-Live and the configured backend
model available. Supply `OPENAI_API_KEY` through a runtime environment file or
use `--openai-key-file /path/to/key`. Start a conversation explicitly. Silence
during a connected voice session is billable. Local observation continues with
voice disconnected.

## NixOS

```nix
{
  imports = [ inputs.jarvis.nixosModules.default ];
  services.jarvis = {
    enable = true;
    user = "rathi";
    muxPackage = config.services.muxd.package;
    environmentFiles = [ config.sops.secrets."jarvis.env".path ];
  };
}
```

Omit `environmentFiles` to use local controls before configuring the API key.
Secrets belong in runtime files, never literal Nix strings or the repository.

## Validate

```sh
nix develop -c go test -race ./...
nix develop -c go vet ./...
nix build
```

Tests cover authentication, stale and duplicate input, crash recovery and the
documented managed Live function exchange using a mock server. They do not prove
real OpenAI access, voice latency or locked-phone behavior.
