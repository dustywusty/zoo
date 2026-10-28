package daemon

import (
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/jsonx"
)

// Stat is a resource snapshot for one cage, derived from `container stats`.
// The CLI's JSON field names aren't a stable contract, so fields are matched
// by name heuristics and the raw numbers are kept for the UI.
type Stat struct {
	CPUPercent float64            `json:"cpu_percent"`
	MemUsed    float64            `json:"mem_used"`
	MemLimit   float64            `json:"mem_limit"`
	NetRxRate  float64            `json:"net_rx_rate"` // bytes/s
	NetTxRate  float64            `json:"net_tx_rate"`
	NetRx      float64            `json:"net_rx"`
	NetTx      float64            `json:"net_tx"`
	BlkRead    float64            `json:"blk_read"`
	BlkWrite   float64            `json:"blk_write"`
	Pids       float64            `json:"pids"`
	Have       map[string]bool    `json:"have"`
	Raw        map[string]float64 `json:"raw"`
	At         time.Time          `json:"at"`

	cpuCum float64 // cumulative cpu time, for deltas
	cpuNs  float64 // multiplier to nanoseconds
}

func pick(nums map[string]float64, must []string, any []string, not []string) (float64, string, bool) {
	keys := make([]string, 0, len(nums))
	for k := range nums {
		keys = append(keys, k)
	}
	// Deterministic: shortest path first, then alphabetical.
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) < len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, path := range keys {
		v := nums[path]
		p := strings.ToLower(path)
		ok := true
		for _, m := range must {
			if !strings.Contains(p, m) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if len(any) > 0 {
			hit := false
			for _, a := range any {
				if strings.Contains(p, a) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		bad := false
		for _, n := range not {
			if strings.Contains(p, n) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		return v, path, true
	}
	return 0, "", false
}

// parseStat turns one stats element into a Stat, using prev for rates.
func parseStat(el any, prev *Stat, now time.Time) (string, Stat) {
	id := jsonx.String(el, "id", "name", "containerid", "container")
	nums := jsonx.Numbers(el)
	s := Stat{Raw: nums, Have: map[string]bool{}, At: now}

	if v, _, ok := pick(nums, []string{"cpu"}, []string{"percent", "perc", "pct"}, nil); ok {
		s.CPUPercent, s.Have["cpu"] = v, true
	} else if v, path, ok := pick(nums, []string{"cpu"}, []string{"usage", "time", "total"}, []string{"limit", "count", "cpus"}); ok {
		p := strings.ToLower(path)
		s.cpuCum = v
		switch {
		case strings.Contains(p, "nsec") || strings.Contains(p, "nano"):
			s.cpuNs = 1
		case strings.Contains(p, "msec") && !strings.Contains(p, "usec"):
			s.cpuNs = 1e6
		default:
			s.cpuNs = 1e3 // assume microseconds
		}
		if prev != nil && prev.cpuCum > 0 && v >= prev.cpuCum {
			dt := now.Sub(prev.At).Nanoseconds()
			if dt > 0 {
				s.CPUPercent = (v - prev.cpuCum) * s.cpuNs / float64(dt) * 100
				s.Have["cpu"] = true
			}
		}
	}
	if v, _, ok := pick(nums, []string{"mem"}, []string{"usage", "used", "rss"}, []string{"limit", "max", "percent"}); ok {
		s.MemUsed, s.Have["mem"] = v, true
	}
	if v, _, ok := pick(nums, []string{"mem"}, []string{"limit", "max", "total"}, []string{"percent"}); ok {
		s.MemLimit = v
	}
	if v, _, ok := pick(nums, nil, []string{"rx", "received", "netin", "networkin"}, []string{"packet", "error", "drop"}); ok {
		s.NetRx, s.Have["net"] = v, true
	}
	if v, _, ok := pick(nums, nil, []string{"tx", "transmitted", "sent", "netout", "networkout"}, []string{"packet", "error", "drop"}); ok {
		s.NetTx, s.Have["net"] = v, true
	}
	if v, _, ok := pick(nums, nil, []string{"blockread", "blkread", "diskread", "readbytes", "block.read", "io.read"}, nil); ok {
		s.BlkRead, s.Have["blk"] = v, true
	}
	if v, _, ok := pick(nums, nil, []string{"blockwrite", "blkwrite", "diskwrite", "writebytes", "block.write", "io.write"}, nil); ok {
		s.BlkWrite, s.Have["blk"] = v, true
	}
	if v, _, ok := pick(nums, nil, []string{"pids", "process", "numprocs"}, []string{"limit"}); ok {
		s.Pids, s.Have["pids"] = v, true
	}
	if prev != nil {
		dt := now.Sub(prev.At).Seconds()
		if dt > 0 {
			if s.NetRx >= prev.NetRx {
				s.NetRxRate = (s.NetRx - prev.NetRx) / dt
			}
			if s.NetTx >= prev.NetTx {
				s.NetTxRate = (s.NetTx - prev.NetTx) / dt
			}
		}
	}
	return id, s
}
