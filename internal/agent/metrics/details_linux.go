//go:build linux

package metrics

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
)

// NodeDetails adds numeric kernel/process evidence, never process arguments,
// environment, file contents or packet payloads. Existing alert series retain
// their old meaning. Missing files/fields are absent, with explicit source health.
func NodeDetails() Source { return nodeDetails("/proc", "/sys") }

func nodeDetails(procRoot, sysRoot string) Source {
	fs, fsErr := procfs.NewFS(procRoot)
	block, blockErr := blockdevice.NewFS(procRoot, sysRoot)
	previous := map[string]float64{}
	return func(elapsed float64) map[string]float64 {
		v := map[string]float64{}
		counters := map[string]float64{}
		ok := func(source string, err error) bool {
			v["node:source@"+source+":ok"] = 0
			if err == nil {
				v["node:source@"+source+":ok"] = 1
			}
			return err == nil
		}
		counter := func(key string, value uint64) {
			x := float64(value)
			v[key+"_total"], counters[key] = x, x
			if before, found := previous[key]; found && elapsed > 0 && x >= before {
				v[key+"_per_second"] = (x - before) / elapsed
			}
		}
		if !ok("procfs", fsErr) {
			previous = counters
			return v
		}
		if stat, err := fs.Stat(); ok("cpu", err) {
			cpus := map[string]procfs.CPUStat{"all": stat.CPUTotal}
			for id, cpu := range stat.CPU {
				cpus[strconv.FormatInt(id, 10)] = cpu
			}
			for id, cpu := range cpus {
				prefix := "node:cpu@" + id + ":"
				// Guest time is already included in user/nice. Do not add it to
				// the denominator a second time when deriving utilization.
				total := cpu.User + cpu.Nice + cpu.System + cpu.Idle + cpu.Iowait + cpu.IRQ + cpu.SoftIRQ + cpu.Steal
				for mode, seconds := range map[string]float64{"user": cpu.User, "nice": cpu.Nice, "system": cpu.System, "idle": cpu.Idle,
					"iowait": cpu.Iowait, "irq": cpu.IRQ, "softirq": cpu.SoftIRQ, "steal": cpu.Steal, "guest": cpu.Guest, "guest_nice": cpu.GuestNice} {
					v[prefix+mode+"_seconds_total"] = seconds
					key := prefix + mode
					counters[key] = seconds
					if priorTotal, found := previous[prefix+"total"]; found && total > priorTotal {
						if prior, found := previous[key]; found && seconds >= prior {
							v[prefix+mode+"_percent"] = 100 * (seconds - prior) / (total - priorTotal)
						}
					}
				}
				counters[prefix+"total"] = total
			}
			v["node:cpu_cores"] = float64(len(stat.CPU))
			v["node:processes_running"], v["node:processes_blocked"] = float64(stat.ProcessesRunning), float64(stat.ProcessesBlocked)
			v["node:boot_time_seconds"] = float64(stat.BootTime)
			counter("node:context_switches", stat.ContextSwitches)
			counter("node:processes_created", stat.ProcessCreated)
		}
		if load, err := fs.LoadAvg(); ok("load", err) {
			v["node:load1"], v["node:load5"], v["node:load15"] = load.Load1, load.Load5, load.Load15
		}
		if mem, err := fs.Meminfo(); ok("memory", err) {
			for name, value := range map[string]*uint64{"total": mem.MemTotalBytes, "available": mem.MemAvailableBytes,
				"free": mem.MemFreeBytes, "buffers": mem.BuffersBytes, "cached": mem.CachedBytes, "swap_total": mem.SwapTotalBytes,
				"swap_free": mem.SwapFreeBytes, "swap_cached": mem.SwapCachedBytes, "dirty": mem.DirtyBytes,
				"writeback": mem.WritebackBytes, "slab": mem.SlabBytes, "reclaimable": mem.SReclaimableBytes, "page_tables": mem.PageTablesBytes} {
				if value != nil {
					v["node:memory:"+name+"_bytes"] = float64(*value)
				}
			}
		}
		for _, resource := range []string{"cpu", "memory", "io"} {
			if psi, err := fs.PSIStatsForResource(resource); ok("pressure."+resource, err) {
				for name, line := range map[string]*procfs.PSILine{"some": psi.Some, "full": psi.Full} {
					if line != nil {
						prefix := "node:pressure@" + resource + ":" + name
						v[prefix+"_avg10_percent"], v[prefix+"_avg60_percent"], v[prefix+"_avg300_percent"] = line.Avg10, line.Avg60, line.Avg300
						v[prefix+"_microseconds_total"] = float64(line.Total)
					}
				}
			}
		}
		devices, netErr := fs.NetDev()
		if netErr == nil && len(devices) == 0 {
			netErr = errors.New("no network interface observations")
		}
		if ok("network", netErr) {
			for name, n := range devices {
				prefix := "node:interface@" + name + ":"
				for metric, x := range map[string]uint64{"rx_bytes": n.RxBytes, "tx_bytes": n.TxBytes, "rx_packets": n.RxPackets,
					"tx_packets": n.TxPackets, "rx_errors": n.RxErrors, "tx_errors": n.TxErrors, "rx_dropped": n.RxDropped, "tx_dropped": n.TxDropped} {
					counter(prefix+metric, x)
				}
			}
		}
		if ok("blockfs", blockErr) {
			if disks, err := block.ProcDiskstats(); ok("disk_io", err) {
				for _, d := range disks {
					if strings.HasPrefix(d.DeviceName, "loop") || strings.HasPrefix(d.DeviceName, "ram") {
						continue
					}
					prefix := "node:device@" + d.DeviceName + ":"
					for metric, x := range map[string]uint64{"read_ops": d.ReadIOs, "write_ops": d.WriteIOs,
						"read_bytes": d.ReadSectors * 512, "write_bytes": d.WriteSectors * 512,
						"read_milliseconds": d.ReadTicks, "write_milliseconds": d.WriteTicks,
						"busy_milliseconds": d.IOsTotalTicks, "weighted_milliseconds": d.WeightedIOTicks} {
						counter(prefix+metric, x)
					}
					v[prefix+"in_progress"] = float64(d.IOsInProgress)
				}
			}
		}
		self, err := fs.Proc(os.Getpid())
		if ok("self", err) {
			if stat, err := self.Stat(); ok("self.stat", err) {
				v["node:nagi:cpu_seconds_total"] = stat.CPUTime()
				v["node:nagi:rss_bytes"], v["node:nagi:virtual_bytes"] = float64(stat.ResidentMemory()), float64(stat.VirtualMemory())
				v["node:nagi:threads"] = float64(stat.NumThreads)
			}
			if n, err := self.FileDescriptorsLen(); ok("self.fds", err) {
				v["node:nagi:file_descriptors"] = float64(n)
			}
			if snmp, err := self.Snmp(); ok("transport", err) {
				for name, value := range map[string]*float64{"tcp_active_opens": snmp.ActiveOpens, "tcp_passive_opens": snmp.PassiveOpens,
					"tcp_attempt_fails": snmp.AttemptFails, "tcp_resets": snmp.EstabResets, "tcp_current_established": snmp.CurrEstab,
					"tcp_retransmitted_segments": snmp.RetransSegs, "tcp_input_errors": snmp.Tcp.InErrs,
					"udp_receive_buffer_errors": snmp.Udp.RcvbufErrors, "udp_send_buffer_errors": snmp.Udp.SndbufErrors,
					"udp_input_errors": snmp.Udp.InErrors, "udp_no_ports": snmp.Udp.NoPorts} {
					if value != nil {
						v["node:transport:"+name] = *value
					}
				}
			}
			if net, err := self.Netstat(); ok("transport.extended", err) {
				for name, value := range map[string]*float64{"listen_overflows": net.ListenOverflows, "listen_drops": net.ListenDrops,
					"syn_cookies_sent": net.SyncookiesSent, "syn_cookies_failed": net.SyncookiesFailed, "syn_queue_drops": net.TCPReqQFullDrop,
					"tcp_timeouts": net.TCPTimeouts, "tcp_backlog_drops": net.TCPBacklogDrop} {
					if value != nil {
						v["node:transport:"+name] = *value
					}
				}
			}
		}
		for _, name := range []string{"nf_conntrack_count", "nf_conntrack_max"} {
			if n, err := fs.SysctlInts("net/netfilter/" + name); ok(name, err) && len(n) == 1 {
				v["node:transport:"+name] = float64(n[0])
			}
		}
		v["node:nagi:goroutines"] = float64(runtime.NumGoroutine())
		// Counter baselines only survive consecutive successful observations.
		// A missing source, hotplug or reset never becomes a huge delta or zero.
		previous = counters
		return v
	}
}
