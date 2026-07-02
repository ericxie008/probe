package agent

import (
	"bytes"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	cpuModel              string
	cpuCount              int
	prevProc              map[int32]procSample
	prevProcTime          time.Time
	prevNetSample         time.Time
	prevCPUTimes          []cpu.TimesStat
	tick                  int    // increments each Collect() call
	cachedOS              string // semi-static host info
	cachedInterfaces      []proto.NetInterface
	cachedDisks           []proto.DiskInfo
	cachedDiskTotal       uint64
	cachedDiskUsed        uint64
	cachedProcs           []proto.ProcessInfo
	cachedConnCount       uint32
}

// procSample caches a process's accumulated CPU time and collection moment.
type procSample struct {
	cpuTime float64
}

// procEntry pairs a process handle with its memory percent and RSS.
type procEntry struct {
	p   *process.Process
	m   float64 // memory percent (for sorting)
	rss uint64  // RSS in bytes (for the table)
}

// NewCollector returns a Collector primed with static host facts.
func NewCollector() *Collector {
	c := &Collector{}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		c.cpuModel = infos[0].ModelName
	}
	if counts, err := cpu.Counts(false); err == nil {
		c.cpuCount = counts
	} else {
		c.cpuCount = runtime.NumCPU()
	}
	// Prime the CPU baseline so the first Collect() can compute a delta
	// instead of returning 0% for one cycle.
	if t, err := cpu.Times(true); err == nil {
		c.prevCPUTimes = t
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
		s.Uptime = hi.Uptime
		s.BootTime = hi.BootTime
		if c.cachedOS == "" {
			c.cachedOS = hi.OS + " " + hi.Platform + " " + hi.PlatformVersion
		}
		s.OS = c.cachedOS
	}

	if lv, err := load.Avg(); err == nil {
		s.Load1, s.Load5, s.Load15 = lv.Load1, lv.Load5, lv.Load15
	}

	// CPU: non-blocking incremental sampling. cpu.Times() reads /proc/stat
	// instantly; we compute percentages from the delta since the last
	// sample instead of blocking for 1 second (which cpu.Percent does).
	if cores, err := cpu.Times(true); err == nil && len(cores) > 0 {
		if c.prevCPUTimes != nil && len(c.prevCPUTimes) == len(cores) {
			perCore := make([]float64, len(cores))
			var sum float64
			for i, t := range cores {
				prev := c.prevCPUTimes[i]
				busy := t.User + t.System + t.Nice + t.Iowait + t.Irq +
					t.Softirq + t.Steal + t.Guest + t.GuestNice
				pBusy := prev.User + prev.System + prev.Nice + prev.Iowait + prev.Irq +
					prev.Softirq + prev.Steal + prev.Guest + prev.GuestNice
				total := t.Total() - prev.Total()
				if total > 0 {
					perCore[i] = (busy - pBusy) / total * 100
				}
				sum += perCore[i]
			}
			s.CPUPerCore = perCore
			s.CPUUsage = sum / float64(len(cores))
		}
		c.prevCPUTimes = cores
	}
	s.CPUModel = c.cpuModel
	s.CPUCount = c.cpuCount
	// Temperature: use lightweight sysfs reads. The gopsutil path
	// (host.SensorsTemperatures) does filepath.Glob + per-file I/O
	// over the whole hwmon tree, which is slow on OpenWrt flash.
	// readLinuxThermal reads only /sys/class/thermal, which is much
	// cheaper and works on the same hardware.
	s.CPUTemp = readLinuxThermal()

	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemoryTotal = vm.Total
		s.MemoryUsed = vm.Used
	}
	if sm, err := mem.SwapMemory(); err == nil {
		s.SwapTotal = sm.Total
		s.SwapUsed = sm.Used
	}

	if c.tick%10 == 0 || c.cachedDisks == nil {
		var disks []proto.DiskInfo
		var diskTotal, diskUsed uint64
		if parts, err := disk.Partitions(false); err == nil {
			seen := make(map[string]struct{})
			for _, p := range parts {
				u, err := disk.Usage(p.Mountpoint)
				if err != nil || u == nil {
					continue
				}
				disks = append(disks, proto.DiskInfo{
					Device: p.Device, Mountpoint: p.Mountpoint,
					FSType: p.Fstype, Total: u.Total, Used: u.Used,
				})
				key := p.Device
				if key == "" {
					key = p.Mountpoint
				}
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				diskTotal += u.Total
				diskUsed += u.Used
			}
		}
		c.cachedDisks = disks
		c.cachedDiskTotal = diskTotal
		c.cachedDiskUsed = diskUsed
	}
	s.Disks = c.cachedDisks
	s.DiskTotal = c.cachedDiskTotal
	s.DiskUsed = c.cachedDiskUsed

	netNow := time.Now()
	if counters, err := net.IOCounters(false); err == nil && len(counters) > 0 {
		s.NetIn = counters[0].BytesRecv
		s.NetOut = counters[0].BytesSent
	}
	// Use the real wall-clock gap between consecutive network samples.
	// Collect() includes a ~1s cpu.Percent() blocking call plus other
	// I/O, so now-to-now spans ~4s not the 3s ticker interval. Using
	// the ticker interval here systematically understated speeds.
	netDt := netNow.Sub(c.prevNetSample).Seconds()
	if !c.prevNetSample.IsZero() && netDt > 0 {
		if s.NetIn >= c.prevNetIn {
			s.NetSpeedIn = uint64(float64(s.NetIn-c.prevNetIn) / netDt)
		}
		if s.NetOut >= c.prevNetOut {
			s.NetSpeedOut = uint64(float64(s.NetOut-c.prevNetOut) / netDt)
		}
	}
	c.prevNetIn, c.prevNetOut, c.prevNetSample = s.NetIn, s.NetOut, netNow

	if c.tick%10 == 0 || c.cachedInterfaces == nil {
		var ifcs []proto.NetInterface
		if ifaces, err := net.Interfaces(); err == nil {
			for _, ifc := range ifaces {
				if isVirtualIface(ifc.Name) {
					continue
				}
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
				ifcs = append(ifcs, ni)
			}
		}
		c.cachedInterfaces = ifcs
	}
	s.Interfaces = c.cachedInterfaces

	// Process table: collect every 5th tick (~15s) since this is the
	// heaviest operation — it walks all of /proc and does 2 reads per PID.
	if c.tick%5 == 0 {
		procs, _ := process.Processes()
		curProc := make(map[int32]procSample, len(procs))
		entries := make([]procEntry, 0, len(procs))
		var memTotal uint64
		if vm, err := mem.VirtualMemory(); err == nil {
			memTotal = vm.Total
		}
		for _, p := range procs {
			mi, err := p.MemoryInfo()
			if err != nil || mi == nil || mi.RSS == 0 {
				continue
			}
			var pct float64
			if memTotal > 0 {
				pct = float64(mi.RSS) / float64(memTotal) * 100
			}
			entries = append(entries, procEntry{p: p, m: pct, rss: mi.RSS})
			if t, err := p.Times(); err == nil {
				curProc[p.Pid] = procSample{cpuTime: t.Total()}
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].m > entries[j].m })
		if len(entries) > 10 {
			entries = entries[:10]
		}
		dt := now.Sub(c.prevProcTime).Seconds()
		var procs2 []proto.ProcessInfo
		for _, e := range entries {
			nm, _ := e.p.Name()
			cpuPct := 0.0
			if cur, ok := curProc[e.p.Pid]; ok {
				if prev, ok := c.prevProc[e.p.Pid]; ok && dt > 0 && cur.cpuTime >= prev.cpuTime {
					cpuPct = (cur.cpuTime - prev.cpuTime) / dt * 100
				}
			}
			procs2 = append(procs2, proto.ProcessInfo{
				PID: e.p.Pid, Name: nm, CPU: cpuPct, Memory: e.rss,
			})
		}
		c.prevProc = curProc
		c.prevProcTime = now
		c.cachedProcs = procs2
	}
	s.Processes = c.cachedProcs

	// TCP connection count: throttle to every 5th tick. On OpenWrt
	// routers doing NAT there can be thousands of connections, and
	// net.Connections() reads them all from /proc/net/tcp.
	if c.tick%5 == 0 {
		var count uint32
		if conns, err := net.Connections("tcp"); err == nil {
			for _, cn := range conns {
				if cn.Status == "ESTABLISHED" {
					count++
				}
			}
		}
		c.cachedConnCount = count
	}
	s.ConnCount = c.cachedConnCount

	c.tick++
	s.Timestamp = now.Unix()
	return s
}

