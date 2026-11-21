package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 审计设置（「设置 → 安全」，见 docs/audit.md）：终端录制、保留时间、日志外送、哈希链状态。

const (
	settingRetention     = "audit_retention_days"
	settingLongRetention = "audit_long_retention_days"
	// settingSinks 是外送目标（JSON），用主密钥加密（附加数据 sinksAD）。
	settingSinks = "audit_sinks"
	sinksAD      = "audit-sinks"
)

// Retention 读出保留设置。
func (a *API) Retention(ctx context.Context) audit.Retention {
	r := audit.DefaultRetention
	if v, err := a.Q.GetSetting(ctx, settingRetention); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.Normal = time.Duration(n) * 24 * time.Hour
		}
	}
	if v, err := a.Q.GetSetting(ctx, settingLongRetention); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.Long = time.Duration(n) * 24 * time.Hour
		}
	}
	return r
}

// Sinks 读出外送目标（解密后的）。
func (a *API) Sinks(ctx context.Context) []audit.SinkConfig {
	v, err := a.Q.GetSetting(ctx, settingSinks)
	if err != nil || v == "" {
		return nil
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	plain, err := a.Sealer.Open(sealed, []byte(sinksAD))
	if err != nil {
		a.Log.Warn("外送配置无法解密", "err", err)
		return nil
	}
	var out []audit.SinkConfig
	_ = json.Unmarshal(plain, &out)
	return out
}

func (a *API) saveSinks(ctx context.Context, sinks []audit.SinkConfig) error {
	b, _ := json.Marshal(sinks)
	sealed, err := a.Sealer.Seal(b, []byte(sinksAD))
	if err != nil {
		return err
	}
	return a.Q.SetSetting(ctx, store.SetSettingParams{Key: settingSinks, Value: base64.StdEncoding.EncodeToString(sealed)})
}

func (s *settingsService) GetAuditSettings(ctx context.Context, _ *connect.Request[nagiv1.GetAuditSettingsRequest]) (*connect.Response[nagiv1.AuditSettings], error) {
	return connect.NewResponse(s.auditSettings(ctx)), nil
}

func (a *API) auditSettings(ctx context.Context) *nagiv1.AuditSettings {
	r := a.Retention(ctx)
	out := &nagiv1.AuditSettings{
		Recording: a.RecordingEnabled(ctx), RetentionDays: int32(r.Normal / (24 * time.Hour)), LongRetentionDays: int32(r.Long / (24 * time.Hour)),
		Chain: a.chainStatus(ctx), AlertTotpFailures: a.Auth.AlertEnabled(ctx, auth.NoticeTOTP),
		AlertLoginRatelimit: a.Auth.AlertEnabled(ctx, auth.NoticeRateLimit), AlertAdminNetwork: a.Auth.AlertEnabled(ctx, auth.NoticeNewNetwork),
	}
	var status map[string]audit.SinkStatus
	if a.Shipper != nil {
		status = a.Shipper.Status()
	}
	for _, sk := range a.Sinks(ctx) {
		p := &nagiv1.AuditSink{
			Id: sk.ID, Name: sk.Name, Type: sk.Type, Address: sk.Address, Ca: sk.CA, Url: sk.URL, Endpoint: sk.Endpoint, Region: sk.Region,
			Bucket: sk.Bucket, Prefix: sk.Prefix, Recordings: sk.Recordings, HasSecret: sk.SecretKey != "",
		}
		for k := range sk.Headers {
			p.Headers = append(p.Headers, &nagiv1.AuditSinkHeader{Name: k})
		}
		if st, ok := status[sk.ID]; ok {
			if !st.LastSuccess.IsZero() {
				p.LastSuccess = timestamppb.New(st.LastSuccess)
			}
			p.LastError, p.Failing = st.LastError, !st.FailingFrom.IsZero()
		}
		out.Sinks = append(out.Sinks, p)
	}
	return out
}

func (a *API) chainStatus(ctx context.Context) *nagiv1.AuditChainStatus {
	c := &nagiv1.AuditChainStatus{}
	if r, ok := a.view().LastVerify(ctx); ok {
		c.VerifiedAt, c.Ok, c.Records, c.Reason = timestamppb.New(r.Time), r.OK(), r.Records, r.Reason
	}
	if an, ok := a.view().LastAnchor(ctx); ok {
		c.AnchorAt, c.AnchorId, c.AnchorHash = timestamppb.New(an.Time), an.ID, an.Hash
	}
	return c
}

// sinkFrom 把界面的目标转成配置。密钥和请求头的值留空时沿用已保存的，但目标的类型或地址变了时不沿用：
// 否则偷到管理员会话的人把已有目标改到自己的地址，面板就把保存的令牌发过去。
func sinkFrom(p *nagiv1.AuditSink, old map[string]audit.SinkConfig) (audit.SinkConfig, error) {
	sk := audit.SinkConfig{
		ID: p.GetId(), Name: strings.TrimSpace(p.GetName()), Type: p.GetType(), Address: strings.TrimSpace(p.GetAddress()), CA: strings.TrimSpace(p.GetCa()),
		URL: strings.TrimSpace(p.GetUrl()), Endpoint: strings.TrimSpace(p.GetEndpoint()), Region: strings.TrimSpace(p.GetRegion()),
		Bucket: strings.TrimSpace(p.GetBucket()), Prefix: strings.TrimSpace(p.GetPrefix()), AccessKey: strings.TrimSpace(p.GetAccessKey()),
		SecretKey: p.GetSecretKey(), Recordings: p.GetRecordings(),
	}
	if sk.ID == "" {
		id, _ := auth.RandomToken()
		sk.ID = id[:12]
	}
	prev, ok := old[sk.ID]
	if ok && !sameDestination(prev, sk) {
		prev = audit.SinkConfig{}
	}
	if sk.SecretKey == "" {
		sk.SecretKey = prev.SecretKey
	}
	if sk.AccessKey == "" {
		sk.AccessKey = prev.AccessKey
	}
	if len(p.GetHeaders()) > 0 {
		sk.Headers = map[string]string{}
		for _, h := range p.GetHeaders() {
			name := strings.TrimSpace(h.GetName())
			if name == "" {
				continue
			}
			v := h.GetValue()
			if v == "" {
				if v = prev.Headers[name]; v == "" {
					return sk, i18n.Errorf("请填写请求头 %s 的值（接收地址变了时要重新填写）", name)
				}
			}
			sk.Headers[name] = v
		}
	}
	if sk.Name == "" {
		return sk, i18n.New("外送目标的名称不能为空")
	}
	switch sk.Type {
	case "syslog":
		if !strings.Contains(sk.Address, ":") {
			return sk, i18n.New("syslog 地址的格式是 主机:端口")
		}
	case "http":
		if !strings.HasPrefix(sk.URL, "https://") && !strings.HasPrefix(sk.URL, "http://") {
			return sk, i18n.New("HTTP 地址须以 http:// 或 https:// 开头")
		}
	case "s3":
		if sk.Endpoint == "" || sk.Bucket == "" || sk.AccessKey == "" || sk.SecretKey == "" {
			return sk, i18n.New("S3 需要填写地址、存储桶、Access Key 和 Secret Key")
		}
	default:
		return sk, i18n.New("外送目标的类型只能是 syslog、http 或 s3")
	}
	return sk, nil
}

// sameDestination 判断两个配置是不是发往同一个地方：类型和地址都没变。
func sameDestination(a, b audit.SinkConfig) bool {
	return a.Type == b.Type && a.URL == b.URL && a.Address == b.Address && a.Endpoint == b.Endpoint && a.Bucket == b.Bucket && a.Region == b.Region
}

// SetAuditSettings 保存审计设置。关闭录制、修改外送目标、缩短保留时间要先做高权限确认；修改外送目标是安全事件：先把这条记录发往原来的目标，再生效。
func (s *settingsService) SetAuditSettings(ctx context.Context, req *connect.Request[nagiv1.AuditSettings]) (*connect.Response[nagiv1.AuditSettings], error) {
	m := req.Msg
	was := s.RecordingEnabled(ctx)
	if was && !m.GetRecording() && !auth.CurrentUser(ctx).Elevated() {
		return nil, elevationError()
	}
	if d := m.GetRetentionDays(); d < 7 || d > 3650 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("普通记录的保留时间须在 7 到 3650 天之间"))
	}
	if d := m.GetLongRetentionDays(); d < m.GetRetentionDays() || d > 3650 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("安全记录的保留时间不能短于普通记录，且不超过 3650 天"))
	}
	// 缩短保留时间会在下一次清理时删掉更早的记录（见 docs/audit.md#存储与保留）：和关闭录制一样要先确认身份，记安全事件并告警。
	oldRet := s.Retention(ctx)
	oldNormal, oldLong := int32(oldRet.Normal/(24*time.Hour)), int32(oldRet.Long/(24*time.Hour))
	shortened := m.GetRetentionDays() < oldNormal || m.GetLongRetentionDays() < oldLong
	if shortened && !auth.CurrentUser(ctx).Elevated() {
		return nil, elevationError()
	}
	oldSinks := s.Sinks(ctx)
	old := map[string]audit.SinkConfig{}
	for _, sk := range oldSinks {
		old[sk.ID] = sk
	}
	var sinks []audit.SinkConfig
	for _, p := range m.GetSinks() {
		sk, err := sinkFrom(p, old)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		sinks = append(sinks, sk)
	}
	sinksChanged := len(sinks)+len(oldSinks) > 0 && !reflect.DeepEqual(sinks, oldSinks)
	// 外送目标决定审计记录发到哪里：偷到会话、做不了高权限确认的人不能改（见 docs/audit.md#外送）。
	if sinksChanged && !auth.CurrentUser(ctx).Elevated() {
		return nil, elevationError()
	}
	if sinksChanged {
		// 先记一条安全事件、发往原来的目标，再换成新的：攻击者拿到管理员后改外送，改之前的记录已经送出去了。
		var names []string
		for _, sk := range sinks {
			names = append(names, sk.Type+":"+sk.Name)
		}
		b, _ := json.Marshal(map[string]any{"sinks": names})
		id := s.Record(ctx, audit.Event{Category: audit.Security, Action: "audit_sinks_changed", Params: string(b)})
		if s.Shipper != nil && len(oldSinks) > 0 {
			s.Shipper.FlushHealthy(id, 15*time.Second)
		}
		if err := s.saveSinks(ctx, sinks); err != nil {
			return nil, err
		}
		if s.Shipper != nil {
			s.Shipper.Configure(ctx, sinks)
		}
		s.securityAlert(ctx, "audit-sinks", "日志外送已修改")
	}
	// 登录安全的三个开关改在 设置 → 告警 的事件通知里（关掉要高权限确认并先通知，见 SetAlertNotification），这里不再保存。
	for k, v := range map[string]string{
		settingRetention: strconv.Itoa(int(m.GetRetentionDays())), settingLongRetention: strconv.Itoa(int(m.GetLongRetentionDays())),
	} {
		if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: k, Value: v}); err != nil {
			return nil, err
		}
	}
	if shortened {
		b, _ := json.Marshal(map[string]any{"retention_days": [2]int32{oldNormal, m.GetRetentionDays()}, "long_retention_days": [2]int32{oldLong, m.GetLongRetentionDays()}})
		s.Record(ctx, audit.Event{Category: audit.Security, Action: "audit_retention_shortened", Params: string(b)})
		s.securityAlert(ctx, "audit-retention", "审计记录的保留时间已缩短")
	}
	if was != m.GetRecording() {
		v, title := "1", "终端录制已开启"
		if !m.GetRecording() {
			v, title = "0", "终端录制已关闭"
		}
		if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: SettingRecording, Value: v}); err != nil {
			return nil, err
		}
		s.securityAlert(ctx, "recording", title)
	}
	return connect.NewResponse(s.auditSettings(ctx)), nil
}

func (s *settingsService) TestAuditSink(ctx context.Context, req *connect.Request[nagiv1.AuditSink]) (*connect.Response[nagiv1.TestAuditSinkResponse], error) {
	old := map[string]audit.SinkConfig{}
	for _, sk := range s.Sinks(ctx) {
		old[sk.ID] = sk
	}
	sk, err := sinkFrom(req.Msg, old)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	sh := s.Shipper
	if sh == nil {
		sh = &audit.Shipper{}
	}
	if err := sh.TestSink(ctx, sk); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, i18n.Detail(i18n.New("无法发送到这个目标，请检查地址和凭据"), err))
	}
	return connect.NewResponse(&nagiv1.TestAuditSinkResponse{}), nil
}

func (s *settingsService) VerifyAudit(ctx context.Context, _ *connect.Request[nagiv1.VerifyAuditRequest]) (*connect.Response[nagiv1.AuditChainStatus], error) {
	if _, err := s.view().Verify(ctx); err != nil {
		return nil, err
	}
	return connect.NewResponse(s.chainStatus(ctx)), nil
}
