package main

import (
	"context"
	"sync"
	"time"
)

type SystemInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	CPUModel string `json:"cpuModel"`
	CPUCount int    `json:"cpuCount"`
}

type CPUStats struct {
	Percent float64   `json:"percent"`
	Cores   []float64 `json:"cores"`
}

type MemStats struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Available   uint64  `json:"available"`
	Cached      uint64  `json:"cached"`
	Buffers     uint64  `json:"buffers"`
	Percent     float64 `json:"percent"`
	SwapTotal   uint64  `json:"swapTotal"`
	SwapUsed    uint64  `json:"swapUsed"`
	SwapPercent float64 `json:"swapPercent"`
}

type DiskUsage struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"device"`
	FSType  string  `json:"fsType"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}

type DiskIO struct {
	Name     string  `json:"name"`
	ReadBps  float64 `json:"readBps"`
	WriteBps float64 `json:"writeBps"`
}

type NetIface struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`     // "LAN" or "Wi-Fi"
	IPs     []string `json:"ips"`      // assigned IP addresses
	MAC     string   `json:"mac"`      // hardware address
	State   string   `json:"state"`    // "up" or "down"
	SSID    string   `json:"ssid"`     // Wi-Fi SSID (empty for LAN)
	RxBps   float64  `json:"rxBps"`
	TxBps   float64  `json:"txBps"`
	RxTotal uint64   `json:"rxTotal"`
	TxTotal uint64   `json:"txTotal"`
}

type Temp struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

// System is one immutable snapshot of host metrics. Once published by the
// sampler it is never modified, so it can be shared between requests.
type System struct {
	Time   int64       `json:"time"` // unix milliseconds
	Uptime float64     `json:"uptime"`
	Load   [3]float64  `json:"load"`
	CPU    CPUStats    `json:"cpu"`
	Mem    MemStats    `json:"mem"`
	Disks  []DiskUsage `json:"disks"`
	DiskIO []DiskIO    `json:"diskIo"`
	Net    []NetIface  `json:"net"`
	Temps  []Temp      `json:"temps"`
	Info   SystemInfo  `json:"info"`
}

// History is a columnar view of the recent samples, oldest first.
type History struct {
	Interval float64   `json:"interval"` // seconds between points
	T        []int64   `json:"t"`
	CPU      []float64 `json:"cpu"`
	Mem      []float64 `json:"mem"`
	Load     []float64 `json:"load"`
	NetRx    []float64 `json:"netRx"`
	NetTx    []float64 `json:"netTx"`
	DiskR    []float64 `json:"diskR"`
	DiskW    []float64 `json:"diskW"`
}

type histPoint struct {
	t                                    int64
	cpu, mem, load, netRx, netTx, dR, dW float64
}

const histCap = 300 // 10 minutes at the default 2s interval

// sampler runs a single background loop. HTTP handlers only read the cached
// result, so request volume never changes how much work the host does.
type sampler struct {
	interval time.Duration
	col      *collector

	mu     sync.RWMutex
	latest *System
	ring   [histCap]histPoint
	head   int // next write position
	count  int
}

func newSampler(interval time.Duration) *sampler {
	s := &sampler{interval: interval, col: newCollector()}
	s.sample() // prime counters so the first real sample has valid rates
	return s
}

func (s *sampler) sample() {
	sys := s.col.collect(time.Now())
	pt := histPoint{t: sys.Time, cpu: sys.CPU.Percent, mem: sys.Mem.Percent, load: sys.Load[0]}
	for _, n := range sys.Net {
		pt.netRx += n.RxBps
		pt.netTx += n.TxBps
	}
	for _, d := range sys.DiskIO {
		pt.dR += d.ReadBps
		pt.dW += d.WriteBps
	}
	s.mu.Lock()
	s.latest = sys
	s.ring[s.head] = pt
	s.head = (s.head + 1) % histCap
	if s.count < histCap {
		s.count++
	}
	s.mu.Unlock()
}

func (s *sampler) run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample()
		}
	}
}

func (s *sampler) Latest() *System {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

func (s *sampler) History() History {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := History{
		Interval: s.interval.Seconds(),
		T:        make([]int64, 0, s.count),
		CPU:      make([]float64, 0, s.count),
		Mem:      make([]float64, 0, s.count),
		Load:     make([]float64, 0, s.count),
		NetRx:    make([]float64, 0, s.count),
		NetTx:    make([]float64, 0, s.count),
		DiskR:    make([]float64, 0, s.count),
		DiskW:    make([]float64, 0, s.count),
	}
	start := (s.head - s.count + histCap) % histCap
	for i := 0; i < s.count; i++ {
		p := s.ring[(start+i)%histCap]
		h.T = append(h.T, p.t)
		h.CPU = append(h.CPU, round1(p.cpu))
		h.Mem = append(h.Mem, round1(p.mem))
		h.Load = append(h.Load, round2(p.load))
		h.NetRx = append(h.NetRx, round1(p.netRx))
		h.NetTx = append(h.NetTx, round1(p.netTx))
		h.DiskR = append(h.DiskR, round1(p.dR))
		h.DiskW = append(h.DiskW, round1(p.dW))
	}
	return h
}
