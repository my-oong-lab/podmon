package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	podmanRefreshEvery = 5 * time.Second
	podmanIdleAfter    = 60 * time.Second // stop polling Podman when nobody is looking
	podmanStaleAfter   = 15 * time.Second
	maxAPIBody         = 8 << 20
	maxContainers      = 200
)

// Container is the subset of Podman's container data shown in the dashboard.
// Command lines, environment, labels and mounts are deliberately omitted:
// they commonly contain secrets.
type Container struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	State      string   `json:"state"`
	Status     string   `json:"status"`
	Pod        string   `json:"pod"`
	Infra      bool     `json:"infra"`
	Created    int64    `json:"created"`
	StartedAt  int64    `json:"startedAt"`
	Ports      []string `json:"ports"`
	HasStats   bool     `json:"hasStats"`
	CPU        float64  `json:"cpu"`
	MemUsage   uint64   `json:"memUsage"`
	MemLimit   uint64   `json:"memLimit"`
	MemPercent float64  `json:"memPercent"`
	NetIn      uint64   `json:"netIn"`
	NetOut     uint64   `json:"netOut"`
	BlockIn    uint64   `json:"blockIn"`
	BlockOut   uint64   `json:"blockOut"`
	PIDs       uint64   `json:"pids"`
}

type PodmanSnapshot struct {
	Available  bool        `json:"available"`
	Source     string      `json:"source"` // "socket" or "cli"
	Error      string      `json:"error,omitempty"`
	UpdatedAt  int64       `json:"updatedAt"` // unix milliseconds
	HasStats   bool        `json:"hasStats"`
	Running    int         `json:"running"`
	Total      int         `json:"total"`
	Containers []Container `json:"containers"`
}

// unixTime accepts Podman's differing encodings of a timestamp: a unix
// number or an RFC 3339 string.
type unixTime int64

func (u *unixTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	*u = 0
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return nil
		}
		if t, err := time.Parse(time.RFC3339Nano, str); err == nil && t.Unix() > 0 {
			*u = unixTime(t.Unix())
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil && f > 0 {
		*u = unixTime(int64(f))
	}
	return nil
}

