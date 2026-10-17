package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
)

// Stats is the summary the UI shows for a container.
type Stats struct {
	CPU      float64 `json:"cpu"`      // percent of one core x cores (like `docker stats`)
	Mem      uint64  `json:"mem"`      // bytes, excluding page cache
	MemLimit uint64  `json:"memLimit"` // bytes
	NetRx    uint64  `json:"netRx"`
	NetTx    uint64  `json:"netTx"`
	BlkRead  uint64  `json:"blkRead"`
	BlkWrite uint64  `json:"blkWrite"`
	PIDs     uint64  `json:"pids"`
}

type rawStats struct {
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
	PIDsStats struct {
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

func (r rawStats) summarize() Stats {
	var s Stats
	cpuDelta := float64(r.CPUStats.CPUUsage.TotalUsage) - float64(r.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(r.CPUStats.SystemUsage) - float64(r.PreCPUStats.SystemUsage)
	online := float64(r.CPUStats.OnlineCPUs)
	if online == 0 {
		online = float64(len(r.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && cpuDelta > 0 {
		s.CPU = cpuDelta / sysDelta * online * 100
	}
	// Match `docker stats`: subtract reclaimable page cache.
	cache := r.MemoryStats.Stats["inactive_file"]                // cgroup v2
	if v, ok := r.MemoryStats.Stats["total_inactive_file"]; ok { // cgroup v1
		cache = v
	}
	if r.MemoryStats.Usage > cache {
		s.Mem = r.MemoryStats.Usage - cache
	}
	s.MemLimit = r.MemoryStats.Limit
	for _, n := range r.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}
	for _, b := range r.BlkioStats.IOServiceBytesRecursive {
		switch b.Op {
		case "read", "Read":
			s.BlkRead += b.Value
		case "write", "Write":
			s.BlkWrite += b.Value
		}
	}
	s.PIDs = r.PIDsStats.Current
	return s
}

// Reading is one look at a container's counters. One-shot stats don't carry
// the previous reading, so CPU use is worked out between two of these.
type Reading struct {
	CPUTotal   uint64
	System     uint64
	OnlineCPUs uint32
	Stats      // memory, network, block I/O and PIDs; CPU is left at 0
}

// StatsOnce takes one reading without waiting a second for Docker to take two.
func (c *Client) StatsOnce(ctx context.Context, id string) (Reading, error) {
	var r rawStats
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if err := c.getJSON(ctx, "GET", "/containers/"+url.PathEscape(id)+"/stats", q, nil, &r); err != nil {
		return Reading{}, err
	}
	s := r.summarize()
	s.CPU = 0 // without a previous reading this would be the average since it started
	online := r.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint32(len(r.CPUStats.CPUUsage.PercpuUsage))
	}
	return Reading{CPUTotal: r.CPUStats.CPUUsage.TotalUsage, System: r.CPUStats.SystemUsage, OnlineCPUs: online, Stats: s}, nil
}

// CPUBetween is the CPU use between two readings, counted like `docker
// stats` (100% = one whole core).
func CPUBetween(prev, cur Reading) float64 {
	if cur.CPUTotal < prev.CPUTotal || cur.System <= prev.System {
		return 0
	}
	return float64(cur.CPUTotal-prev.CPUTotal) / float64(cur.System-prev.System) * float64(cur.OnlineCPUs) * 100
}

// StreamStats calls fn roughly once a second with fresh stats until ctx ends
// or the container stops.
func (c *Client) StreamStats(ctx context.Context, id string, fn func(Stats)) error {
	body, err := c.Stream(ctx, "GET", "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"1"}}, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	dec := json.NewDecoder(bufio.NewReader(body))
	for {
		var r rawStats
		if err := dec.Decode(&r); err != nil {
			return err
		}
		fn(r.summarize())
	}
}
