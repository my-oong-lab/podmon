package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestParseCPUStat(t *testing.T) {
	in := []byte("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0 0 0\ncpu1 50 0 50 350 50 0 0 0 0 0\nintr 1\n")
	all, cores := parseCPUStat(in)
	if all.total != 1000 || all.busy != 200 {
		t.Fatalf("all = %+v", all)
	}
	if len(cores) != 2 {
		t.Fatalf("cores = %d", len(cores))
	}
	next := cpuTimes{busy: 700, total: 1500}
	if got := cpuPercent(all, next); got != 100*500.0/500.0 {
		t.Fatalf("cpuPercent = %v", got)
	}
	if got := cpuPercent(next, all); got != 0 { // counter went backwards
		t.Fatalf("backwards counters should give 0, got %v", got)
	}
}

func TestParseMemInfo(t *testing.T) {
	in := []byte("MemTotal: 4000 kB\nMemFree: 100 kB\nMemAvailable: 1000 kB\nBuffers: 10 kB\nCached: 200 kB\nSwapTotal: 2000 kB\nSwapFree: 500 kB\n")
	m := parseMemInfo(in)
	if m.Total != 4000*1024 || m.Used != 3000*1024 || m.Percent != 75 {
		t.Fatalf("mem = %+v", m)
	}
	if m.SwapUsed != 1500*1024 || m.SwapPercent != 75 {
		t.Fatalf("swap = %+v", m)
	}
	// MemAvailable larger than MemTotal must not underflow.
	m = parseMemInfo([]byte("MemTotal: 100 kB\nMemAvailable: 900 kB\n"))
	if m.Used != 0 || m.Percent != 0 {
		t.Fatalf("clamp failed: %+v", m)
	}
}

func TestParseNetDevAndFilter(t *testing.T) {
	in := []byte("Inter-|   Receive\n face |bytes\n  eth0: 1000 1 0 0 0 0 0 0 2000 1 0 0 0 0 0 0\n  lo: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n")
	m := parseNetDev(in)
	if m["eth0"].rx != 1000 || m["eth0"].tx != 2000 {
		t.Fatalf("eth0 = %+v", m["eth0"])
	}
	for name, want := range map[string]bool{"eth0": true, "enp3s0": true, "lo": false, "veth123": false, "podman0": false, "cni-podman0": false, "br-abc": false} {
		if includeIface(name) != want {
			t.Errorf("includeIface(%q) != %v", name, want)
		}
	}
}

