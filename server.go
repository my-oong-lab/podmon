package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxInFlight  = 64
	authMaxFails = 5
	authWindow   = 5 * time.Minute
	authLockout  = 5 * time.Minute
	authMaxKeys  = 4096
)

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; " +
	"img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

type staticFile struct {
	data  []byte
	ctype string
}

type app struct {
	cfg     *config
	sampler *sampler
	pm      *podmanClient
	static  map[string]staticFile
	started time.Time

	userHash [32]byte
	passHash [32]byte
	limiter  *failLimiter
	sem      chan struct{}
	hosts    map[string]struct{}
}

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
}

// loadStatic reads the embedded web assets into memory once. Requests are
// answered from this map by exact path, so no request value is ever used to
// build a filesystem path.
func loadStatic(fsys fs.FS) (map[string]staticFile, error) {
	files := map[string]staticFile{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ct, ok := contentTypes[path.Ext(p)]
		if !ok {
			return fmt.Errorf("unsupported embedded file type: %s", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files["/"+p] = staticFile{data: b, ctype: ct}
		if p == "index.html" {
			files["/"] = staticFile{data: b, ctype: ct}
		}
		return nil
	})
	return files, err
}

func newApp(cfg *config, s *sampler, pm *podmanClient, assets fs.FS) (*app, error) {
	static, err := loadStatic(assets)
	if err != nil {
		return nil, err
	}
	a := &app{
		cfg:     cfg,
		sampler: s,
		pm:      pm,
		static:  static,
		started: time.Now(),
		limiter: newFailLimiter(authMaxFails, authWindow, authLockout, authMaxKeys),
		sem:     make(chan struct{}, maxInFlight),
		hosts:   map[string]struct{}{},
	}
	a.userHash = sha256.Sum256([]byte(cfg.user))
	a.passHash = sha256.Sum256([]byte(cfg.password))
	for _, h := range cfg.allowedHosts {
		a.hosts[strings.ToLower(h)] = struct{}{}
	}
	return a, nil
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /api/state", a.authed(http.HandlerFunc(a.apiState)))
	mux.Handle("GET /api/history", a.authed(http.HandlerFunc(a.apiHistory)))
	mux.Handle("GET /", a.authed(http.HandlerFunc(a.serveStatic)))
	return a.recoverer(a.securityHeaders(a.hostCheck(a.limit(mux))))
}

func (a *app) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic serving %s: %v", r.URL.Path, v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *app) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if a.cfg.tlsCert != "" {
			h.Set("Strict-Transport-Security", "max-age=15552000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly extracts the lower-cased host name from a Host header value.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	return strings.ToLower(h)
}

// hostCheck rejects requests whose Host header is not allow-listed. It stops
// DNS-rebinding attacks against an unauthenticated loopback instance.
func (a *app) hostCheck(next http.Handler) http.Handler {
	if len(a.hosts) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.hosts[hostOnly(r.Host)]; !ok {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limit caps concurrent requests so a flood cannot exhaust a small machine.
func (a *app) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case a.sem <- struct{}{}:
			defer func() { <-a.sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
		}
	})
}

func (a *app) checkCreds(user, pass string) bool {
	uh := sha256.Sum256([]byte(user))
	ph := sha256.Sum256([]byte(pass))
	// Both comparisons always run; hashing first makes the compare length-independent.
	u := subtle.ConstantTimeCompare(uh[:], a.userHash[:])
	p := subtle.ConstantTimeCompare(ph[:], a.passHash[:])
	return u&p == 1
}

