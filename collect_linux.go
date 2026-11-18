//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type cpuTimes struct{ busy, total uint64 }
type netCounters struct{ rx, tx uint64 }
type ioCounters struct{ readSectors, writeSectors uint64 }
type mountEntry struct{ device, mount, fstype string }

// Only local, block-backed filesystems are reported. Network filesystems are
// excluded on purpose: statfs on a hung mount would block the sampler.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "f2fs": true,
	"zfs": true, "fuseblk": true,
}

// Virtual interfaces created by Podman/Docker/libvirt are skipped so network
// totals reflect real traffic instead of double-counting container bridges.
var hiddenIfacePrefixes = []string{"veth", "cni-", "podman", "br-", "docker", "virbr", "vnet"}

func includeIface(name string) bool {
	if name == "lo" || name == "" {
		return false
	}
	for _, p := range hiddenIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// netIfaceDetails reads interface details via net.Interfaces().
// Returns a map of interface name -> (type, IPs, MAC, state).
func netIfaceDetails() map[string]struct {
	ifaceType string
	ips       []string
	mac       string
	state     string
} {
	result := make(map[string]struct {
		ifaceType string
		ips       []string
		mac       string
		state     string
	})
	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, iface := range ifaces {
		if !includeIface(iface.Name) {
			continue
		}
		// Get IPs
		addrs, err := iface.Addrs()
		var ips []string
		if err == nil {
			for _, a := range addrs {
				ips = append(ips, a.String())
			}
		}
		// Get MAC
		mac := iface.HardwareAddr.String()
		// Get state
		state := "down"
		if iface.Flags&net.FlagUp != 0 {
			state = "up"
		}
		// Determine type: Wi-Fi if wireless or name starts with wl
		ifaceType := "LAN"
		// Check for wireless in sysfs
		wirelessPath := filepath.Join("/sys/class/net", iface.Name, "wireless")
		if _, err := os.Stat(wirelessPath); err == nil {
			ifaceType = "Wi-Fi"
		} else if strings.HasPrefix(iface.Name, "wl") {
			ifaceType = "Wi-Fi"
		}
		result[iface.Name] = struct {
			ifaceType string
			ips       []string
			mac       string
			state     string
		}{
			ifaceType: ifaceType,
			ips:       ips,
			mac:       mac,
			state:     state,
		}
	}
	return result
}

func parseCPUStat(b []byte) (all cpuTimes, cores []cpuTimes) {
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var v [10]uint64
		for i := 1; i < len(f) && i <= 10; i++ {
			v[i-1], _ = strconv.ParseUint(f[i], 10, 64)
		}
		// user nice system idle iowait irq softirq steal (guest is already in user)
		idle := v[3] + v[4]
		total := v[0] + v[1] + v[2] + v[3] + v[4] + v[5] + v[6] + v[7]
		t := cpuTimes{total: total}
		if total > idle {
			t.busy = total - idle
		}
		if f[0] == "cpu" {
			all = t
		} else {
			cores = append(cores, t)
		}
	}
	return all, cores
}

func cpuPercent(prev, cur cpuTimes) float64 {
	dt := delta(cur.total, prev.total)
	if dt == 0 {
		return 0
	}
	return clampPct(float64(delta(cur.busy, prev.busy)) / float64(dt) * 100)
}

func parseMemInfo(b []byte) MemStats {
	kv := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		kv[key] = v
	}
	total := kv["MemTotal"]
	avail, ok := kv["MemAvailable"]
	if !ok {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	if avail > total {
		avail = total
	}
	m := MemStats{
		Total:     total,
		Available: avail,
		Used:      total - avail,
		Cached:    kv["Cached"] + kv["SReclaimable"],
		Buffers:   kv["Buffers"],
		SwapTotal: kv["SwapTotal"],
	}
	if total > 0 {
		m.Percent = round1(clampPct(float64(m.Used) / float64(total) * 100))
	}
	if st, sf := kv["SwapTotal"], kv["SwapFree"]; st > 0 {
		if sf > st {
			sf = st
		}
		m.SwapUsed = st - sf
		m.SwapPercent = round1(clampPct(float64(m.SwapUsed) / float64(st) * 100))
	}
	return m
}

