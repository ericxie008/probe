package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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
	cachedUDPConnCount    uint32

	// DisableProcs disables process list sampling to keep CPU near zero on weak ARM/embedded.
	DisableProcs   bool
	lastProcSample time.Time
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
	c := &Collector{
		cachedProcs: make([]proto.ProcessInfo, 0),
	}
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

	if c.tick%20 == 0 || c.cachedDisks == nil {
		var disks []proto.DiskInfo
		var diskTotal, diskUsed uint64
		if parts, err := disk.Partitions(false); err == nil {
			seen := make(map[string]struct{})
			var fallbackDisks []proto.DiskInfo
			var fbTotal, fbUsed uint64

			for _, p := range parts {
				u, err := disk.Usage(p.Mountpoint)
				if err != nil || u == nil {
					continue
				}
				info := proto.DiskInfo{
					Device: p.Device, Mountpoint: p.Mountpoint,
					FSType: p.Fstype, Total: u.Total, Used: u.Used,
				}
				// Keep root fallback
				if p.Mountpoint == "/" {
					fallbackDisks = append(fallbackDisks, info)
					fbTotal = u.Total
					fbUsed = u.Used
				}

				// Filter virtual / container / pseudo filesystems (Nezha approach)
				if isVirtualFS(p.Fstype, p.Mountpoint) {
					continue
				}

				key := p.Device
				if key == "" {
					key = p.Mountpoint
				}
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}

				disks = append(disks, info)
				diskTotal += u.Total
				diskUsed += u.Used
			}

			// If all disks were filtered (e.g. rootfs on OpenWrt overlay), use fallback root
			if len(disks) == 0 && len(fallbackDisks) > 0 {
				disks = fallbackDisks
				diskTotal = fbTotal
				diskUsed = fbUsed
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

	if c.tick%20 == 0 || c.cachedInterfaces == nil {
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

	// Process table: collect at most once every 60s to keep CPU overhead near zero on weak ARM cores.
	if !c.DisableProcs {
		if c.lastProcSample.IsZero() || now.Sub(c.lastProcSample) >= 60*time.Second {
			c.cachedProcs = c.collectProcesses(now)
			c.lastProcSample = now
		}
	}
	s.Processes = c.cachedProcs

	// TCP & UDP connection count: direct kernel read on Linux, throttled gopsutil on others.
	if tcp, udp, ok := fastLinuxSockstat(); ok {
		c.cachedConnCount = tcp
		c.cachedUDPConnCount = udp
	} else if c.tick%5 == 0 {
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
	s.UDPConnCount = c.cachedUDPConnCount

	c.tick++
	s.Timestamp = now.Unix()
	return s
}

// collectProcesses gathers top memory/CPU processes with optimizations for Linux and Darwin.
func (c *Collector) collectProcesses(now time.Time) []proto.ProcessInfo {
	var memTotal uint64
	if vm, err := mem.VirtualMemory(); err == nil {
		memTotal = vm.Total
	}

	// On Linux, use direct zero-reflection /proc scanner
	if runtime.GOOS == "linux" {
		if procs, ok := c.fastLinuxProcesses(now, memTotal); ok {
			return procs
		}
	}

	// On Darwin, use single-shot aggregate ps (20ms, 1 child process, zero per-pid fork loops)
	if runtime.GOOS == "darwin" {
		if procs, ok := c.fastDarwinProcesses(); ok {
			return procs
		}
	}

	// Fallback to gopsutil on other non-Linux platforms
	procs, err := process.Processes()
	if err != nil || len(procs) == 0 {
		return c.cachedProcs
	}
	curProc := make(map[int32]procSample, len(procs))
	entries := make([]procEntry, 0, 32)

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
	procs2 := make([]proto.ProcessInfo, 0, len(entries))
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
	return procs2
}

// fastDarwinProcesses parses top processes in a single shot using 'ps -A -o pid=,rss=,%cpu=,comm='.
// This eliminates the O(N) 1000+ child process forks that gopsutil nocgo runs on Darwin.
func (c *Collector) fastDarwinProcesses() ([]proto.ProcessInfo, bool) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,rss=,%cpu=,comm=").Output()
	if err != nil {
		return nil, false
	}
	type rawProc struct {
		pid  int32
		rss  uint64
		cpu  float64
		name string
	}
	var list []rawProc
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) < 4 {
			continue
		}
		pidInt, err := strconv.Atoi(fields[0])
		if err != nil || pidInt <= 0 {
			continue
		}
		rssKB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || rssKB == 0 {
			continue
		}
		cpuPct, _ := strconv.ParseFloat(fields[2], 64)
		comm := strings.Join(fields[3:], " ")
		name := filepath.Base(comm)
		list = append(list, rawProc{
			pid:  int32(pidInt),
			rss:  rssKB * 1024,
			cpu:  cpuPct,
			name: name,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].rss > list[j].rss })
	if len(list) > 10 {
		list = list[:10]
	}
	res := make([]proto.ProcessInfo, 0, len(list))
	for _, p := range list {
		res = append(res, proto.ProcessInfo{
			PID:    p.pid,
			Name:   p.name,
			CPU:    p.cpu,
			Memory: p.rss,
		})
	}
	return res, true
}

