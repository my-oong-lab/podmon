# podmon

A small dashboard for a home server: CPU (total and per core), memory and swap,
disk usage and throughput, network, load, uptime, temperatures, and the
containers running under **Podman**. One static Go binary (~7 MB), no
third-party Go modules, no CDN, no database. History is kept in memory for the
last 10 minutes.

It is built for modest hardware: one background sampler reads `/proc` every
2 seconds regardless of how many browsers are open, and Podman is only polled
while someone is looking at the page.

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o podmon .
# for the server from another machine:
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o podmon .
```

## Set up on the server

1. Enable the rootless Podman API socket (gives per-container CPU and memory):
   ```sh
   systemctl --user enable --now podman.socket
   ```
   Without it podmon falls back to `podman ps`: names, images, state and ports
   still show, but not per-container CPU and memory.
2. Create a password file (at least 12 characters):
   ```sh
   mkdir -p ~/.config/podmon && chmod 700 ~/.config/podmon
   head -c 24 /dev/urandom | base64 > ~/.config/podmon/password
   chmod 600 ~/.config/podmon/password
   cat ~/.config/podmon/password      # this is your login password
   ```
3. Install and start:
   ```sh
   install -D podmon ~/.local/bin/podmon
   mkdir -p ~/.config/systemd/user
   cp deploy/podmon.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now podmon
   loginctl enable-linger "$USER"
   ```
4. Open `http://<server-ip>:8080` and sign in as `admin`.

## Security notes

- **Login is required** unless you pass `-no-auth`, which only works on a
  loopback address. Five wrong passwords lock the client out for five minutes.
- **Plain HTTP sends the password unencrypted.** On a trusted home LAN that may
  be acceptable. Otherwise pass `-tls-cert`/`-tls-key`, or reach the dashboard
  through an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 server`, and run podmon
  with `-listen 127.0.0.1:8080`) or a VPN such as WireGuard/Tailscale.
- **Do not expose it to the internet.**
- It is read-only: it cannot start, stop or change containers. Command lines,
  environment variables, labels and mounts are never read or shown.
- Behind a reverse proxy, set `-allowed-hosts your.name` so only that Host
  header is accepted.
- Failed logins are counted per client address. Behind a reverse proxy every
  visitor shares the proxy's address, so five wrong guesses from anyone lock
  everyone out for five minutes. Prefer direct access, a VPN or an SSH tunnel.
- At most 16 login attempts from one client are checked at the same time, so a
  burst of parallel guesses is capped at a few more than the five-try limit
  instead of being unlimited.
- With `-tls-key`, keep the key readable only by the account running podmon
  (`chmod 600`); podmon does not check this for you.
- Pass `-podman-bin /usr/bin/podman` if you rely on the `podman ps` fallback and
  do not fully trust `PATH`.
- The bundled font files come from the npm package named in
  `third_party/barlow-semi-condensed/SOURCE.txt`, with checksums.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8080` | address and port |
| `-user` | `admin` | login name (or `PODMON_USER`) |
| `-tls-cert`, `-tls-key` | | serve HTTPS |
| `-allowed-hosts` | | comma-separated Host allowlist |
| `-podman-socket` | auto | Podman API socket path |
| `-podman-bin` | from PATH | absolute path of `podman` for the fallback |
| `-interval` | `2s` | sampling interval, 1s to 60s |
| `-no-auth` | off | no login, loopback only |

The password comes from `PODMON_PASSWORD_FILE` (preferred) or `PODMON_PASSWORD`.

## Tests

```sh
go vet ./... && go test -race ./...
```

The font (Barlow Semi Condensed) is bundled under the SIL Open Font License;
see `third_party/barlow-semi-condensed/LICENSE`.
