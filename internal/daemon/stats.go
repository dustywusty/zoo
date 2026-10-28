package daemon

import (
	"time"

	"github.com/dusty/zoo/internal/backend"
)

// Stat is a resource snapshot for one cage, with rates derived from the
// previous sample.
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
	Raw        map[string]float64 `json:"raw,omitempty"`
	At         time.Time          `json:"at"`

	cpuTimeNs float64
}

// toStat turns a backend sample into a Stat, using prev for rates.
func toStat(s backend.Sample, prev *Stat, now time.Time) Stat {
	st := Stat{
		CPUPercent: s.CPUPercent,
		MemUsed:    s.MemUsed, MemLimit: s.MemLimit,
		NetRx: s.NetRx, NetTx: s.NetTx,
		BlkRead: s.BlkRead, BlkWrite: s.BlkWrite,
		Pids: s.Pids, Have: map[string]bool{}, Raw: s.Raw, At: now,
		cpuTimeNs: s.CPUTimeNs,
	}
	for k, v := range s.Have {
		if k != "cpu_time" {
			st.Have[k] = v
		}
	}
	if prev == nil {
		return st
	}
	dt := now.Sub(prev.At)
	if dt <= 0 {
		return st
	}
	if s.Have["cpu_time"] && !s.Have["cpu"] && prev.cpuTimeNs > 0 && s.CPUTimeNs >= prev.cpuTimeNs {
		st.CPUPercent = (s.CPUTimeNs - prev.cpuTimeNs) / float64(dt.Nanoseconds()) * 100
		st.Have["cpu"] = true
	}
	if st.NetRx >= prev.NetRx {
		st.NetRxRate = (st.NetRx - prev.NetRx) / dt.Seconds()
	}
	if st.NetTx >= prev.NetTx {
		st.NetTxRate = (st.NetTx - prev.NetTx) / dt.Seconds()
	}
	return st
}