func (a *app) authed(next http.Handler) http.Handler {
	if a.cfg.noAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r)
		if wait := a.limiter.blocked(key); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="podmon", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		// begin/finish make the check-then-count sequence atomic enough that a
		// burst of parallel guesses cannot all be evaluated before a lockout
		// becomes visible: at most maxAttemptsInFlight are evaluated at once.
		if wait, admitted := a.limiter.begin(key); !admitted {
			if wait <= 0 {
				wait = time.Second
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
		good := a.checkCreds(user, pass)
		if locked := a.limiter.finish(key, good); locked {
			log.Printf("auth: locking out %s for %s after repeated failures", key, authLockout)
		}
		if !good {
			w.Header().Set("WWW-Authenticate", `Basic realm="podmon", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey identifies the peer for rate limiting. IPv6 peers are grouped by
// /64 so one host cannot rotate through its whole prefix.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

type failEntry struct {
	count       int // recorded failures in the current window
	inflight    int // credentialed attempts currently being evaluated
	first       time.Time
	lockedUntil time.Time
}

// failLimiter locks a peer out after repeated failed logins. The table is
// bounded so it cannot be used to exhaust memory, and active lockouts are the
// last entries to be evicted.
type failLimiter struct {
	mu      sync.Mutex
	m       map[string]*failEntry
	max     int
	window  time.Duration
	lockout time.Duration
	capKeys int
	now     func() time.Time
}

// maxAttemptsInFlight bounds parallel guesses from one peer. It is well above
// the handful of parallel requests a browser makes while loading the page.
const maxAttemptsInFlight = 16

func newFailLimiter(max int, window, lockout time.Duration, capKeys int) *failLimiter {
	return &failLimiter{m: map[string]*failEntry{}, max: max, window: window, lockout: lockout, capKeys: capKeys, now: time.Now}
}

func (l *failLimiter) blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.m[key]; ok {
		if d := e.lockedUntil.Sub(l.now()); d > 0 {
			return d
		}
	}
	return 0
}

// begin admits one credentialed attempt, or refuses it because the peer is
// locked out or already has too many attempts in flight.
func (l *failLimiter) begin(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.capKeys {
			l.purge(now)
		}
		e = &failEntry{first: now}
		l.m[key] = e
	}
	if d := e.lockedUntil.Sub(now); d > 0 {
		return d, false
	}
	if e.inflight >= maxAttemptsInFlight {
		return 0, false
	}
	e.inflight++
	return 0, true
}

// finish records the outcome of an admitted attempt and reports whether it
// triggered a lockout.
func (l *failLimiter) finish(key string, good bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.m[key]
	if !ok {
		return false
	}
	if e.inflight > 0 {
		e.inflight--
	}
	locked := false
	if good {
		e.count = 0
	} else {
		if now.Sub(e.first) > l.window {
			e.count, e.first = 0, now
		}
		e.count++
		if e.count >= l.max {
			e.lockedUntil = now.Add(l.lockout)
			e.count = 0
			e.first = now
			locked = true
		}
	}
	if e.count == 0 && e.inflight == 0 && !now.Before(e.lockedUntil) {
		delete(l.m, key)
	}
	return locked
}

// purge makes room for a new key. Expired entries go first, then entries that
// are not locked; an active lockout is evicted only when the table holds
// nothing else, and then the one expiring soonest.
func (l *failLimiter) purge(now time.Time) {
	for k, e := range l.m {
		if e.inflight == 0 && !now.Before(e.lockedUntil) && now.Sub(e.first) > l.window {
			delete(l.m, k)
		}
	}
	for k, e := range l.m {
		if len(l.m) < l.capKeys {
			return
		}
		if e.inflight == 0 && !now.Before(e.lockedUntil) {
			delete(l.m, k)
		}
	}
	if len(l.m) < l.capKeys {
		return
	}
	var oldest string
	var oldestAt time.Time
	for k, e := range l.m {
		if oldest == "" || e.lockedUntil.Before(oldestAt) {
			oldest, oldestAt = k, e.lockedUntil
		}
	}
	delete(l.m, oldest)
}

type stateResponse struct {
	ServerTime int64          `json:"serverTime"`
	System     *System        `json:"system"`
	Podman     PodmanSnapshot `json:"podman"`
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("json: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}

func (a *app) apiState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, stateResponse{
		ServerTime: time.Now().UnixMilli(),
		System:     a.sampler.Latest(),
		Podman:     a.pm.Snapshot(),
	})
}

func (a *app) apiHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.sampler.History())
}

func (a *app) serveStatic(w http.ResponseWriter, r *http.Request) {
	f, ok := a.static[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", a.started, bytes.NewReader(f.data))
}
