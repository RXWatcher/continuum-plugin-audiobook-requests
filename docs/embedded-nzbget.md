# Embedded NZBGet supervisor

When `abook_download_mode=embedded_nzbget`, the plugin process supervises an NZBGet child process on loopback. Implementation lives in `internal/embeddednzbget/`.

The bundled NZBGet tarball (`internal/embeddednzbget/dist/nzbget-26.1-linux-amd64.tar.gz`) is `go:embed`-ed into the binary; the supervisor extracts it on first run.

Supported platform: `linux/amd64` only. `New()` refuses anything else.

## On-disk layout

Anchored under `embedded_download_dir` (the same dir used for embedded torrents — they don't collide because NZBGet's files live under `.nzbget/`):

```
<embedded_download_dir>/.nzbget/
├── bin/                              # extracted NZBGet binaries
│   ├── nzbget-x86_64                 # main daemon
│   ├── unrar-x86_64
│   ├── 7za-x86_64
│   ├── cacert.pem                    # NZBGet's CertStore
│   ├── webui/                        # not exposed; control surface is RPC only
│   └── .bundle-version               # sentinel: "<bundleVersion>:<sha256>"
├── queue/                            # NZBGet MainDir
│   ├── dst/                          # DestDir (completed downloads land here)
│   ├── inter/                        # InterDir (in-flight downloads)
│   ├── tmp/                          # TempDir
│   ├── queue/                        # QueueDir (NZBGet's internal state)
│   ├── nzb/                          # NzbDir
│   └── nzbget.log                    # LogFile (also pumped through hclog)
└── nzbget.conf                       # regenerated on every Start
```

`ensureExtracted` only re-extracts when the `.bundle-version` sentinel doesn't match `<embedded bundleVersion>:<embedded bundleSHA256>`. Plugin upgrades that change the bundled NZBGet version trigger a re-extract. A SHA-256 check on the embedded tarball runs first as defence-in-depth against ldflags injection / build-time tampering — if the sum mismatches, Start fails with `bundle SHA mismatch: embedded bundle has been tampered with`.

## Generated credentials

Every `Start()` mints fresh RPC credentials via `newRPCCredentials` (8 random bytes for the username, 16 for the password, hex-encoded; username is prefixed `silo-`). They're written into `nzbget.conf` and exposed via `Manager.Credentials()` / `Manager.Endpoint()`.

In `main.go`, after the supervisor starts, the plugin **overwrites** the config's `NZBGetURL`, `NZBGetUsername`, and `NZBGetPassword` with the supervised values:

```go
cfg.NZBGetURL = fmt.Sprintf("http://127.0.0.1:%d", sup.Port())
cfg.NZBGetUsername = user
cfg.NZBGetPassword = pass
if cfg.NZBGetCategory == "" {
    cfg.NZBGetCategory = "audiobooks"
}
```

Operator-supplied NZBGet credentials are **ignored** in this mode. The admin UI still lets you fill them in (validation passes them through), but on the next plugin restart they're discarded and replaced again. To switch to a real NZBGet on the network, set `abook_download_mode=external_nzbget` first.

## Port selection

`pickFreePort` asks the kernel for a free ephemeral port via `net.Listen("tcp", "127.0.0.1:0")` then closes the listener. There's a tiny TOCTOU window before NZBGet binds it; in practice it has never been observed to collide. If it does, `waitForPort` times out at `portProbeDeadline = 30s` and Start fails with `rpc port never came up`.

NZBGet binds to `127.0.0.1` only. The `nzbget.conf` settings that enforce loopback:

```
ControlIP=127.0.0.1
AuthorizedIP=          # empty = no extra hosts
FormAuth=no
DaemonUsername=
```

## Lifecycle

