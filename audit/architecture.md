# podmon architecture (run-1, quick profile)

## 1. Product, principals, protected resources
podmon is a read-only home-server dashboard: one Go binary (stdlib only, `go.mod:1-3`) serving an embedded vanilla-JS page and two JSON endpoints. It reports host CPU, memory, disk, network, load, temperatures (from `/proc`, `/sys`) and Podman containers (rootless Podman API socket, or `podman ps` fallback).
Principals: (a) the operator who starts it and chooses flags, env and the password file; (b) one shared authenticated dashboard user (HTTP Basic, no roles, no sessions); (c) unauthenticated network peers, who may reach only `GET /healthz` by design; (d) container workloads, which are not request principals but whose names/images/ports/status are attacker-influenced strings that flow into the dashboard.
Protected resources: the shared credential, host and container metadata (names, images, ports and bound IPs, mount points, hardware), and availability of a 4 GB home server. There is no write or control path: only GET routes exist, and Podman is only called with GET.

## 2. Comparable-software baseline
No source-grounded comparable exists (no dependencies; README names none). Omitted.

## 3. Stack, deployment paths, offline limits
Go (toolchain 1.24, `go 1.22`), `net/http` method-pattern mux, `//go:embed web` served from an in-memory exact-path map (`server.go`, `main.go:24`). Linux only (`collect_linux.go`, build tag). Deployed as a systemd user unit: `deploy/podmon.service` runs `-listen 0.0.0.0:8080` over plain HTTP with `PODMON_PASSWORD_FILE`; TLS and `-allowed-hosts` are optional flags. Offline build and `go test` need no network. Running the binary reads real `/proc`/`/sys`, binds a port and may dial Podman, so it is excluded from this run. Local platform: `unshare` namespaces work, but read-only mounts, disk quota and a combined wrapper are unproven, so target execution is blocked. No scratch-promotion code exists. This run is source review only.

## 4. Entry surfaces and source-to-sink paths
- HTTP: `GET /healthz` (no auth), `GET /api/state`, `GET /api/history`, `GET /` (static, exact map lookup, no filesystem path built). Request fields consumed: `Authorization`, `Host`, `RemoteAddr`, `URL.Path`, conditional headers. No bodies, cookies, sessions, CORS or CSRF tokens.
- CLI/env/config: `-listen -user -no-auth -tls-cert -tls-key -allowed-hosts -podman-socket -podman-bin -interval`; `PODMON_USER`, `PODMON_PASSWORD`, `PODMON_PASSWORD_FILE` (`main.go loadConfig`).
- Podman socket / CLI to browser: JSON decoded (8 MiB cap) into `apiContainer`/`apiStat`, passed through `toContainer` and `sanitize`, cached in a snapshot, served to every `/api/state` caller, rendered by `web/app.js`.
- `/proc`, `/sys`, `/etc/os-release` to browser: bounded `readSmall` reads, parsers in `collect_linux.go`, free-text fields (mount, device, hostname, labels) sanitized; mount paths reach `syscall.Statfs`.
- Browser: `web/app.js` builds DOM with `h()` using `textContent`/`setAttribute`; one `localStorage` flag.

## 5. Trust boundaries and strongest source-visible controls
- Network to handlers: Basic auth (`authed`), constant-time hash compare, lockout limiter keyed by `RemoteAddr` (IPv6 by /64), 64 concurrent requests, server timeouts, 16 KB headers. Middleware order: recoverer, securityHeaders, hostCheck, limit, mux.
- Host header: allowlist enforced only when non-empty; it defaults on only in `-no-auth` loopback mode.
- Config: password min 12 chars; password file must be a regular 0600 file; `-no-auth` loopback-only.
- Podman socket: must be a socket owned by root or the euid (Lstat), IDs validated before use, GET only; CLI fallback uses a fixed argv and filtered env.
- Output to browser: `sanitize` strips control/format characters and bounds length (no HTML escaping); CSP `default-src 'none'; script-src 'self'; style-src 'self'`; DENY framing; `no-store` on `/api/*`.
- Deployment facts source cannot establish: whether the unit hardening is active, whether TLS or a proxy fronts the port, firewall/LAN exposure, `PATH`, file permissions of the installed password file.

## 6. Starting paths
`main.go`, `server.go`, `podman.go`, `collect_linux.go`, `util.go`, `metrics.go`, `web/app.js`, `web/index.html`, `deploy/podmon.service`, `podmon_test.go`.

## 7. Prior coverage
No prior ledger or findings exist for this repo. No carried confirmations, no changed-source revalidation, no hunter exclusions.

## 8. Companion selection (derived from ATTACK-CLASSES.md)
Selected: `WEB-PROTOCOL-AND-AUTH.md` (HTTP listener with Basic auth, Host header, caching), `CLIENT-SIDE.md` (browser DOM rendering of server-supplied strings, storage, framing), `RESOURCE-EXHAUSTION-AND-AVAILABILITY.md` (pre-auth work, bounded buffers, fatal errors on a small host), `DESKTOP-MOBILE-AND-LOCAL-IPC.md` (outbound unix-socket IPC to Podman, local socket ownership), `CLOUD-AND-DEPLOYMENT.md` (shipped systemd unit and secret lifecycle).
Excluded: `AI-AND-LLM.md` (no model or agent code), `MEMORY-SAFETY-AND-BINARY.md` (pure Go, no unsafe or cgo), `PROTOCOLS-RPC-AND-MESSAGING.md` (no RPC, broker or webhook; the Podman REST client is covered under local IPC), `DATA-ISOLATION-AND-LIFECYCLE.md` (single shared principal, no tenants, no stored data), `SUPPLY-CHAIN-AND-RELEASE.md` (no dependencies, CI or release process in the repo).