func TestParseMounts(t *testing.T) {
	in := []byte("/dev/sda2 / ext4 rw 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n/dev/sdb1 /mnt/my\\040disk ext4 rw 0 0\nserver:/x /nfs nfs rw 0 0\n")
	got := parseMounts(in)
	if len(got) != 2 || got[0].mount != "/" || got[1].mount != "/mnt/my disk" {
		t.Fatalf("mounts = %+v", got)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("a\x00b\nc‮d", 50); got != "a b cd" {
		t.Fatalf("control/bidi not stripped: %q", got)
	}
	if got := sanitize(strings.Repeat("x", 500), 10); len(got) != 10 {
		t.Fatalf("not truncated: %d", len(got))
	}
	if got := sanitize("ok\xff", 10); !strings.HasSuffix(got, "�") {
		t.Fatalf("invalid utf8 not replaced: %q", got)
	}
}

func TestUnixTime(t *testing.T) {
	var c apiContainer
	for _, in := range []string{`{"Created": 1700000000}`, `{"Created": "2023-11-14T22:13:20Z"}`, `{"Created": null}`, `{"Created": "garbage"}`} {
		if err := json.Unmarshal([]byte(in), &c); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
	}
}

func TestToContainer(t *testing.T) {
	a := apiContainer{ID: strings.Repeat("a", 64), Names: []string{"/web"}, Image: "img", State: "RUNNING",
		Ports: []apiPort{{HostIP: "", HostPort: 8080, ContainerPort: 80, Range: 1, Protocol: "tcp"}}}
	c := toContainer(a, nil)
	if c.Name != "web" || c.ID != strings.Repeat("a", 12) || c.State != "running" {
		t.Fatalf("container = %+v", c)
	}
	if len(c.Ports) != 1 || c.Ports[0] != "0.0.0.0:8080->80/tcp" {
		t.Fatalf("ports = %v", c.Ports)
	}
}

func TestRunningIDsRejectsBadIDs(t *testing.T) {
	list := []apiContainer{
		{ID: strings.Repeat("a", 64), State: "running"},
		{ID: "../../etc/passwd", State: "running"},
		{ID: strings.Repeat("b", 64), State: "exited"},
		{ID: strings.Repeat("A", 64), State: "running"},
	}
	ids := runningIDs(list)
	if len(ids) != 1 || ids[0] != strings.Repeat("a", 64) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := map[string]string{"PODMON_PASSWORD": "a-long-enough-password"}

	if _, err := loadConfig(nil, env(good)); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
	if _, err := loadConfig(nil, env(nil)); err == nil {
		t.Fatal("missing password must be rejected")
	}
	if _, err := loadConfig(nil, env(map[string]string{"PODMON_PASSWORD": "short"})); err == nil {
		t.Fatal("short password must be rejected")
	}
	if _, err := loadConfig([]string{"-no-auth", "-listen", "0.0.0.0:8080"}, env(nil)); err == nil {
		t.Fatal("-no-auth on a public address must be rejected")
	}
	if _, err := loadConfig([]string{"-no-auth", "-listen", "127.0.0.1:8080"}, env(nil)); err != nil {
		t.Fatalf("-no-auth on loopback should work: %v", err)
	}
	if _, err := loadConfig([]string{"-tls-cert", "x"}, env(good)); err == nil {
		t.Fatal("cert without key must be rejected")
	}
	if _, err := loadConfig([]string{"-podman-bin", "podman"}, env(good)); err == nil {
		t.Fatal("relative podman binary must be rejected")
	}
	if _, err := loadConfig([]string{"-user", "a:b"}, env(good)); err == nil {
		t.Fatal("colon in user must be rejected")
	}
}

func testApp(t *testing.T) *app {
	t.Helper()
	cfg := &config{user: "admin", password: "correct-horse-battery", interval: time.Second}
	assets := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>hi")}, "app.js": {Data: []byte("1")}}
	a, err := newApp(cfg, &sampler{interval: time.Second, latest: &System{}}, newPodman("", ""), assets)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func do(a *app, method, target, user, pass, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	if host != "" {
		r.Host = host
	}
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w
}

func TestAuthAndRouting(t *testing.T) {
	a := testApp(t)
	if w := do(a, "GET", "/", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no creds: %d", w.Code)
	}
	if w := do(a, "GET", "/api/state", "admin", "nope", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad creds: %d", w.Code)
	}
	if w := do(a, "GET", "/api/state", "admin", "correct-horse-battery", ""); w.Code != 200 {
		t.Fatalf("good creds: %d", w.Code)
	}
	if w := do(a, "GET", "/healthz", "", "", ""); w.Code != 200 {
		t.Fatalf("healthz: %d", w.Code)
	}
	if w := do(a, "POST", "/api/state", "admin", "correct-horse-battery", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	for _, p := range []string{"/../main.go", "/..%2fmain.go", "/web/../go.mod", "/nope"} {
		if w := do(a, "GET", p, "admin", "correct-horse-battery", ""); w.Code == 200 && strings.Contains(w.Body.String(), "package main") {
			t.Fatalf("path traversal served source for %q", p)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := do(testApp(t), "GET", "/", "admin", "correct-horse-battery", "")
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("CSP must forbid framing")
	}
}

func TestHostCheck(t *testing.T) {
	a := testApp(t)
	a.hosts = map[string]struct{}{"localhost": {}}
	a.cfg.noAuth = true
	if w := do(a, "GET", "/healthz", "", "", "evil.example:8080"); w.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", w.Code)
	}
	if w := do(a, "GET", "/healthz", "", "", "localhost:8080"); w.Code != 200 {
		t.Fatalf("allowed host: %d", w.Code)
	}
}

func TestLockout(t *testing.T) {
	a := testApp(t)
	codes := []int{}
	for i := 0; i < authMaxFails+1; i++ {
		codes = append(codes, do(a, "GET", "/api/state", "admin", "bad", "").Code)
	}
	if codes[len(codes)-1] != http.StatusTooManyRequests {
		t.Fatalf("expected lockout, got %v", codes)
	}
	// Even the right password is refused while locked out.
	if w := do(a, "GET", "/api/state", "admin", "correct-horse-battery", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out peer was let in: %d", w.Code)
	}
}

func TestLimiterBounded(t *testing.T) {
	l := newFailLimiter(3, time.Minute, time.Minute, 8)
	for i := 0; i < 100; i++ {
		key := "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, ok := l.begin(key); ok {
			l.finish(key, false)
		}
	}
	if len(l.m) > 8 {
		t.Fatalf("limiter table grew to %d", len(l.m))
	}
}

func TestLimiterKeepsActiveLockouts(t *testing.T) {
	l := newFailLimiter(3, time.Minute, time.Hour, 4)
	locked := false
	for i := 0; i < 3; i++ {
		if _, ok := l.begin("victim"); !ok {
			t.Fatal("attempt refused before lockout")
		}
		locked = l.finish("victim", false)
	}
	if !locked {
		t.Fatal("expected a lockout after three failures")
	}
	for i := 0; i < 50; i++ { // churn many one-failure keys through a full table
		key := "other" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, ok := l.begin(key); ok {
			l.finish(key, false)
		}
	}
	if l.blocked("victim") <= 0 {
		t.Fatal("key churn evicted an active lockout")
	}
}

func TestParallelGuessesAreBounded(t *testing.T) {
	a := testApp(t)
	const n = 64
	start := make(chan struct{})
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			codes <- do(a, "GET", "/api/state", "admin", "wrong", "").Code
		}()
	}
	close(start)
	evaluated, refused := 0, 0
	for i := 0; i < n; i++ {
		switch <-codes {
		case http.StatusUnauthorized:
			evaluated++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	if evaluated > authMaxFails+maxAttemptsInFlight {
		t.Fatalf("%d parallel guesses were evaluated, want at most %d", evaluated, authMaxFails+maxAttemptsInFlight)
	}
	if refused == 0 {
		t.Fatal("no parallel guess was refused")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	a := testApp(t)
	for i := 0; i < authMaxFails-1; i++ {
		do(a, "GET", "/api/state", "admin", "wrong", "")
	}
	if w := do(a, "GET", "/api/state", "admin", "correct-horse-battery", ""); w.Code != 200 {
		t.Fatalf("good login after failures: %d", w.Code)
	}
	for i := 0; i < authMaxFails-1; i++ { // the counter restarted, so these do not lock out
		do(a, "GET", "/api/state", "admin", "wrong", "")
	}
	if w := do(a, "GET", "/api/state", "admin", "correct-horse-battery", ""); w.Code != 200 {
		t.Fatalf("failures were not reset by a successful login: %d", w.Code)
	}
}

func TestClientKeyGroupsIPv6(t *testing.T) {
	a := httptest.NewRequest("GET", "/", nil)
	a.RemoteAddr = "[2001:db8:1:2:aaaa::1]:1"
	b := httptest.NewRequest("GET", "/", nil)
	b.RemoteAddr = "[2001:db8:1:2:bbbb::9]:2"
	if clientKey(a) != clientKey(b) {
		t.Fatal("addresses in one /64 must share a key")
	}
}

func TestSafeDevName(t *testing.T) {
	for n, want := range map[string]bool{"sda": true, "nvme0n1": true, "cciss!c0d0": true, ".": false, "..": false, "": false, "a/b": false} {
		if safeDevName(n) != want {
			t.Errorf("safeDevName(%q) != %v", n, want)
		}
	}
}
