# podmon security audit, run-1

## 1. Run scope and limits

**This is a partial pass.** It used the `quick` profile of the Cloudflare `security-audit` skill, so it ran one hunter wave and one final coverage critic, and the critic's gaps were recorded as deferred, not hunted.

| Item | Value |
|---|---|
| Target | `/home/claude/podmon`, whole repository (`scope_paths: ["."]`) |
| Source ref | Not a git repository. Audited tree identified by content hash `c423b136900784bb` (sha256 over the sorted per-file hashes). Edits made after the audit are listed in section 6. |
| Profile | `quick`: units coarsened to surface × boundary × attack class |
| Budget | 16 agent invocations. 8 agents returned results (4 reconnaissance, 3 hunters, 1 coverage critic). The other 8 launches were aborted by account usage limits before doing any work and are counted against the budget, so it was fully used. |
| Execution | Source review only. No audit agent built or ran target code, because no OS-enforced sandbox (read-only mounts, disk limit, artifact promotion) was verified. Every recorded check has `method: source`. (The app's own unit tests and a loopback smoke test were run by me while building the app. They are not audit evidence.) |
| Prior runs | None existed. Nothing was carried over, and no changed-source revalidation applies. |
| Deviation | Hunter prompts were delivered as a file each hunter read in full, instead of being pasted into the delegation message. The file holds the skill's blocks verbatim. |

## 2. Posture summary

No confirmed vulnerabilities and no unresolved leads (`findings.json` is `[]`). Across 7 covered units, hunters found the app's controls sound on the paths they read: every route except `/healthz` is behind Basic auth, there is no path or HTML injection sink, and only fixed-argument, read-only calls reach Podman. 15 hardening notes came out of the hunt. The critic then found two behaviours the hunters had passed too quickly, and four unowned areas. All six are deferred below. I fixed the two behaviours afterwards (section 6).

A clean result from one source-only quick pass means "nothing found in the paths examined", not "secure".

## 3. Confirmed findings

None.

## 4. Needs validation

None. See `NEEDS-VALIDATION.md`. Deployment facts the source cannot show (whether the unit hardening is active, whether TLS or a proxy fronts port 8080, firewall exposure, file permissions of the installed password file) are not leads, because no source-grounded boundary hypothesis depends on them.

## 5. Hardening notes and positive patterns

**Hardening notes (not vulnerabilities).** Status is as of this report.

| Note | Status |
|---|---|
| Lockout is checked, then counted, in separate steps, so a parallel burst could be evaluated before the lockout appeared (critic) | Fixed after the audit |
| Limiter eviction under many source keys could drop active lockouts | Fixed after the audit |
| `statfs` on a hung mount, including `fuseblk`, could freeze the single sampler and delay start-up (critic and hunter) | Fixed after the audit |
| `Blocks - Bfree` could underflow on an unusual filesystem | Fixed after the audit |
| `safeDevName` accepted `.` and `..` (not reachable from kernel-sourced names) | Fixed after the audit |
| `sanitize` did not strip U+2028 and U+2029 | Fixed after the audit |
| Shared lockout key behind a reverse proxy | Documented in README |
| `PATH` resolves `podman` for the CLI fallback unless `-podman-bin` is set | Documented in README |
| `-tls-key` file mode is not checked | Documented in README |
| Shipped unit serves plain HTTP on `0.0.0.0:8080`, so the login crosses the LAN unencrypted | Open by design; README and a startup warning explain the options |
| No cap on accepted TCP connections, only on in-flight handlers (64) | Open (volumetric) |
| An unauthenticated flood can occupy the 64 handler slots | Open (volumetric) |
| `ServeMux` answers 405/301 before authentication (no data) | Open |
| `validSocket` checks the socket, then dials (no `SO_PEERCRED`) | Open, same-user authority only |
| Static assets use `no-cache`, not `no-store` | Open (public source) |
| HSTS is sent only with `-tls-cert` | Open, add it at the proxy |
| `h()` passes any key to `setAttribute`; safe today, unsafe if a future call uses data-derived keys | Open |
| 403 and 429 responses show as generic "offline" in the UI | Open (UX) |

**Positive source patterns.** Constant-time credential comparison over fixed-size digests. A password is mandatory, 12 characters or more, from a 0600 file, and `-no-auth` is loopback-only. Static files are served from an in-memory exact-match map, so no request value forms a filesystem path. Podman is called with GET only, container IDs are validated as 64 hex characters, responses are capped at 8 MiB, and command lines, environment, labels and mounts are never read. All server strings reach the DOM through `textContent`, and the CSP forbids inline script. Every `/proc` and `/sys` read is size-bounded.

## 6. Changes made after the audit (not re-audited)

After the audit I changed the code to address the fixed items in section 5: an atomic login limiter (admission and outcome are recorded under one lock, with at most 16 attempts evaluated per client at once, and active lockouts evicted last), a 2-second bounded `statfs` that skips a wedged mount, the underflow guard, the two input tightenings, README notes, and `third_party/barlow-semi-condensed/SOURCE.txt` with checksums.

I verified these by running the project's tests under the race detector, including new tests for parallel guesses, lockout survival under key churn, and success resetting the counter. I also ran a loopback burst: of 40 parallel wrong guesses, 5 were evaluated and 35 refused, and 12 parallel correct logins all succeeded. The pre-fix race was identified from the critic's reading of the source and was not demonstrated by execution. **None of this was reviewed by an independent audit agent.**

## 7. Coverage summary (from `coverage-ledger.json`)

| Status | Units |
|---|---|
| covered | 7 |
| candidate | 0 |
| blocked | 0 |
| deferred | 6 |

**Covered (7):** HTTP auth and routes (access control); credential handling; the shipped unit and obvious-things check; Podman socket and CLI ingestion; the chained refresh lifecycle; browser DOM rendering; embedded assets and response headers.

**Deferred (reason `quick_profile_final_critic`):**
1. Pre-auth limiter, reassigned: atomicity of the lockout. The earlier conclusion is archived in `attempts`.
2. `/proc`, `/sys` ingest, reassigned: `fuseblk` `statfs`, start-up sampling and shutdown. The earlier conclusion is archived.
3. TLS listener mode: `-tls-key` handling, HTTP/2 against HTTP/1-derived bounds, HSTS.
4. Lockout race as its own unit (companion to 1).
5. Process lifecycle: sampler priming, sampler goroutine and shutdown.
6. Vendored binary assets: the three `.woff2` files and licence. Provenance cannot be established offline. Source and checksums are now recorded, and the files are byte-identical to the npm package.

**Key exclusions:** AI, memory-safety, RPC/messaging, data-isolation and supply-chain companions were excluded with source-fact reasons. The critic found the supply-chain exclusion too broad because of the vendored font, which is deferred unit 6. "Feature abuse and data leakage" was excluded for most units because the app is read-only. Authenticated disclosure of host and container metadata is by design.

**Final critic:** `stop: false`. It accepted no clean-coverage claim: 4 missing units and 2 reassignments, all deferred. A `deep` run would hunt them.
