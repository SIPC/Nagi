package alert

import (
	"math"
	"slices"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 告警规则的分层设置（见 docs/monitoring.md#告警）：内置默认 → 全局默认 → 节点；内置默认 → 全局默认 → 项目 → 容器。
// 每一层只写改过的字段，没写的沿用上一层。面板算出每个节点、容器的生效规则（每个字段都有值）下发给节点。

// 规则的字段。
const (
	FieldThreshold        = "threshold"
	FieldFor              = "for_seconds"
	FieldRecoverThreshold = "recover_threshold"
	FieldRecoverSeconds   = "recover_seconds"
	FieldRepeat           = "repeat_minutes"
	FieldWindow           = "window_minutes"
	FieldMinRequests      = "min_requests"
)

// 抖动保护的默认值：这么多分钟内触发这么多轮时暂停逐条通知；之后保持这么多分钟不变才恢复逐条通知。
const (
	DefaultFlapWindowMinutes = 60
	DefaultFlapCount         = 3
	FlapQuietMinutes         = 30
	// InsufficientDataAfter：5xx、4xx 告警中，请求数一直不够这么久，就以「数据不足」结束。
	InsufficientDataAfter = 30 * time.Minute
)

// Def 说明一条规则：哪一层设置、单位、哪些字段有意义。
type Def struct {
	Name string
	// Node：节点的规则（在节点上判断，或面板按节点判断）；否则是容器的规则（按容器、端口映射判断）。
	Node bool
	// Route：按端口映射判断，可以在映射上覆盖（全局 → 项目 → 容器 → 映射）。
	Route bool
	// Unit：percent、seconds、days、count、ratio、conn_rate、connections、mbps、ms，没有阈值时为空。
	Unit string
	// Lower：值越低越糟（证书剩余天数）。
	Lower bool
	// Panel：面板自己判断的整个面板的规则（审计链校验、令牌到期），只有全局一层，不下发给节点。
	Panel bool
	// Off：默认关闭（阈值因环境而异或误报多，需要用户自己打开）。
	Off    bool
	Fields []string
}

var (
	thresholdFields = []string{FieldThreshold, FieldFor, FieldRecoverThreshold, FieldRecoverSeconds, FieldRepeat}
	rateFields      = []string{FieldThreshold, FieldWindow, FieldMinRequests, FieldFor, FieldRecoverThreshold, FieldRecoverSeconds, FieldRepeat}
	// countFields：窗口内的次数（事件数、增量），没有回差阈值，恢复看「连续多久没有新的」。
	countFields = []string{FieldThreshold, FieldWindow, FieldRecoverSeconds, FieldRepeat}
	// eventFields：一次失败就是一轮，下一次成功恢复。
	eventFields = []string{FieldRepeat}
)

// Defs 是全部规则，顺序就是界面上的顺序。
var Defs = []Def{
	{Name: RuleMem, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleDisk, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleInode, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleCPU, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleOffline, Node: true, Unit: "seconds", Fields: []string{FieldThreshold, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleCert, Node: true, Unit: "days", Lower: true, Fields: []string{FieldThreshold, FieldRecoverThreshold, FieldRepeat}},
	{Name: RuleNodeCert, Node: true, Unit: "days", Lower: true, Fields: []string{FieldThreshold, FieldRepeat}},
	{Name: RuleUpgrade, Node: true, Fields: eventFields},
	{Name: RuleLogWrite, Node: true, Unit: "count", Fields: []string{FieldThreshold, FieldWindow, FieldFor, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleLogFull, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleMigration, Node: true, Fields: eventFields},
	{Name: RuleMeshPeer, Node: true, Unit: "seconds", Fields: []string{FieldThreshold, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleMeshLoss, Node: true, Unit: "percent", Off: true, Fields: thresholdFields},
	{Name: RuleMeshRTT, Node: true, Unit: "ms", Off: true, Fields: thresholdFields},
	{Name: RuleWitness, Node: true, Unit: "seconds", Fields: []string{FieldThreshold, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleKernelStorage, Node: true, Unit: "count", Fields: countFields},
	{Name: RuleKernelOOM, Node: true, Unit: "count", Fields: countFields},
	{Name: RuleNIC, Node: true, Unit: "percent", Off: true, Fields: thresholdFields},
	{Name: RuleConntrack, Node: true, Unit: "percent", Fields: thresholdFields},
	{Name: RuleClockSkew, Node: true, Unit: "seconds_skew", Fields: thresholdFields},
	{Name: RuleRestart, Unit: "count", Fields: []string{FieldThreshold, FieldWindow, FieldRepeat}},
	{Name: RuleContainerOOM, Unit: "count", Fields: countFields},
	{Name: RuleHealth, Fields: []string{FieldFor, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleContainerCPU, Unit: "percent", Fields: thresholdFields},
	{Name: RuleErrors, Route: true, Unit: "percent", Fields: rateFields},
	{Name: RuleClientErrors, Route: true, Unit: "percent", Fields: rateFields},
	{Name: RuleTraffic, Route: true, Unit: "ratio", Fields: rateFields},
	{Name: RuleL4Surge, Route: true, Unit: "ratio", Fields: rateFields},
	{Name: RuleL4Active, Route: true, Unit: "connections", Off: true, Fields: thresholdFields},
	{Name: RuleL4Fail, Route: true, Unit: "percent", Fields: rateFields},
	{Name: RuleL4Bandwidth, Route: true, Unit: "mbps", Off: true, Fields: []string{FieldThreshold, FieldWindow, FieldFor, FieldRecoverThreshold, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleL4Listen, Route: true, Fields: []string{FieldFor, FieldRecoverSeconds, FieldRepeat}},
	{Name: RuleSnapshot, Fields: eventFields},
	{Name: RuleSnapshotStale, Unit: "ratio", Fields: []string{FieldThreshold, FieldRepeat}},
	{Name: RuleBackup, Fields: eventFields},
	{Name: RuleBackupLag, Unit: "ratio", Fields: []string{FieldThreshold, FieldRepeat}},
	{Name: RuleEgress, Fields: eventFields},
	{Name: RuleAuditVerify, Panel: true, Fields: eventFields},
	{Name: RuleTokenExpiry, Panel: true, Unit: "days", Lower: true, Off: true, Fields: []string{FieldThreshold, FieldRepeat}},
}

// DefOf 返回规则的说明，没有这条规则时 ok 为 false。
func DefOf(name string) (Def, bool) {
	for _, d := range Defs {
		if d.Name == name {
			return d, true
		}
	}
	return Def{}, false
}

func (d Def) Has(field string) bool { return slices.Contains(d.Fields, field) }

// Recovered 判断一个值有没有回到恢复阈值的好的一侧：要降到恢复阈值以下，等于不算；
// 恢复阈值是 0 时（不能更低）回到 0 就算。lower 的规则（证书剩余天数）反过来，≥ 恢复阈值算。
func (d Def) Recovered(v, recover float64) bool {
	if d.Lower {
		return v >= recover
	}
	return v < recover || recover <= 0 && v <= 0
}

func f(v float64) *float64 { return &v }
func u(v uint32) *uint32   { return &v }
func b(v bool) *bool       { return &v }

// builtin 是内置默认值：触发、恢复（回差）、持续时间和重复提醒间隔（见 docs/monitoring.md#告警 的默认值表）。
func builtin() map[string]*nagiv1.AlertRule {
	const repeat = 240
	return map[string]*nagiv1.AlertRule{
		RuleMem:           {Threshold: f(90), ForSeconds: u(300), RecoverThreshold: f(85), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleDisk:          {Threshold: f(90), ForSeconds: u(120), RecoverThreshold: f(85), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleInode:         {Threshold: f(90), ForSeconds: u(300), RecoverThreshold: f(85), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleCPU:           {Threshold: f(90), ForSeconds: u(600), RecoverThreshold: f(80), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleOffline:       {Threshold: f(60), RecoverSeconds: u(60), RepeatMinutes: u(repeat)},
		RuleCert:          {Threshold: f(7), RecoverThreshold: f(7), RepeatMinutes: u(repeat)},
		RuleNodeCert:      {Threshold: f(7), RepeatMinutes: u(repeat)},
		RuleUpgrade:       {RepeatMinutes: u(0)},
		RuleLogWrite:      {Threshold: f(10), WindowMinutes: u(5), ForSeconds: u(600), RecoverSeconds: u(600), RepeatMinutes: u(repeat)},
		RuleLogFull:       {Threshold: f(90), ForSeconds: u(1800), RecoverThreshold: f(85), RecoverSeconds: u(1800), RepeatMinutes: u(repeat)},
		RuleMigration:     {RepeatMinutes: u(0)},
		RuleMeshPeer:      {Threshold: f(300), RecoverSeconds: u(120), RepeatMinutes: u(repeat)},
		RuleMeshLoss:      {Threshold: f(5), ForSeconds: u(600), RecoverThreshold: f(2), RecoverSeconds: u(600), RepeatMinutes: u(repeat)},
		RuleMeshRTT:       {Threshold: f(500), ForSeconds: u(600), RecoverThreshold: f(300), RecoverSeconds: u(600), RepeatMinutes: u(repeat)},
		RuleWitness:       {Threshold: f(300), RecoverSeconds: u(60), RepeatMinutes: u(repeat)},
		RuleKernelStorage: {Threshold: f(2), WindowMinutes: u(10), RecoverSeconds: u(3600), RepeatMinutes: u(repeat)},
		RuleKernelOOM:     {Threshold: f(1), WindowMinutes: u(10), RecoverSeconds: u(1800), RepeatMinutes: u(repeat)},
		RuleNIC:           {Threshold: f(0.1), ForSeconds: u(600), RecoverThreshold: f(0.05), RecoverSeconds: u(600), RepeatMinutes: u(repeat)},
		RuleConntrack:     {Threshold: f(80), ForSeconds: u(300), RecoverThreshold: f(70), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleClockSkew:     {Threshold: f(5), ForSeconds: u(300), RecoverThreshold: f(2), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleRestart:       {Threshold: f(3), WindowMinutes: u(10), RepeatMinutes: u(repeat)},
		RuleContainerOOM:  {Threshold: f(1), WindowMinutes: u(10), RecoverSeconds: u(1800), RepeatMinutes: u(repeat)},
		RuleHealth:        {ForSeconds: u(0), RecoverSeconds: u(60), RepeatMinutes: u(repeat)},
		RuleContainerCPU:  {Threshold: f(90), ForSeconds: u(600), RecoverThreshold: f(80), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleErrors:        {Threshold: f(5), WindowMinutes: u(5), MinRequests: f(20), ForSeconds: u(0), RecoverThreshold: f(2), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleClientErrors:  {Threshold: f(50), WindowMinutes: u(5), MinRequests: f(200), ForSeconds: u(0), RecoverThreshold: f(30), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleTraffic:       {Threshold: f(10), WindowMinutes: u(5), MinRequests: f(50), ForSeconds: u(0), RecoverThreshold: f(3), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleL4Surge:       {Threshold: f(10), WindowMinutes: u(5), MinRequests: f(20), ForSeconds: u(120), RecoverThreshold: f(3), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleL4Active:      {Threshold: f(5000), ForSeconds: u(300), RecoverThreshold: f(4000), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleL4Fail:        {Threshold: f(20), WindowMinutes: u(5), MinRequests: f(20), ForSeconds: u(0), RecoverThreshold: f(5), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleL4Bandwidth:   {Threshold: f(100), WindowMinutes: u(5), ForSeconds: u(120), RecoverThreshold: f(80), RecoverSeconds: u(300), RepeatMinutes: u(repeat)},
		RuleL4Listen:      {ForSeconds: u(30), RecoverSeconds: u(60), RepeatMinutes: u(repeat)},
		RuleSnapshot:      {RepeatMinutes: u(repeat)},
		RuleSnapshotStale: {Threshold: f(2), RepeatMinutes: u(repeat)},
		RuleBackup:        {RepeatMinutes: u(repeat)},
		RuleBackupLag:     {Threshold: f(2), RepeatMinutes: u(repeat)},
		RuleEgress:        {RepeatMinutes: u(10)},
		RuleAuditVerify:   {RepeatMinutes: u(0)},
		RuleTokenExpiry:   {Threshold: f(7), RepeatMinutes: u(0)},
	}
}

// Builtin 返回内置默认值（除了 Def.Off 的规则都开着，每个有意义的字段都有值）。
func Builtin() map[string]*nagiv1.AlertRule {
	out := builtin()
	for _, d := range Defs {
		out[d.Name].Enabled = b(!d.Off)
	}
	return out
}

// overlay 把 top 里写了的字段盖到 base 上（base 会被修改）。
func overlay(base, top *nagiv1.AlertRule) {
	if top == nil {
		return
	}
	if top.Enabled != nil {
		base.Enabled = b(*top.Enabled)
	}
	if top.Threshold != nil {
		base.Threshold = f(*top.Threshold)
	}
	if top.ForSeconds != nil {
		base.ForSeconds = u(*top.ForSeconds)
	}
	if top.RecoverThreshold != nil {
		base.RecoverThreshold = f(*top.RecoverThreshold)
	}
	if top.RecoverSeconds != nil {
		base.RecoverSeconds = u(*top.RecoverSeconds)
	}
	if top.RepeatMinutes != nil {
		base.RepeatMinutes = u(*top.RepeatMinutes)
	}
	if top.WindowMinutes != nil {
		base.WindowMinutes = u(*top.WindowMinutes)
	}
	if top.MinRequests != nil {
		base.MinRequests = f(*top.MinRequests)
	}
}

// Merge 按层合并：从内置默认值开始，依次盖上各层写了的字段。返回全部规则，每个有意义的字段都有值。
// 恢复阈值比触发阈值还「糟」时（下层改了触发阈值，上层的恢复阈值没跟着改）按触发阈值算，即没有回差。
func Merge(layers ...map[string]*nagiv1.AlertRule) map[string]*nagiv1.AlertRule {
	out := Builtin()
	for _, d := range Defs {
		r := out[d.Name]
		for _, l := range layers {
			overlay(r, l[d.Name])
		}
		if d.Has(FieldRecoverThreshold) && r.Threshold != nil && r.RecoverThreshold != nil {
			if d.Lower && *r.RecoverThreshold < *r.Threshold || !d.Lower && *r.RecoverThreshold > *r.Threshold {
				r.RecoverThreshold = f(*r.Threshold)
			}
		}
	}
	return out
}

// Effective 返回下发给节点的生效规则：node 为 true 时是节点的规则，否则是容器的规则。
func Effective(policy *nagiv1.AlertPolicy, node bool, layers ...map[string]*nagiv1.AlertRule) *nagiv1.EffectiveAlertRules {
	all := Merge(append([]map[string]*nagiv1.AlertRule{policy.GetRules()}, layers...)...)
	out := &nagiv1.EffectiveAlertRules{Rules: map[string]*nagiv1.AlertRule{}, Language: Language(policy.GetLanguage()),
		FlapWindowMinutes: or(policy.GetFlapWindowMinutes(), DefaultFlapWindowMinutes), FlapCount: or(policy.GetFlapCount(), DefaultFlapCount)}
	for _, d := range Defs {
		if d.Node == node && !d.Panel {
			out.Rules[d.Name] = all[d.Name]
		}
	}
	if node {
		out.Notifications = NodeNotices(policy)
	}
	return out
}

// Language 把通知语言规范成 zh-CN 或 en。
func Language(s string) string {
	if l, ok := i18n.Match(s); ok && l == i18n.En {
		return string(i18n.En)
	}
	return string(i18n.Zh)
}

// Rule 取生效规则里的一条，没有时（旧版本面板）用内置默认值。
func Rule(rules map[string]*nagiv1.AlertRule, name string) *nagiv1.AlertRule {
	if r := rules[name]; r != nil {
		return r
	}
	return Merge()[name]
}

// On 判断生效规则开着没有。
func On(r *nagiv1.AlertRule) bool { return r == nil || r.Enabled == nil || r.GetEnabled() }

// ---- 检查 ----

type bound struct{ min, max float64 }

var fieldBounds = map[string]bound{
	FieldFor: {0, 86400}, FieldRecoverSeconds: {0, 86400}, FieldRepeat: {0, 10080}, FieldWindow: {1, 60}, FieldMinRequests: {0, 1e6},
}

var unitBounds = map[string]bound{
	"percent": {0, 100}, "seconds": {10, 86400}, "days": {1, 365}, "count": {1, 1000}, "ratio": {1, 1000},
	"connections": {1, 1e7}, "mbps": {1, 1e5}, "ms": {1, 60000}, "seconds_skew": {1, 3600},
}

// CheckLayer 检查一层设置：规则名、字段是否适用、范围；再检查和 below（下面各层）合并以后，恢复阈值不比触发阈值糟。
// scope 是这一层：空（全局，什么规则都行）、node、project、container、route（只有按映射判断的规则）。
func CheckLayer(layer map[string]*nagiv1.AlertRule, scope string, below ...map[string]*nagiv1.AlertRule) error {
	for name, r := range layer {
		d, ok := DefOf(name)
		if !ok {
			return i18n.Errorf("没有告警规则 %s", name)
		}
		allowed := scope == "" || scope == "node" && d.Node || (scope == "project" || scope == "container") && !d.Node && !d.Panel || scope == "route" && d.Route
		if !allowed {
			return i18n.Errorf("告警规则 %s 不能在这里设置", name)
		}
		if r == nil {
			continue
		}
		set := map[string]*float64{}
		if r.Threshold != nil {
			set[FieldThreshold] = r.Threshold
		}
		if r.RecoverThreshold != nil {
			set[FieldRecoverThreshold] = r.RecoverThreshold
		}
		if r.MinRequests != nil {
			set[FieldMinRequests] = r.MinRequests
		}
		for field, p := range map[string]*uint32{FieldFor: r.ForSeconds, FieldRecoverSeconds: r.RecoverSeconds, FieldRepeat: r.RepeatMinutes, FieldWindow: r.WindowMinutes} {
			if p != nil {
				set[field] = f(float64(*p))
			}
		}
		for field, p := range set {
			if !d.Has(field) {
				return i18n.Errorf("告警规则 %s 没有 %s 这一项", name, field)
			}
			v := *p
			bd, ok := fieldBounds[field]
			if field == FieldThreshold || field == FieldRecoverThreshold {
				bd, ok = unitBounds[d.Unit]
				if field == FieldRecoverThreshold {
					bd.min = 0
				}
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || ok && (v < bd.min || v > bd.max) {
				return i18n.Errorf("告警规则 %s 的 %s 须在 %v–%v 之间", name, field, bd.min, bd.max)
			}
		}
	}
	for name, r := range layer {
		d, _ := DefOf(name)
		if r == nil || !d.Has(FieldRecoverThreshold) || r.Threshold == nil && r.RecoverThreshold == nil {
			continue
		}
		// 这一层盖在下面各层上以后，恢复阈值不能比触发阈值糟（Merge 会悄悄改掉，这里要明确拒绝）。
		m := Merge(below...)[name]
		overlay(m, r)
		if d.Lower && m.GetRecoverThreshold() < m.GetThreshold() {
			return i18n.Errorf("告警规则 %s 的恢复阈值不能小于触发阈值", name)
		}
		if !d.Lower && m.GetRecoverThreshold() > m.GetThreshold() {
			return i18n.Errorf("告警规则 %s 的恢复阈值不能大于触发阈值", name)
		}
	}
	return nil
}

// Clean 去掉空的规则（一个字段都没写），返回 nil 表示全部沿用。
func Clean(layer map[string]*nagiv1.AlertRule) map[string]*nagiv1.AlertRule {
	var out map[string]*nagiv1.AlertRule
	for k, r := range layer {
		if r == nil || proto.Equal(r, &nagiv1.AlertRule{}) {
			continue
		}
		if out == nil {
			out = map[string]*nagiv1.AlertRule{}
		}
		out[k] = proto.CloneOf(r)
	}
	return out
}

// ---- 和旧版本的规则（AlertRules）互相换算 ----

// FromLegacy 把旧版本的规则换成一层设置：写了的阈值、关掉的开关。改了触发阈值时恢复阈值按内置默认的回差跟着改。
func FromLegacy(r *nagiv1.AlertRules, names []string) map[string]*nagiv1.AlertRule {
	if r == nil {
		return nil
	}
	out := map[string]*nagiv1.AlertRule{}
	get := func(name string) *nagiv1.AlertRule {
		if out[name] == nil {
			out[name] = &nagiv1.AlertRule{}
		}
		return out[name]
	}
	def := builtin()
	threshold := func(name string, v uint32) {
		if v == 0 || !slices.Contains(names, name) {
			return
		}
		x := get(name)
		x.Threshold = f(float64(v))
		if d := def[name]; d.RecoverThreshold != nil {
			x.RecoverThreshold = f(max(0, float64(v)-(d.GetThreshold()-d.GetRecoverThreshold())))
			if name == RuleCert {
				x.RecoverThreshold = f(float64(v))
			}
		}
	}
	threshold(RuleDisk, r.GetDiskPercent())
	threshold(RuleMem, r.GetMemPercent())
	threshold(RuleRestart, r.GetRestartCount())
	threshold(RuleCert, r.GetCertDays())
	threshold(RuleOffline, r.GetOfflineSeconds())
	threshold(RuleErrors, r.GetErrorPercent())
	if v := r.GetMemMinutes(); v != 0 && slices.Contains(names, RuleMem) {
		get(RuleMem).ForSeconds = u(v * 60)
	}
	if v := r.GetRestartMinutes(); v != 0 && slices.Contains(names, RuleRestart) {
		get(RuleRestart).WindowMinutes = u(v)
	}
	for k, on := range r.GetEnabled() {
		if !on && slices.Contains(names, k) {
			get(k).Enabled = b(false)
		}
	}
	return Clean(out)
}

// Legacy 把生效规则换成旧版本节点认识的规则（阈值取整，0 在旧版本里表示默认值，所以至少写 1）。
func Legacy(eff *nagiv1.EffectiveAlertRules) *nagiv1.AlertRules {
	if eff == nil {
		return nil
	}
	rules := eff.GetRules()
	n := func(name string) uint32 { return uint32(max(1, math.Round(Rule(rules, name).GetThreshold()))) }
	out := &nagiv1.AlertRules{Enabled: map[string]bool{}}
	for name, r := range rules {
		out.Enabled[name] = On(r)
		switch name {
		case RuleDisk:
			out.DiskPercent = n(name)
		case RuleMem:
			out.MemPercent = n(name)
			out.MemMinutes = max(1, r.GetForSeconds()/60)
		case RuleRestart:
			out.RestartCount, out.RestartMinutes = n(name), max(1, r.GetWindowMinutes())
		case RuleCert:
			out.CertDays = n(name)
		case RuleOffline:
			out.OfflineSeconds = n(name)
		case RuleErrors:
			out.ErrorPercent = n(name)
		}
	}
	return out
}

// LegacyEffective 把旧版本面板下发的规则换成生效规则（节点连着旧版本面板时用）。
func LegacyEffective(r *nagiv1.AlertRules, node bool) *nagiv1.EffectiveAlertRules {
	names := ContainerRules
	if node {
		names = NodeRules
	}
	return Effective(nil, node, FromLegacy(r, names))
}

// Strictest 合并几个面板给同一个节点的规则：任何一个开着就判断，阈值取更容易告警的、更难恢复的。
func Strictest(a, b *nagiv1.AlertRule, lower bool) *nagiv1.AlertRule {
	if a == nil {
		return proto.CloneOf(b)
	}
	if b == nil {
		return proto.CloneOf(a)
	}
	x := proto.CloneOf(a)
	x.Enabled = bp(On(a) || On(b))
	worse := func(p, q *float64) *float64 {
		if p == nil || q == nil {
			if p == nil {
				return q
			}
			return p
		}
		if lower {
			return f(max(*p, *q))
		}
		return f(min(*p, *q))
	}
	x.Threshold = worse(a.Threshold, b.Threshold)
	x.RecoverThreshold = worse(a.RecoverThreshold, b.RecoverThreshold)
	minU := func(p, q *uint32) *uint32 {
		if p == nil || q == nil {
			if p == nil {
				return q
			}
			return p
		}
		return u(min(*p, *q))
	}
	maxU := func(p, q *uint32) *uint32 {
		if p == nil || q == nil {
			if p == nil {
				return q
			}
			return p
		}
		return u(max(*p, *q))
	}
	x.ForSeconds = minU(a.ForSeconds, b.ForSeconds)
	x.RecoverSeconds = maxU(a.RecoverSeconds, b.RecoverSeconds)
	x.WindowMinutes = maxU(a.WindowMinutes, b.WindowMinutes)
	if a.MinRequests != nil && b.MinRequests != nil {
		x.MinRequests = f(min(*a.MinRequests, *b.MinRequests))
	}
	return x
}

func bp(v bool) *bool { return &v }