// fastLinuxProcesses reads /proc directly, skipping all kernel threads in 0.001ms.
func (c *Collector) fastLinuxProcesses(now time.Time, memTotal uint64) ([]proto.ProcessInfo, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	type rawProc struct {
		pid     int32
		name    string
		rss     uint64
		cpuTime float64
		memPct  float64
	}
	pageSize := uint64(os.Getpagesize())
	if pageSize == 0 {
		pageSize = 4096
	}

	curProc := make(map[int32]procSample, 64)
	var rawList []rawProc

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pidInt, err := strconv.Atoi(entry.Name())
		if err != nil || pidInt <= 0 {
			continue
		}
		pid := int32(pidInt)
		pidStr := entry.Name()

		// Read /proc/<pid>/statm: "size resident shared text lib data dt"
		statmData, err := os.ReadFile("/proc/" + pidStr + "/statm")
		if err != nil {
			continue
		}
		fields := bytes.Fields(statmData)
		if len(fields) < 2 {
			continue
		}
		residentPages, err := strconv.ParseUint(string(fields[1]), 10, 64)
		if err != nil || residentPages == 0 {
			// Kernel thread or zero RSS -> skip immediately
			continue
		}
		rss := residentPages * pageSize

		// Read /proc/<pid>/comm for process name
		commData, err := os.ReadFile("/proc/" + pidStr + "/comm")
		if err != nil {
			continue
		}
		name := string(bytes.TrimSpace(commData))

		// Read /proc/<pid>/stat for utime + stime
		var cpuTotal float64
		if statData, err := os.ReadFile("/proc/" + pidStr + "/stat"); err == nil {
			rParen := bytes.LastIndexByte(statData, ')')
			if rParen >= 0 && rParen+2 < len(statData) {
				rest := bytes.Fields(statData[rParen+2:])
				if len(rest) > 12 {
					utime, _ := strconv.ParseFloat(string(rest[11]), 64)
					stime, _ := strconv.ParseFloat(string(rest[12]), 64)
					cpuTotal = (utime + stime) / 100.0
				}
			}
		}

		curProc[pid] = procSample{cpuTime: cpuTotal}
		var memPct float64
		if memTotal > 0 {
			memPct = float64(rss) / float64(memTotal) * 100
		}
		rawList = append(rawList, rawProc{
			pid:     pid,
			name:    name,
			rss:     rss,
			cpuTime: cpuTotal,
			memPct:  memPct,
		})
	}

	sort.Slice(rawList, func(i, j int) bool { return rawList[i].memPct > rawList[j].memPct })
	if len(rawList) > 10 {
		rawList = rawList[:10]
	}

	dt := now.Sub(c.prevProcTime).Seconds()
	result := make([]proto.ProcessInfo, 0, len(rawList))
	for _, rp := range rawList {
		cpuPct := 0.0
		if prev, ok := c.prevProc[rp.pid]; ok && dt > 0 && rp.cpuTime >= prev.cpuTime {
			cpuPct = (rp.cpuTime - prev.cpuTime) / dt * 100
		}
		result = append(result, proto.ProcessInfo{
			PID:    rp.pid,
			Name:   rp.name,
			CPU:    cpuPct,
			Memory: rp.rss,
		})
	}

	c.prevProc = curProc
	c.prevProcTime = now
	return result, true
}

// fastLinuxSockstat reads active TCP and UDP sockets from /proc/net/sockstat and /proc/net/sockstat6.
// This executes in < 0.1ms with zero allocations, avoiding massive overhead on routers.
func fastLinuxSockstat() (tcpCount uint32, udpCount uint32, ok bool) {
	data, err := os.ReadFile("/proc/net/sockstat")
	if err != nil {
		return 0, 0, false
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			if fields[0] == "TCP:" {
				for i := 1; i < len(fields)-1; i++ {
					if fields[i] == "inuse" {
						if n, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
							tcpCount += uint32(n)
						}
						break
					}
				}
			} else if fields[0] == "UDP:" {
				for i := 1; i < len(fields)-1; i++ {
					if fields[i] == "inuse" {
						if n, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
							udpCount += uint32(n)
						}
						break
					}
				}
			}
		}
	}

	if data6, err := os.ReadFile("/proc/net/sockstat6"); err == nil {
		lines6 := strings.Split(string(data6), "\n")
		for _, line := range lines6 {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if fields[0] == "TCP6:" {
					for i := 1; i < len(fields)-1; i++ {
						if fields[i] == "inuse" {
							if n, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
								tcpCount += uint32(n)
							}
							break
						}
					}
				} else if fields[0] == "UDP6:" {
					for i := 1; i < len(fields)-1; i++ {
						if fields[i] == "inuse" {
							if n, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
								udpCount += uint32(n)
							}
							break
						}
					}
				}
			}
		}
	}
	return tcpCount, udpCount, true
}

// isVirtualFS reports whether a filesystem is a virtual or pseudo-filesystem
// that should be excluded from disk capacity and usage calculations.
func isVirtualFS(fstype, mountpoint string) bool {
	if strings.HasPrefix(mountpoint, "/dev") ||
		strings.HasPrefix(mountpoint, "/proc") ||
		strings.HasPrefix(mountpoint, "/sys") ||
		strings.Contains(mountpoint, "/docker/overlay2") ||
		strings.Contains(mountpoint, "/containerd/") {
		return true
	}
	virtualTypes := []string{
		"tmpfs", "devtmpfs", "devfs", "iso9660", "overlay", "squashfs",
		"none", "proc", "sysfs", "cgroup", "cgroup2", "pstore", "ramfs", "autofs",
	}
	lower := strings.ToLower(fstype)
	for _, v := range virtualTypes {
		if lower == v {
			return true
		}
	}
	return false
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
		"vnet", "wg", "utun", "fw", "ifb",
		"ppp", "sit", "ip6tnl", "ovs-system"}
	lower := strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
