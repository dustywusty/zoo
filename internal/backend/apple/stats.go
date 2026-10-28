package apple

import (
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/jsonx"
)

// The `container stats` JSON field names aren't a stable contract, so fields
// are matched by name heuristics and the raw numbers are kept for the UI.

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
		p := strings.ToLower(path)
		if !containsAll(p, must) || (len(any) > 0 && !containsAny(p, any)) || containsAny(p, not) {
			continue
		}
		return nums[path], path, true
	}
	return 0, "", false
}

func containsAll(s string, subs []string) bool {
	for _, m := range subs {
		if !strings.Contains(s, m) {
			return false
		}
	}
	return true
}

func containsAny(s string, subs []string) bool {
	for _, m := range subs {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// exact are the `container stats --format json` keys verified on 1.6.1.
var exact = map[string]string{
	"cpu_time": "cpuUsageUsec", "mem": "memoryUsageBytes", "mem_limit": "memoryLimitBytes",
	"net_rx": "networkRxBytes", "net_tx": "networkTxBytes",
	"blk_read": "blockReadBytes", "blk_write": "blockWriteBytes", "pids": "numProcesses",
}

// parseStat turns one stats element into a Sample: exact field names first,
// name heuristics as a fallback for other releases.
func parseStat(el any) backend.Sample {
	if m, ok := el.(map[string]any); ok {
		if _, ok := m[exact["mem"]]; ok {
			return parseExact(m)
		}
	}
	return parseHeuristic(el)
}

func parseExact(m map[string]any) backend.Sample {
	s := backend.Sample{ID: jsonx.String(m, "id"), Have: map[string]bool{}}
	num := func(key string) (float64, bool) { return jsonx.Number(m[exact[key]]) }
	if v, ok := num("cpu_time"); ok {
		s.CPUTimeNs, s.Have["cpu_time"] = v*1e3, true
	}
	if v, ok := num("mem"); ok {
		s.MemUsed, s.Have["mem"] = v, true
	}
	s.MemLimit, _ = num("mem_limit")
	rx, okRx := num("net_rx")
	tx, okTx := num("net_tx")
	if okRx || okTx {
		s.NetRx, s.NetTx, s.Have["net"] = rx, tx, true
	}
	r, okR := num("blk_read")
	w, okW := num("blk_write")
	if okR || okW {
		s.BlkRead, s.BlkWrite, s.Have["blk"] = r, w, true
	}
	if v, ok := num("pids"); ok {
		s.Pids, s.Have["pids"] = v, true
	}
	return s
}

func parseHeuristic(el any) backend.Sample {
	nums := jsonx.Numbers(el)
	s := backend.Sample{
		ID:   jsonx.String(el, "id", "name", "containerid", "container"),
		Raw:  nums,
		Have: map[string]bool{},
	}
	if v, _, ok := pick(nums, []string{"cpu"}, []string{"percent", "perc", "pct"}, nil); ok {
		s.CPUPercent, s.Have["cpu"] = v, true
	} else if v, path, ok := pick(nums, []string{"cpu"}, []string{"usage", "time", "total"}, []string{"limit", "count", "cpus"}); ok {
		p := strings.ToLower(path)
		mult := 1e3 // assume microseconds
		switch {
		case strings.Contains(p, "nsec") || strings.Contains(p, "nano"):
			mult = 1
		case strings.Contains(p, "msec") && !strings.Contains(p, "usec"):
			mult = 1e6
		}
		s.CPUTimeNs, s.Have["cpu_time"] = v*mult, true
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
	return s
}