func parseNetDev(b []byte) map[string]netCounters {
	out := map[string]netCounters{}
	for i, line := range strings.Split(string(b), "\n") {
		if i < 2 { // two header lines
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[strings.TrimSpace(name)] = netCounters{rx: rx, tx: tx}
	}
	return out
}

func parseDiskStats(b []byte, whole func(string) bool) map[string]ioCounters {
	out := map[string]ioCounters{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !whole(f[2]) {
			continue
		}
		rs, _ := strconv.ParseUint(f[5], 10, 64)
		ws, _ := strconv.ParseUint(f[9], 10, 64)
		out[f[2]] = ioCounters{readSectors: rs, writeSectors: ws}
	}
	return out
}

func safeDevName(n string) bool {
	if n == "" || len(n) > 64 || n == "." || n == ".." {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '!', r == '.':
		default:
			return false
		}
	}
	return true
}

func isWholeDisk(name string) bool {
	if !safeDevName(name) {
		return false
	}
	for _, p := range []string{"loop", "ram", "zram", "dm-", "sr", "fd", "md", "nbd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	fi, err := os.Stat("/sys/block/" + name)
	return err == nil && fi.IsDir()
}

// unescapeMount decodes the octal escapes (\040 for space, etc.) used in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseMounts(b []byte) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		out = append(out, mountEntry{device: unescapeMount(f[0]), mount: unescapeMount(f[1]), fstype: f[2]})
	}
	return out
}

var (
	statfsMu   sync.Mutex
	statfsBusy = map[string]struct{}{}
)

// statfsWithTimeout runs statfs off the sampling goroutine. A wedged mount
// (a stuck FUSE filesystem, say) is skipped instead of freezing the dashboard
// or delaying start-up, and at most one call per path is ever outstanding.
func statfsWithTimeout(path string, d time.Duration) (syscall.Statfs_t, bool) {
	var zero syscall.Statfs_t
	statfsMu.Lock()
	if _, busy := statfsBusy[path]; busy {
		statfsMu.Unlock()
		return zero, false
	}
	statfsBusy[path] = struct{}{}
	statfsMu.Unlock()

	type result struct {
		st  syscall.Statfs_t
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var st syscall.Statfs_t
		err := syscall.Statfs(path, &st)
		statfsMu.Lock()
		delete(statfsBusy, path)
		statfsMu.Unlock()
		ch <- result{st, err}
	}()
	select {
	case r := <-ch:
		return r.st, r.err == nil
	case <-time.After(d):
		return zero, false
	}
}

func readDisks() []DiskUsage {
	b, err := readSmall("/proc/mounts", 1<<20)
	if err != nil {
		return []DiskUsage{}
	}
	best := map[string]mountEntry{} // one entry per device: shortest mount point wins
	for _, m := range parseMounts(b) {
		if cur, ok := best[m.device]; !ok || len(m.mount) < len(cur.mount) {
			best[m.device] = m
		}
	}
	out := make([]DiskUsage, 0, len(best))
	for _, m := range best {
		st, ok := statfsWithTimeout(m.mount, 2*time.Second)
		if !ok {
			continue
		}
		bs := uint64(st.Bsize)
		total := st.Blocks * bs
		var used uint64
		if st.Blocks > st.Bfree {
			used = (st.Blocks - st.Bfree) * bs
		}
		avail := st.Bavail * bs
		d := DiskUsage{
			Mount:  sanitize(m.mount, 128),
			Device: sanitize(m.device, 128),
			FSType: sanitize(m.fstype, 16),
			Total:  total,
			Used:   used,
			Free:   avail,
		}
		if used+avail > 0 {
			d.Percent = round1(clampPct(float64(used) / float64(used+avail) * 100))
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func readTemps() []Temp {
	out := []Temp{}
	paths, _ := filepath.Glob("/sys/class/hwmon/hwmon*/temp*_input")
	sort.Strings(paths)
	for _, p := range paths {
		if len(out) >= 16 {
			break
		}
		raw, err := readSmall(p, 32)
		if err != nil {
			continue
		}
		milli, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		c := float64(milli) / 1000
		if c <= -50 || c > 150 {
			continue
		}
		name, _ := readSmall(filepath.Join(filepath.Dir(p), "name"), 64)
		label, _ := readSmall(strings.TrimSuffix(p, "_input")+"_label", 64)
		parts := []string{}
		for _, s := range []string{string(name), string(label)} {
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			parts = append(parts, filepath.Base(p))
		}
		out = append(out, Temp{Label: sanitize(strings.Join(parts, " "), 48), Celsius: round1(c)})
	}
	return out
}

func parseOSRelease(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return sanitize(strings.Trim(strings.TrimSpace(v), `"'`), 64)
		}
	}
	return ""
}

func parseCPUModel(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return sanitize(strings.Join(strings.Fields(v), " "), 96)
		}
	}
	return ""
}

