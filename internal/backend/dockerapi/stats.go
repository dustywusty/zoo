package dockerapi

import "strings"

// StatsJSON is the part of a stats sample zoo reads. Docker sends the
// previous CPU reading along with each one, so a rate needs no state.
type StatsJSON struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint32 `json:"online_cpus"`
}

// CPUPercent is what `docker stats` shows: percent of one core (200% is
// two busy cores). ok is false until there are two readings to compare.
func (s StatsJSON) CPUPercent() (float64, bool) {
	cpu := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sys := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if s.PreCPUStats.SystemUsage == 0 || sys <= 0 || cpu < 0 {
		return 0, false
	}
	n := float64(s.CPUStats.OnlineCPUs)
	if n == 0 {
		n = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if n == 0 {
		n = 1
	}
	return cpu / sys * n * 100, true
}

// MemUsed is what `docker stats` shows: usage minus reclaimable page cache
// (inactive_file on cgroup v2, total_inactive_file on v1).
func (s StatsJSON) MemUsed() float64 {
	u := s.MemoryStats.Usage
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := s.MemoryStats.Stats[k]; ok && v < u {
			return float64(u - v)
		}
	}
	return float64(u)
}

// Net sums received and sent bytes over all interfaces.
func (s StatsJSON) Net() (rx, tx float64) {
	for _, n := range s.Networks {
		rx += float64(n.RxBytes)
		tx += float64(n.TxBytes)
	}
	return rx, tx
}

// Blk sums block-device bytes read and written.
func (s StatsJSON) Blk() (read, write float64) {
	for _, e := range s.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			read += float64(e.Value)
		case "write":
			write += float64(e.Value)
		}
	}
	return read, write
}
