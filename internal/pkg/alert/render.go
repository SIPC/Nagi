package alert

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 通知文字（见 docs/monitoring.md#消息格式）：规则告警按结构化字段拼出，出现时写当前值、阈值和超标了多久；
// 恢复时写恢复时的值、恢复阈值、稳定了多久，以及本轮一共持续了多久和峰值。语言按 Alert.language。

func isEn(a *nagiv1.Alert) bool { return a.GetLanguage() == "en" }

// Num 写一个数：最多一位小数，去掉多余的 0。
func Num(v float64) string {
	return strconv.FormatFloat(float64(int64(v*10+sign(v)*0.5))/10, 'f', -1, 64)
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// Duration 写一段时间：秒、分钟、小时、天，最多两级。
func Duration(d time.Duration, en bool) string {
	s := int64(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	type unit struct {
		n      int64
		zh, en string
	}
	units := []unit{{86400, "天", "d"}, {3600, "小时", "h"}, {60, "分钟", "min"}, {1, "秒", "s"}}
	var parts []string
	for _, u := range units {
		if s >= u.n || u.n == 1 && len(parts) == 0 {
			n := s / u.n
			s -= n * u.n
			if en {
				parts = append(parts, fmt.Sprintf("%d %s", n, u.en))
			} else {
				parts = append(parts, fmt.Sprintf("%d %s", n, u.zh))
			}
			if len(parts) == 2 || u.n <= 60 && len(parts) == 1 && s == 0 {
				break
			}
		} else if len(parts) > 0 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func secs(n uint32) time.Duration { return time.Duration(n) * time.Second }

// unitOf 是值的写法：百分比加 %，天、秒、次、倍加单位。
func unitOf(a *nagiv1.Alert, v float64) string {
	d, _ := DefOf(a.GetRule())
	en := isEn(a)
	switch d.Unit {
	case "percent":
		return Num(v) + "%"
	case "days":
		if en {
			return Num(v) + " days"
		}
		return Num(v) + " 天"
	case "seconds":
		if en {
			return Num(v) + " s"
		}
		return Num(v) + " 秒"
	case "count":
		if en {
			return Num(v) + " times"
		}
		return Num(v) + " 次"
	case "ratio":
		if en {
			return Num(v) + "×"
		}
		return Num(v) + " 倍"
	case "connections":
		if en {
			return Num(v) + " connections"
		}
		return Num(v) + " 个连接"
	case "mbps":
		return Num(v) + " Mbit/s"
	case "ms":
		return Num(v) + " ms"
	case "seconds_skew":
		if en {
			return Num(v) + " s"
		}
		return Num(v) + " 秒"
	}
	return Num(v)
}

// what 是规则告警的主语：「内存使用」「磁盘 /srv 使用」「映射 api 的 5xx 比例」等。
func what(a *nagiv1.Alert) string {
	s, en := a.GetSubject(), isEn(a)
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	switch a.GetRule() {
	case RuleMem:
		return pick("内存使用", "Memory usage")
	case RuleCPU:
		return pick("CPU 使用", "CPU usage")
	case RuleDisk:
		if s == "" {
			return pick("磁盘使用", "Disk usage")
		}
		return pick("磁盘 "+s+" 使用", "Disk "+s+" usage")
	case RuleContainerCPU:
		return pick(s+" 的 CPU 使用（占上限）", "CPU usage of "+s+" (of its limit)")
	case RuleErrors:
		return pick("映射 "+s+" 的 5xx 比例", "5xx share of mapping "+s)
	case RuleClientErrors:
		return pick("映射 "+s+" 的 4xx 比例", "4xx share of mapping "+s)
	case RuleTraffic:
		return pick("映射 "+s+" 的请求量", "Requests to mapping "+s)
	case RuleInode:
		return pick("磁盘 "+s+" 的 inode 使用", "Inode usage on "+s)
	case RuleLogFull:
		return pick("日志库容量使用", "Log store usage")
	case RuleMeshLoss:
		return pick("到 "+s+" 的节点网络丢包", "Node network loss to "+s)
	case RuleMeshRTT:
		return pick("到 "+s+" 的节点网络延迟", "Node network latency to "+s)
	case RuleNIC:
		return pick("网卡 "+s+" 的错误和丢包", "Errors and drops on "+s)
	case RuleConntrack:
		return pick("连接跟踪表使用", "Connection tracking table usage")
	case RuleClockSkew:
		return pick("节点时钟偏差", "Node clock skew")
	case RuleL4Surge:
		return pick("映射 "+s+" 的新连接", "New connections to mapping "+s)
	case RuleL4Active:
		return pick("映射 "+s+" 的活跃连接", "Active connections on mapping "+s)
	case RuleL4Fail:
		return pick("映射 "+s+" 的连接失败比例（连不上容器或转发出错）", "Failed connection share of mapping "+s+" (container unreachable or forwarding error)")
	case RuleL4Bandwidth:
		return pick("映射 "+s+" 的带宽", "Bandwidth of mapping "+s)
	}
	return s
}

// countText 是「窗口内的次数」类规则（日志库写入失败、内核磁盘错误、OOM）的出现文字。
func countText(a *nagiv1.Alert, en bool) string {
	s, n, w := a.GetSubject(), Num(a.GetValue()), Num(a.GetThreshold())
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	switch a.GetRule() {
	case RuleLogWrite:
		return pick("日志库写入失败 "+n+" 次（阈值 "+w+" 次）", "Log store writes failed "+n+" times (threshold "+w+")")
	case RuleKernelStorage:
		return pick("内核报告磁盘错误 "+n+" 次（阈值 "+w+" 次）", "Kernel reported "+n+" storage errors (threshold "+w+")")
	case RuleKernelOOM:
		return pick("内核因内存不足杀掉进程 "+n+" 次", "Kernel OOM-killed processes "+n+" times")
	case RuleContainerOOM:
		return pick(s+" 因内存不足被杀 "+n+" 次", s+" was OOM-killed "+n+" times")
	}
	return s
}

// countRecovered 是这类规则的恢复文字。
func countRecovered(a *nagiv1.Alert, en bool) string {
	quiet := Duration(secs(a.GetRecoverSeconds()), en)
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	switch a.GetRule() {
	case RuleLogWrite:
		return pick("日志库写入已恢复，"+quiet+"没有新的失败", "Log store writes recovered: no failures for "+quiet)
	case RuleKernelStorage:
		return pick("内核 "+quiet+"没有再报告磁盘错误", "No kernel storage errors for "+quiet)
	case RuleKernelOOM:
		return pick("内核 "+quiet+"没有再因内存不足杀进程", "No kernel OOM kills for "+quiet)
	case RuleContainerOOM:
		return pick(a.GetSubject()+" "+quiet+"没有再因内存不足被杀", a.GetSubject()+" not OOM-killed for "+quiet)
	}
	return a.GetSubject()
}

// Render 返回规则告警的标题和补充说明，非规则告警（事件、一次性通知、旧版本节点）原样返回。
func Render(a *nagiv1.Alert) (title, detail string) {
	switch a.GetStatus() {
	case "firing", "resolved":
	default: // 已结束、反复出现、一次性通知：节点拼好了文字
		return a.GetTitle(), a.GetMessage()
	}
	valueless := map[string]bool{RuleHealth: true, RuleOffline: true, RuleL4Listen: true, RuleMeshPeer: true, RuleWitness: true,
		RuleUpgrade: true, RuleAuditVerify: true, RuleMigration: true}
	if a.GetRule() == "" || a.Value == nil && !valueless[a.GetRule()] {
		if isEn(a) {
			return i18n.Text(i18n.En, a.GetTitle()), a.GetMessage()
		}
		return a.GetTitle(), a.GetMessage()
	}
	en := isEn(a)
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	v, th, rec := a.GetValue(), a.GetThreshold(), a.GetRecoverThreshold()
	dur := Duration(secs(a.GetDurationSeconds()), en)
	stable := ""
	if a.GetRecoverSeconds() > 0 {
		stable = pick("，已稳定 ", ", stable for ") + Duration(secs(a.GetRecoverSeconds()), en)
	}
	resolved := a.GetStatus() == "resolved"
	lasted := ""
	if resolved {
		lasted = pick("本次持续 ", "Lasted ") + dur
		if a.Peak != nil && a.GetRule() != RuleOffline && a.GetRule() != RuleHealth && a.GetRule() != RuleRestart {
			if a.GetRule() == RuleCert {
				lasted += pick("，最少 ", ", lowest ") + unitOf(a, a.GetPeak())
			} else {
				lasted += pick("，最高 ", ", peak ") + unitOf(a, a.GetPeak())
			}
		}
	}
	switch a.GetRule() {
	case RuleOffline:
		if resolved {
			return pick("节点已重新连上面板", "Node reconnected to the panel") + stable, pick("本次失联 ", "Was unreachable for ") + dur
		}
		return pick("节点失联已 ", "Node unreachable for ") + dur + pick("（阈值 ", " (threshold ") + unitOf(a, th) + pick("）", ")"), a.GetMessage()
	case RuleHealth:
		if resolved {
			return pick(a.GetSubject()+" 健康检查已通过", a.GetSubject()+" passes its health check again") + stable, lasted
		}
		t := pick(a.GetSubject()+" 健康检查失败", a.GetSubject()+" failed its health check")
		if a.GetDurationSeconds() > 0 {
			t += pick("，已 ", " for ") + dur
		}
		return t, a.GetMessage()
	case RuleCert:
		if resolved {
			return pick("域名 "+a.GetSubject()+" 的证书已续期，"+Num(v)+" 天后过期", "Certificate for "+a.GetSubject()+" renewed, expires in "+Num(v)+" days"), lasted
		}
		return pick("域名 "+a.GetSubject()+" 的证书 "+Num(v)+" 天后过期（阈值 "+Num(th)+" 天）",
			"Certificate for "+a.GetSubject()+" expires in "+Num(v)+" days (threshold "+Num(th)+" days)"), a.GetMessage()
	case RuleRestart:
		if resolved {
			return pick(a.GetSubject()+" 已恢复稳定，最近一个统计窗口内没有重启", a.GetSubject()+" is stable again: no restarts in the last window"), lasted
		}
		return pick(a.GetSubject()+" 重启了 "+Num(v)+" 次（阈值 "+Num(th)+" 次）", a.GetSubject()+" restarted "+Num(v)+" times (threshold "+Num(th)+")"), a.GetMessage()
	case RuleLogWrite, RuleKernelStorage, RuleKernelOOM, RuleContainerOOM:
		if resolved {
			return countRecovered(a, en), lasted
		}
		return countText(a, en), a.GetMessage()
	case RuleL4Listen:
		if resolved {
			return pick("映射 "+a.GetSubject()+" 的端口已恢复监听", "Mapping "+a.GetSubject()+" is listening again") + stable, lasted
		}
		return pick("映射 "+a.GetSubject()+" 的端口无法监听", "Mapping "+a.GetSubject()+" cannot listen on its port"), a.GetMessage()
	case RuleMeshPeer:
		if resolved {
			return pick("到 "+a.GetSubject()+" 的节点网络已恢复", "Node network to "+a.GetSubject()+" restored") + stable, lasted
		}
		return pick("到 "+a.GetSubject()+" 的节点网络不通已 "+dur+"（双方都连着面板）", "No node network path to "+a.GetSubject()+" for "+dur+" (both connected to the panel)"), a.GetMessage()
	case RuleWitness:
		if resolved {
			return pick(a.GetSubject()+" 的见证人已恢复回应", "Witness for "+a.GetSubject()+" responds again") + stable, lasted
		}
		return pick(a.GetSubject()+" 的见证人已 "+dur+"没有回应", "Witness for "+a.GetSubject()+" has not responded for "+dur), a.GetMessage()
	case RuleUpgrade:
		if resolved {
			return pick("节点 "+a.GetNode()+" 已升级成功", "Node "+a.GetNode()+" upgraded successfully"), lasted
		}
		return pick("节点 "+a.GetNode()+" 升级到 "+a.GetSubject()+" 失败", "Node "+a.GetNode()+" failed to upgrade to "+a.GetSubject()), a.GetMessage()
	case RuleAuditVerify:
		if resolved {
			return pick("审计链校验已通过", "Audit chain verification passes again"), lasted
		}
		return pick("审计链定时校验未通过", "Scheduled audit chain verification failed"), a.GetMessage()
	case RuleMigration:
		if resolved {
			return pick(a.GetSubject()+" 迁移已完成", a.GetSubject()+" migration completed"), lasted
		}
		return pick(a.GetSubject()+" 迁移失败", a.GetSubject()+" migration failed"), a.GetMessage()
	case RuleTokenExpiry:
		if resolved {
			return pick("访问令牌 "+a.GetSubject()+" 已续期或删除", "Access token "+a.GetSubject()+" renewed or deleted"), lasted
		}
		return pick("访问令牌 "+a.GetSubject()+" "+Num(v)+" 天后过期", "Access token "+a.GetSubject()+" expires in "+Num(v)+" days"), a.GetMessage()
	case RuleNodeCert:
		if resolved {
			return pick("节点证书已续期", "Node certificate renewed"), lasted
		}
		return pick("节点证书续期失败，"+Num(v)+" 天后过期", "Node certificate renewal failed; expires in "+Num(v)+" days"), a.GetMessage()
	case RuleSnapshotStale, RuleBackupLag:
		subject := a.GetSubject()
		if resolved {
			if a.GetRule() == RuleBackupLag {
				return pick(subject+" 的副本已重新同步", "Replica of "+subject+" synced again"), lasted
			}
			return pick(subject+" 已重新成功快照", subject+" snapshotted successfully again"), lasted
		}
		if a.GetRule() == RuleBackupLag {
			return pick(subject+" 的副本已 "+dur+"没有同步成功（阈值 "+Num(th)+" 个间隔）", "Replica of "+subject+" not synced for "+dur+" (threshold "+Num(th)+" intervals)"), a.GetMessage()
		}
		return pick(subject+" 已 "+dur+"没有成功快照（阈值 "+Num(th)+" 个间隔）", subject+" has had no successful snapshot for "+dur+" (threshold "+Num(th)+" intervals)"), a.GetMessage()
	}
	d, _ := DefOf(a.GetRule())
	if resolved {
		verb := pick("降到 ", " down to ")
		if d.Lower {
			verb = pick("回到 ", " back to ")
		}
		return what(a) + verb + unitOf(a, v) + pick("（恢复阈值 ", " (recovery threshold ") + unitOf(a, rec) + stable + pick("）", ")"), lasted
	}
	over := pick("，超过 ", ", above ")
	if d.Lower {
		over = pick("，低于 ", ", below ")
	}
	t := what(a) + pick(" ", " ") + unitOf(a, v) + over + unitOf(a, th)
	if a.GetDurationSeconds() > 0 {
		t += pick(" 已 ", " for ") + dur
	}
	return t, a.GetMessage()
}
