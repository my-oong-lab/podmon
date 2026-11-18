package main

import (
	"io"
	"math"
	"os"
	"strings"
	"unicode"
)

func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func clampPct(v float64) float64 {
	v = safeFloat(v)
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round1(v float64) float64 { return math.Round(safeFloat(v)*10) / 10 }
func round2(v float64) float64 { return math.Round(safeFloat(v)*100) / 100 }

// delta returns cur-prev, treating counter resets/wraps as zero.
func delta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// readSmall reads at most max bytes from a file (procfs/sysfs files report
// size 0, so os.ReadFile-style size hints do not help; the limit is the guard).
func readSmall(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// sanitize makes externally supplied text (container names, images, mount
// points) safe to display: valid UTF-8, no control or bidi/format characters,
// bounded length.
func sanitize(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		switch {
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			r = ' '
		case unicode.Is(unicode.Cf, r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