// isIPv6 reports whether an interface address is IPv6. gopsutil returns
// addresses with a CIDR suffix (e.g. "192.168.1.10/24" or "fe80::1/64"),
// so we strip the prefix first to avoid any ambiguity.
func isIPv6(addr string) bool {
	ip := addr
	if i := strings.IndexByte(ip, '/'); i >= 0 {
		ip = ip[:i]
	}
	return strings.Contains(ip, ":")
}

// readLinuxThermal reads CPU temperature from /sys/class/thermal/thermal_zone*/temp.
// This is the standard Linux sysfs path and works on ARM servers where
// gopsutil's SensorsTemperatures() often returns nothing.
func readLinuxThermal() float64 {
	entries, err := os.ReadDir("/sys/class/thermal")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) < 12 || name[:12] != "thermal_zone" {
			continue
		}
		data, err := os.ReadFile("/sys/class/thermal/" + name + "/temp")
		if err != nil {
			continue
		}
		data = bytes.TrimSpace(data)
		milli, err := strconv.Atoi(string(data))
		if err == nil && milli > 0 {
			return float64(milli) / 1000.0
		}
	}
	return 0
}

// isVirtualIface reports whether an interface name corresponds to a
// virtual or loopback adapter that is not a physical NIC (docker0,
// veth*, br-*, tun*, lo, etc.).
func isVirtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	prefixes := []string{"docker", "veth", "br-", "tun", "tap", "virbr",
		"vnet", "wg", "utun", "fw", "ifb"}
	lower := strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