func readSystemInfo() SystemInfo {
	info := SystemInfo{CPUCount: runtime.NumCPU()}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = sanitize(h, 64)
	}
	if b, err := readSmall("/proc/sys/kernel/osrelease", 256); err == nil {
		info.Kernel = sanitize(strings.TrimSpace(string(b)), 64)
	}
	if b, err := readSmall("/etc/os-release", 8192); err == nil {
		info.OS = parseOSRelease(b)
	}
	if b, err := readSmall("/proc/cpuinfo", 1<<20); err == nil {
		info.CPUModel = parseCPUModel(b)
	}
	return info
}

type collector struct {
	info      SystemInfo
	prevT     time.Time
	prevCPU   cpuTimes
	prevCores []cpuTimes
	prevNet   map[string]netCounters
	prevDisk  map[string]ioCounters
}

func newCollector() *collector { return &collector{info: readSystemInfo()} }

func (c *collector) collect(now time.Time) *System {
	dt := 0.0
	havePrev := !c.prevT.IsZero()
	if havePrev {
		dt = now.Sub(c.prevT).Seconds()
	}
	sys := &System{
		Time:   now.UnixMilli(),
		Info:   c.info,
		CPU:    CPUStats{Cores: []float64{}},
		Disks:  []DiskUsage{},
		DiskIO: []DiskIO{},
		Net:    []NetIface{},
		Temps:  []Temp{},
	}

	if b, err := readSmall("/proc/stat", 1<<20); err == nil {
		all, cores := parseCPUStat(b)
		if havePrev {
			sys.CPU.Percent = round1(cpuPercent(c.prevCPU, all))
			for i, cur := range cores {
				p := 0.0
				if i < len(c.prevCores) {
					p = cpuPercent(c.prevCores[i], cur)
				}
				sys.CPU.Cores = append(sys.CPU.Cores, round1(p))
			}
		}
		c.prevCPU, c.prevCores = all, cores
	}

	if b, err := readSmall("/proc/meminfo", 1<<16); err == nil {
		sys.Mem = parseMemInfo(b)
	}
	if b, err := readSmall("/proc/loadavg", 256); err == nil {
		f := strings.Fields(string(b))
		for i := 0; i < 3 && i < len(f); i++ {
			v, _ := strconv.ParseFloat(f[i], 64)
			sys.Load[i] = round2(v)
		}
	}
	if b, err := readSmall("/proc/uptime", 128); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			sys.Uptime = safeFloat(v)
		}
	}

	if b, err := readSmall("/proc/net/dev", 1<<18); err == nil {
		cur := parseNetDev(b)
		details := netIfaceDetails()
		for name, n := range cur {
			if !includeIface(name) {
				continue
			}
			ni := NetIface{
				Name:     sanitize(name, 32),
				RxTotal:  n.rx,
				TxTotal:  n.tx,
				Type:     "LAN",
				IPs:      []string{},
				MAC:      "",
				State:    "down",
			}
			if d, ok := details[name]; ok {
				ni.Type = d.ifaceType
				ni.IPs = d.ips
				ni.MAC = d.mac
				ni.State = d.state
			}
			if p, ok := c.prevNet[name]; ok && dt > 0 {
				ni.RxBps = round1(float64(delta(n.rx, p.rx)) / dt)
				ni.TxBps = round1(float64(delta(n.tx, p.tx)) / dt)
			}
			sys.Net = append(sys.Net, ni)
		}
		sort.Slice(sys.Net, func(i, j int) bool { return sys.Net[i].Name < sys.Net[j].Name })
		c.prevNet = cur
	}

	if b, err := readSmall("/proc/diskstats", 1<<18); err == nil {
		cur := parseDiskStats(b, isWholeDisk)
		for name, n := range cur {
			d := DiskIO{Name: sanitize(name, 32)}
			if p, ok := c.prevDisk[name]; ok && dt > 0 {
				d.ReadBps = round1(float64(delta(n.readSectors, p.readSectors)) * 512 / dt)
				d.WriteBps = round1(float64(delta(n.writeSectors, p.writeSectors)) * 512 / dt)
			}
			sys.DiskIO = append(sys.DiskIO, d)
		}
		sort.Slice(sys.DiskIO, func(i, j int) bool { return sys.DiskIO[i].Name < sys.DiskIO[j].Name })
		c.prevDisk = cur
	}

	sys.Disks = readDisks()
	sys.Temps = readTemps()
	c.prevT = now
	return sys
}
