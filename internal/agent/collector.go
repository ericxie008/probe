package agent

import (
	"runtime"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"

	"probe/internal/proto"
)

// Collector gathers host metrics using gopsutil.
type Collector struct {
	// prevNet holds the previous aggregate byte counters, used to compute
	// instantaneous speed over the interval since the last Collect() call.
	prevNetIn, prevNetOut uint64
	prevTime              time.Time
	cpuModel              string
	cpuCount              int
}

// procEntry pairs a process handle with its memory percent for sorting.
type procEntry struct {
	p *process.Process
	m float64
}

// NewCollector returns a Collector primed with static host facts.
func NewCollector() *Collector {
	c := &Collector{prevTime: time.Now()}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		c.cpuModel = infos[0].ModelName
	}
	if counts, err := cpu.Counts(false); err == nil {
		c.cpuCount = counts
	} else {
		c.cpuCount = runtime.NumCPU()
	}
	return c
}

// CPUModel returns the cached model name.
func (c *Collector) CPUModel() string { return c.cpuModel }

// CPUCount returns the cached logical core count.
func (c *Collector) CPUCount() int { return c.cpuCount }

// Collect samples the current system state into a proto.State.
func (c *Collector) Collect(agentID, name string) *proto.State {
	now := time.Now()
	s := &proto.State{
		AgentID: agentID,
		Name:    name,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}

	if hi, err := host.Info(); err == nil {
		s.OS = hi.OS + " " + hi.Platform + " " + hi.PlatformVersion
		s.Uptime = hi.Uptime
		s.BootTime = hi.BootTime
	}

	if lv, err := load.Avg(); err == nil {
		s.Load1, s.Load5, s.Load15 = lv.Load1, lv.Load5, lv.Load15
	}

	if pct, err := cpu.Percent(time.Second, false); err == nil && len(pct) > 0 {
		s.CPUUsage = pct[0]
	}
	if perCore, err := cpu.Percent(time.Second, true); err == nil {
		s.CPUPerCore = perCore
	}
	s.CPUModel = c.cpuModel
	s.CPUCount = c.cpuCount
	if temps, err := host.SensorsTemperatures(); err == nil {
		for _, t := range temps {
			if t.Temperature > 0 {
				s.CPUTemp = t.Temperature
				break
			}
		}
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemoryTotal = vm.Total
		s.MemoryUsed = vm.Used
	}
	if sm, err := mem.SwapMemory(); err == nil {
		s.SwapTotal = sm.Total
		s.SwapUsed = sm.Used
	}

	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			u, err := disk.Usage(p.Mountpoint)
			if err != nil || u == nil {
				continue
			}
			s.DiskTotal += u.Total
			s.DiskUsed += u.Used
			s.Disks = append(s.Disks, proto.DiskInfo{
				Device:     p.Device,
				Mountpoint: p.Mountpoint,
				FSType:     p.Fstype,
				Total:      u.Total,
				Used:       u.Used,
			})
		}
	}

	if counters, err := net.IOCounters(false); err == nil && len(counters) > 0 {
		s.NetIn = counters[0].BytesRecv
		s.NetOut = counters[0].BytesSent
	}
	dt := now.Sub(c.prevTime).Seconds()
	if dt > 0 {
		if s.NetIn >= c.prevNetIn {
			s.NetSpeedIn = uint64(float64(s.NetIn-c.prevNetIn) / dt)
		}
		if s.NetOut >= c.prevNetOut {
			s.NetSpeedOut = uint64(float64(s.NetOut-c.prevNetOut) / dt)
		}
	}
	c.prevNetIn, c.prevNetOut, c.prevTime = s.NetIn, s.NetOut, now

	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			ni := proto.NetInterface{
				Name: ifc.Name,
				MAC:  ifc.HardwareAddr,
			}
			for _, a := range ifc.Addrs {
				if isIPv6(a.Addr) {
					if ni.IPv6 == "" {
						ni.IPv6 = a.Addr
					}
				} else if ni.IPv4 == "" {
					ni.IPv4 = a.Addr
				}
			}
			s.Interfaces = append(s.Interfaces, ni)
		}
	}

	if procs, err := process.Processes(); err == nil {
		entries := make([]procEntry, 0, len(procs))
		for _, p := range procs {
			m, err := p.MemoryPercent()
			if err != nil || m <= 0 {
				continue
			}
			entries = append(entries, procEntry{p, float64(m)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].m > entries[j].m })
		if len(entries) > 10 {
			entries = entries[:10]
		}
		for _, e := range entries {
			nm, _ := e.p.Name()
			cpuPct, _ := e.p.CPUPercent()
			pi := proto.ProcessInfo{PID: e.p.Pid, Name: nm, CPU: cpuPct}
			if mem, _ := e.p.MemoryInfo(); mem != nil {
				pi.Memory = mem.RSS
			}
			s.Processes = append(s.Processes, pi)
		}
	}

	if conns, err := net.Connections("tcp"); err == nil {
		for _, cn := range conns {
			if cn.Status == "ESTABLISHED" {
				s.ConnCount++
			}
		}
	}

	s.Timestamp = now.Unix()
	return s
}

func isIPv6(addr string) bool {
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return true
		}
	}
	return false
}