| Operation | What it does | Idempotent? |
| --- | --- | --- |
| `Start(ctx)` | ensureExtracted → pickFreePort → mintCreds → write conf → spawn `nzbget-x86_64 -c <conf> -s` (foreground, NOT --daemon) → wait for RPC port (≤30s) | Yes — if already running and reachable, returns nil. If already running but the port stopped answering, Stop()s and restarts. |
| `Stop()` | SIGTERM the whole process group → wait ≤ `supervisorShutdownTimeout = 10s` → SIGKILL → drain log goroutines (≤2s each) | Yes — no-op if never started. |
| `Endpoint()` | Returns `http://<user>:<pass>@127.0.0.1:<port>/` or `""` before Start. | Read-only. |
| `Credentials()` | Returns the current `(user, pass)` tuple. | Read-only. |
| `Port()` | Returns the current bound port, or `0`. | Read-only. |

### Configure-time restart semantics

Configure builds a fresh supervisor and `Start()`s it before swapping out the old one. The old supervisor is `Stop()`ed **after** the new one is serving — so the consumer never sees a "no NZBGet" window between configs.

The new daemon writes a fresh `nzbget.conf` on every Start (regenerated from the current Usenet provider settings + new RPC creds). NZBGet reloads its conf from disk on start; there is no "graceful conf reload" path.

### Init failure fallback

If `embeddednzbget.New` returns an error (e.g. wrong platform, missing download dir), or `Start` fails (port stuck, extract failed, tampered bundle), `main.go` logs a warning and **falls back to the operator-supplied external NZBGet config** (`cfg.NZBGetURL` / username / password as-typed). If those are also empty, `cfg.AbookConfigured()` returns false and abook is silently disabled for every event.

## Logs

NZBGet's stdout/stderr are pumped line-by-line through the plugin's hclog as `nzbget[stdout]` / `nzbget[stderr]` with the daemon line in the `line` field. To grep:

```bash
journalctl -u silo --since "10 min ago" | grep 'nzbget\[' | tail -50
```

NZBGet also writes its own log to `<embedded_download_dir>/.nzbget/queue/nzbget.log`. That file has more detail (per-article failures, par2 progress) than the line-pump emits.

## News provider config

Encoded into `Server1.*` in the generated conf:

```
Server1.Host        ← usenet_host
Server1.Port        ← usenet_port
Server1.Username    ← usenet_username
Server1.Password    ← usenet_password
Server1.Encryption  ← "yes" if usenet_ssl else "no"
Server1.Connections ← usenet_connections (default 8)
```

The conf intentionally configures **one** provider. If you need backup tiers, multiple servers, or any non-default tuning, run a standalone NZBGet and switch to `external_nzbget` — the supervised daemon is deliberately minimal.

## Common supervisor issues

| Symptom | Likely cause | Where to look |
| --- | --- | --- |
| Plugin logs `embedded nzbget failed to start; falling back to external` | Usenet provider creds wrong, port collision (30s probe), disk too full to extract bin, SELinux/AppArmor blocking exec | `journalctl` for the warning + `<download_dir>/.nzbget/queue/nzbget.log` if it got far enough to start |
| `bundle SHA mismatch` | Build artifact tampered with, or filesystem corrupted the extracted bin | Re-install the plugin |
| NZBGet starts but every download fails with auth errors | Wrong `usenet_username` / `usenet_password` | NZBGet's own log shows `Authentication failed` — easier to read than the plugin's pumped lines |
| RPC test button works, but appends never start downloading | Provider unreachable, or `Server1.Encryption` mismatch (SSL on but provider expects plain, or vice versa) | NZBGet log; toggle `usenet_ssl` |
| Plugin restart leaves the daemon orphaned | The supervisor SIGTERMs the **process group** (`Setpgid` is set). An orphan means the plugin died non-gracefully | `ps -ef | grep nzbget-x86_64` after a crash; manually `kill -TERM -<pgid>` |
| `nzbget did not exit on SIGTERM; sending SIGKILL` | Long-running par2 / unrar; rare. Tune `supervisorShutdownTimeout` in source if it becomes routine | the daemon log immediately before shutdown |