type apiPort struct {
	HostIP        string `json:"host_ip"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Range         int    `json:"range"`
	Protocol      string `json:"protocol"`
}

type apiContainer struct {
	ID        string    `json:"Id"`
	Names     []string  `json:"Names"`
	Image     string    `json:"Image"`
	State     string    `json:"State"`
	Status    string    `json:"Status"`
	Pod       string    `json:"Pod"`
	PodName   string    `json:"PodName"`
	IsInfra   bool      `json:"IsInfra"`
	Created   unixTime  `json:"Created"`
	StartedAt unixTime  `json:"StartedAt"`
	Ports     []apiPort `json:"Ports"`
}

type apiStat struct {
	ContainerID string  `json:"ContainerID"`
	Name        string  `json:"Name"`
	CPU         float64 `json:"CPU"`
	MemUsage    uint64  `json:"MemUsage"`
	MemLimit    uint64  `json:"MemLimit"`
	MemPerc     float64 `json:"MemPerc"`
	NetInput    uint64  `json:"NetInput"`
	NetOutput   uint64  `json:"NetOutput"`
	BlockInput  uint64  `json:"BlockInput"`
	BlockOutput uint64  `json:"BlockOutput"`
	PIDs        uint64  `json:"PIDs"`
}

type podmanClient struct {
	socket string // explicit socket path, or "" to auto-detect
	bin    string // podman binary for the CLI fallback, or ""

	mu         sync.RWMutex
	snap       PodmanSnapshot
	lastErr    string
	lastAccess atomic.Int64
	wake       chan struct{}
}

func newPodman(socket, bin string) *podmanClient {
	if bin == "" {
		if p, err := exec.LookPath("podman"); err == nil && filepath.IsAbs(p) {
			bin = p
		}
	}
	return &podmanClient{
		socket: socket,
		bin:    bin,
		wake:   make(chan struct{}, 1),
		snap:   PodmanSnapshot{Containers: []Container{}},
	}
}

func (p *podmanClient) run(ctx context.Context) {
	p.refresh(ctx)
	t := time.NewTicker(podmanRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.recentlyAccessed() {
				p.refresh(ctx)
			}
		case <-p.wake:
			p.refresh(ctx)
		}
	}
}

func (p *podmanClient) recentlyAccessed() bool {
	return time.Since(time.Unix(0, p.lastAccess.Load())) < podmanIdleAfter
}

// Snapshot returns the cached container list. Calling it marks the dashboard
// as being watched, which keeps the background refresh running.
func (p *podmanClient) Snapshot() PodmanSnapshot {
	p.lastAccess.Store(time.Now().UnixNano())
	p.mu.RLock()
	s := p.snap
	p.mu.RUnlock()
	if time.Since(time.UnixMilli(s.UpdatedAt)) > podmanStaleAfter {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	return s
}

func (p *podmanClient) candidates() []string {
	if p.socket != "" {
		return []string{p.socket}
	}
	var c []string
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" && filepath.IsAbs(x) {
		c = append(c, filepath.Join(x, "podman", "podman.sock"))
	}
	if euid := os.Geteuid(); euid == 0 {
		c = append(c, "/run/podman/podman.sock")
	} else {
		c = append(c, fmt.Sprintf("/run/user/%d/podman/podman.sock", euid))
	}
	return c
}

// validSocket accepts only a real unix socket owned by root or by the user
// this process runs as.
func validSocket(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == 0 || st.Uid == uint32(os.Geteuid())
}

func (p *podmanClient) findSocket() string {
	for _, c := range p.candidates() {
		if validSocket(c) {
			return c
		}
	}
	return ""
}

func newSocketClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
}

// podmanAPIBase is the minimum API version prefix accepted by Podman 4.x and 5.x.
const podmanAPIBase = "/v4.0.0"

// getJSON performs a GET against a fixed Podman API path and decodes at most
// maxAPIBody bytes of JSON.
func getJSON(ctx context.Context, hc *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman"+podmanAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("podman api returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxAPIBody {
		return errors.New("podman api response too large")
	}
	return json.Unmarshal(body, out)
}

func validID(id string) bool {
	if len(id) < 12 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func runningIDs(list []apiContainer) []string {
	var ids []string
	for _, c := range list {
		if len(ids) >= maxContainers {
			break
		}
		if c.State == "running" && validID(c.ID) {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func (p *podmanClient) viaSocket(ctx context.Context, sock string) ([]apiContainer, map[string]apiStat, error) {
	hc := newSocketClient(sock)
	defer hc.CloseIdleConnections()
	var list []apiContainer
	if err := getJSON(ctx, hc, "/libpod/containers/json?all=true", &list); err != nil {
		return nil, nil, err
	}
	stats := map[string]apiStat{}
	if ids := runningIDs(list); len(ids) > 0 {
		q := url.Values{"stream": {"false"}}
		for _, id := range ids {
			q.Add("containers", id)
		}
		var resp struct {
			Stats []apiStat `json:"Stats"`
		}
		if err := getJSON(ctx, hc, "/libpod/containers/stats?"+q.Encode(), &resp); err != nil {
			log.Printf("podman stats unavailable: %v", err)
		} else {
			for _, s := range resp.Stats {
				stats[s.ContainerID] = s
			}
		}
	}
	return list, stats, nil
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(b []byte) (int, error) {
	if l.buf.Len()+len(b) > l.max {
		return 0, errors.New("output too large")
	}
	return l.buf.Write(b)
}

func podmanEnv() []string {
	var env []string
	for _, k := range []string{"HOME", "XDG_RUNTIME_DIR", "PATH", "USER", "LOGNAME", "LANG", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// viaCLI is the fallback when the API socket is not enabled. It runs a fixed
// command line; no request data ever reaches it.
func (p *podmanClient) viaCLI(ctx context.Context) ([]apiContainer, error) {
	cmd := exec.CommandContext(ctx, p.bin, "ps", "--all", "--format", "json")
	cmd.Env = podmanEnv()
	cmd.Dir = "/"
	out := &limitedBuffer{max: maxAPIBody}
	cmd.Stdout = out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	var list []apiContainer
	if b := bytes.TrimSpace(out.buf.Bytes()); len(b) > 0 {
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("podman ps output: %w", err)
		}
	}
	return list, nil
}

func formatPort(p apiPort) string {
	ip := p.HostIP
	if ip == "" {
		ip = "0.0.0.0"
	}
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	rng := func(base int) string {
		if p.Range > 1 {
			return fmt.Sprintf("%d-%d", base, base+p.Range-1)
		}
		return fmt.Sprintf("%d", base)
	}
	if p.HostPort == 0 {
		return sanitize(fmt.Sprintf("%s/%s", rng(p.ContainerPort), proto), 64)
	}
	return sanitize(fmt.Sprintf("%s:%s->%s/%s", ip, rng(p.HostPort), rng(p.ContainerPort), proto), 64)
}

func toContainer(a apiContainer, st *apiStat) Container {
	name := ""
	if len(a.Names) > 0 {
		name = strings.TrimPrefix(a.Names[0], "/")
	}
	if name == "" && len(a.ID) >= 12 {
		name = a.ID[:12]
	}
	id := a.ID
	if len(id) > 12 {
		id = id[:12]
	}
	pod := a.PodName
	c := Container{
		ID:        sanitize(id, 12),
		Name:      sanitize(name, 128),
		Image:     sanitize(a.Image, 200),
		State:     strings.ToLower(sanitize(a.State, 24)),
		Status:    sanitize(a.Status, 64),
		Pod:       sanitize(pod, 128),
		Infra:     a.IsInfra,
		Created:   int64(a.Created),
		StartedAt: int64(a.StartedAt),
		Ports:     []string{},
	}
	for i, p := range a.Ports {
		if i >= 16 {
			break
		}
		c.Ports = append(c.Ports, formatPort(p))
	}
	if st != nil {
		c.HasStats = true
		c.CPU = round1(safeFloat(st.CPU))
		c.MemUsage = st.MemUsage
		c.MemLimit = st.MemLimit
		c.MemPercent = round1(safeFloat(st.MemPerc))
		c.NetIn, c.NetOut = st.NetInput, st.NetOutput
		c.BlockIn, c.BlockOut = st.BlockInput, st.BlockOutput
		c.PIDs = st.PIDs
	}
	return c
}

func (p *podmanClient) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	snap := PodmanSnapshot{UpdatedAt: time.Now().UnixMilli(), Containers: []Container{}}
	sock := p.findSocket()
	var list []apiContainer
	var stats map[string]apiStat
	var err error
	source := ""

	if sock != "" {
		if list, stats, err = p.viaSocket(ctx, sock); err == nil {
			source = "socket"
		}
	}
	if source == "" && p.bin != "" {
		l, cerr := p.viaCLI(ctx)
		if cerr == nil {
			list, source, err = l, "cli", nil
		} else if err == nil {
			err = cerr
		}
	}
	if source == "" {
		if sock == "" && p.bin == "" {
			snap.Error = "Podman not found. Enable its API socket with: systemctl --user enable --now podman.socket"
			err = errors.New(snap.Error)
		} else {
			snap.Error = "Could not read containers from Podman. See the podmon log for details."
		}
		p.store(snap, err)
		return
	}

	snap.Available = true
	snap.Source = source
	snap.HasStats = source == "socket"
	for i, a := range list {
		if i >= maxContainers {
			break
		}
		var st *apiStat
		if s, ok := stats[a.ID]; ok {
			st = &s
		}
		c := toContainer(a, st)
		snap.Containers = append(snap.Containers, c)
		if !c.Infra {
			snap.Total++
			if c.State == "running" {
				snap.Running++
			}
		}
	}
	sort.SliceStable(snap.Containers, func(i, j int) bool {
		a, b := snap.Containers[i], snap.Containers[j]
		if a.Infra != b.Infra {
			return !a.Infra
		}
		if (a.State == "running") != (b.State == "running") {
			return a.State == "running"
		}
		return a.Name < b.Name
	})
	p.store(snap, nil)
}

func (p *podmanClient) store(s PodmanSnapshot, err error) {
	p.mu.Lock()
	p.snap = s
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	changed := msg != p.lastErr
	p.lastErr = msg
	p.mu.Unlock()
	if changed && msg != "" {
		log.Printf("podman: %s", msg) // only on change, so a persistent failure does not flood the log
	}
}

// validActions are the allowed container actions. Only socket-backed (not CLI) is supported.
var validActions = map[string]struct{}{
	"start":    {},
	"stop":     {},
	"restart":  {},
	"pause":    {},
	"unpause":  {},
	"kill":     {},
	"remove":   {},
}

// Action performs a container action via the Podman API socket.
// Returns error if socket not available, action invalid, or API fails.
func (p *podmanClient) Action(ctx context.Context, id, action string) error {
	if !validID(id) {
		return errors.New("invalid container ID")
	}
	if _, ok := validActions[action]; !ok {
		return errors.New("invalid action")
	}
	sock := p.findSocket()
	if sock == "" {
		return errors.New("podman socket not available")
	}
	hc := newSocketClient(sock)
	defer hc.CloseIdleConnections()

	path := "/libpod/containers/" + id + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://podman"+podmanAPIBase+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("podman api returned HTTP %d", resp.StatusCode)
	}
	return nil
}
