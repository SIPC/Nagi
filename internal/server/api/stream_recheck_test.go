package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"net/http/cookiejar"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

const testAppHost = "panel.test:9443"

// pairTestApp 在开发模式下用软件密钥给成员添加一台手机，换取 ip 上的 App 令牌，返回设备 ID 和令牌。
func pairTestApp(t *testing.T, as *auth.Service, userID int64, ip string) (string, string) {
	t.Helper()
	ctx := auth.WithMeta(context.Background(), auth.Meta{IP: ip, UserAgent: chromeUA})
	c, err := as.NewSession(ctx, userID, ip)
	if err != nil {
		t.Fatal(err)
	}
	web, err := as.SessionUser(ctx, c.Value)
	if err != nil {
		t.Fatal(err)
	}
	as.SetDevAppKeys(true)
	defer as.SetDevAppKeys(false)
	code, _, err := as.CreateAppPairing(ctx, web)
	if err != nil {
		t.Fatal(err)
	}
	var keys [3]auth.AppKeyProof
	var device *ecdsa.PrivateKey
	for i := range keys {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			device = k
		}
		keys[i].PublicKey, _ = x509.MarshalPKIXPublicKey(&k.PublicKey)
	}
	res, err := as.PairApp(ctx, ip, code, keys, auth.AppDeviceInfo{Model: "Pixel 9", AndroidVersion: "16", SDK: 36})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := as.AppChallenge(ctx, ip, res.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(auth.AppTokenSigned(testAppHost, res.DeviceID, ch))
	sig, err := ecdsa.SignASN1(rand.Reader, device, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	tok, err := as.AppToken(ctx, ip, auth.AppTokenRequest{DeviceID: res.DeviceID, Challenge: ch, Key: auth.AppKeyDevice, Host: testAppHost,
		Signature: sig}, []string{testAppHost}, false)
	if err != nil {
		t.Fatal(err)
	}
	return res.DeviceID, tok.Token
}

// v0.35.0#4：App 打开的终端按设备记录重查：令牌过期不影响（不再 30 秒后必断），退出手机、降权、移出范围后断开；
// 日志流按令牌重查，令牌要重新换取时把带 Nagi-App-Retoken 的错误交给流。
func TestWatchSessionApp(t *testing.T) {
	old := sessionRecheck
	sessionRecheck = 5 * time.Millisecond
	t.Cleanup(func() { sessionRecheck = old })

	h := newAuditHarness(t)
	a := h.a
	bg := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(bg, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	op, err := a.Auth.CreateUser(bg, "op", "op-password-1", auth.OperatorRoleID)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x'')`)
	const ip = "198.51.100.20"
	at := func(ip string) context.Context { return auth.WithMeta(bg, auth.Meta{IP: ip}) }

	type setup struct {
		u     auth.User
		token string
	}
	reset := func() setup {
		t.Helper()
		exec(`DELETE FROM app_devices`)
		exec(`DELETE FROM user_projects`)
		exec(`UPDATE users SET role = 'member', role_id = 2, all_projects = 0 WHERE id = ?`, op)
		exec(`INSERT INTO user_projects (user_id, project_id) VALUES (?, 'p1')`, op)
		id, tok := pairTestApp(t, a.Auth, op, ip)
		u, err := a.Auth.AppTokenUser(bg, tok, ip)
		if err != nil || u.DeviceID != id {
			t.Fatal(u, err)
		}
		return setup{u, tok}
	}
	// watch 在 limit 之内等重查断开，没断开时返回 nil。
	watch := func(ctx context.Context, s setup, mode appRecheck, limit time.Duration) <-chan error {
		ctx, cancel := context.WithTimeout(ctx, limit)
		hdr := make(map[string][]string)
		if s.token != "" {
			hdr["Authorization"] = []string{"Bearer " + s.token}
		}
		done := make(chan error, 1)
		go func() {
			defer cancel()
			a.watchSession(ctx, s.u, hdr, mode, func(cur auth.User) error {
				return a.containerAccess(ctx, cur, "p1", auth.PermTerminal)
			}, func(err error) { done <- err })
			close(done)
		}()
		return done
	}

	// 终端不看令牌：不带令牌（相当于令牌早已过期）也一直开着。
	s := reset()
	if err := <-watch(at(ip), setup{u: s.u}, appByDevice, 100*time.Millisecond); err != nil {
		t.Fatalf("App 打开的终端不该断开：%v", err)
	}
	// 日志流：令牌有效时一直开着。
	if err := <-watch(at(ip), s, appByToken, 100*time.Millisecond); err != nil {
		t.Fatalf("令牌有效时日志流不该断开：%v", err)
	}

	cases := []struct {
		name   string
		mode   appRecheck
		change func(s setup)
		want   error
	}{
		{"终端：退出手机", appByDevice, func(s setup) {
			if ok, err := a.Auth.RemoveAppDevice(bg, op, "op", s.u.DeviceID, "test"); !ok || err != nil {
				t.Fatal(ok, err)
			}
		}, errSessionExpired},
		{"终端：改密码（退出全部手机）", appByDevice, func(setup) { exec(`DELETE FROM app_devices WHERE user_id = ?`, op) }, errSessionExpired},
		{"终端：降为只读", appByDevice, func(setup) { exec(`UPDATE users SET role_id = 3 WHERE id = ?`, op) }, errRoleChanged},
		{"终端：移出项目范围", appByDevice, func(setup) { exec(`DELETE FROM user_projects`) }, errRoleChanged},
		// v0.35.0-fix#2：App 退到后台、锁屏以后不再换取令牌，超过解锁时长断开；签名失败次数到上限也断开。
		{"终端：超过解锁时长", appByDevice, func(s setup) {
			exec(`UPDATE app_devices SET last_unlock_at = ? WHERE id = ?`, time.Now().Add(-16*time.Minute).Unix(), s.u.DeviceID)
		}, errAppLocked},
		{"终端：签名失败次数到上限", appByDevice, func(s setup) { exec(`UPDATE app_devices SET sig_failures = 5 WHERE id = ?`, s.u.DeviceID) }, errAppLocked},
		{"日志：退出手机", appByToken, func(s setup) { exec(`DELETE FROM app_devices WHERE id = ?`, s.u.DeviceID) }, errSessionExpired},
		{"日志：降为只读", appByToken, func(setup) { exec(`UPDATE users SET role_id = 3 WHERE id = ?`, op) }, errRoleChanged},
		{"日志：移出项目范围", appByToken, func(setup) { exec(`DELETE FROM user_projects`) }, errRoleChanged},
	}
	for _, c := range cases {
		s := reset()
		done := watch(at(ip), s, c.mode, 5*time.Second)
		select {
		case err := <-done:
			t.Fatalf("%s：还没改就断开了：%v", c.name, err)
		case <-time.After(30 * time.Millisecond):
		}
		c.change(s)
		if err := <-done; !errors.Is(err, c.want) {
			t.Errorf("%s：得到 %v，想要 %v", c.name, err, c.want)
		}
	}

	// 日志流的令牌要重新换取（这里是换了网段；过期同理，见 auth.TestStreamUser）：交出带 Nagi-App-Retoken 的错误。
	s = reset()
	err = <-watch(at("203.0.113.50"), s, appByToken, 5*time.Second)
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Meta().Get(auth.RetokenHeader) != "network" {
		t.Errorf("令牌要重新换取时应带 Nagi-App-Retoken：%v", err)
	}
	// 拿别的手机的令牌冒充：断开。
	other, _ := pairTestApp(t, a.Auth, op, ip)
	fake := s
	fake.u.DeviceID = other
	if err := <-watch(at(ip), fake, appByToken, 5*time.Second); !errors.Is(err, errSessionExpired) {
		t.Errorf("令牌和打开流的手机不一致：%v", err)
	}
	// 管理员不在允许的地址：终端也断开。
	s = reset()
	exec(`UPDATE users SET role = 'admin', role_id = 1 WHERE id = ?`, op)
	s.u.Role = auth.RoleAdmin
	if _, err := a.Auth.SetAdminNetworks(bg, []string{"192.0.2.0/24"}, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.Auth.SetAdminNetworks(bg, nil, "192.0.2.1") })
	if err := <-watch(at(ip), s, appByDevice, 5*time.Second); !errors.Is(err, errSessionExpired) {
		t.Errorf("管理员不在允许的地址时终端应断开：%v", err)
	}

	// v0.35.0-fix#2：按设备重查的终端从打开起最长用到会话设置的「最长登录时间」，和网页会话一致。
	s = reset()
	if _, err := a.recheckCaller(at(ip), s.u, nil, appByDevice, time.Now().Add(-auth.DefaultSessionPolicy.MaxAge+time.Hour)); err != nil {
		t.Errorf("还没到最长登录时间的终端不该断开：%v", err)
	}
	if _, err := a.recheckCaller(at(ip), s.u, nil, appByDevice, time.Now().Add(-auth.DefaultSessionPolicy.MaxAge-time.Minute)); !errors.Is(err, errSessionExpired) {
		t.Errorf("超过最长登录时间的终端应断开：%v", err)
	}
	exec(`INSERT INTO settings (key, value) VALUES (?, '2')`, auth.SettingSessionMaxAge)
	if _, err := a.recheckCaller(at(ip), s.u, nil, appByDevice, time.Now().Add(-3*time.Hour)); !errors.Is(err, errSessionExpired) {
		t.Errorf("最长登录时间改成 2 小时以后，打开 3 小时的终端应断开：%v", err)
	}
}

// fakeLogs 是假的节点日志流：每 2 毫秒一行，直到打开它的请求结束。
type fakeLogs struct{ ctx context.Context }

func (f fakeLogs) Recv() (*nagiv1.LogLine, error) {
	select {
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	case <-time.After(2 * time.Millisecond):
		return &nagiv1.LogLine{Stream: "stdout", Text: "line"}, nil
	}
}

// v0.35.0#4：用 App 令牌打开的日志流（follow）在退出手机、移出访问范围以后结束，不再照常推送；
// 网页会话打开的日志流照旧：移出范围后结束。
func TestStreamLogsRechecksCaller(t *testing.T) {
	old := sessionRecheck
	sessionRecheck = 10 * time.Millisecond
	t.Cleanup(func() { sessionRecheck = old })
	oldLogs := logStreamFor
	t.Cleanup(func() { logStreamFor = oldLogs })
	logStreamFor = func(_ *projectService, ctx context.Context, _ *nagiv1.StreamLogsApiRequest) (logStream, error) {
		return fakeLogs{ctx}, nil
	}

	h := newAuditHarness(t)
	bg := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.db.ExecContext(bg, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	op, err := h.a.Auth.CreateUser(bg, "op", "op-password-1", auth.OperatorRoleID)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO projects (id, name, network_name, revision, created_at, settings) VALUES ('p1', 'one', 'one', 1, 0, x'')`)
	exec(`UPDATE users SET all_projects = 0 WHERE id = ?`, op)
	inScope := func() {
		exec(`DELETE FROM user_projects`)
		exec(`INSERT INTO user_projects (user_id, project_id) VALUES (?, 'p1')`, op)
	}

	// open 打开 follow 的日志流，先收到 20 行（跨过几次重查），返回之后收到的行数通道（流结束时关闭）和流。
	type opened struct {
		lines  <-chan struct{}
		stream *connect.ServerStreamForClient[nagiv1.LogLine]
	}
	open := func(name string, c nagiv1connect.ProjectServiceClient, set func(*connect.Request[nagiv1.StreamLogsApiRequest])) opened {
		t.Helper()
		ctx, cancel := context.WithCancel(bg)
		t.Cleanup(cancel)
		req := connect.NewRequest(&nagiv1.StreamLogsApiRequest{ProjectId: "p1", Container: "web", Follow: true})
		if set != nil {
			set(req)
		}
		stream, err := c.StreamLogs(ctx, req)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		lines := make(chan struct{}, 1<<16)
		go func() {
			defer close(lines)
			for stream.Receive() {
				lines <- struct{}{}
			}
		}()
		for range 20 {
			select {
			case _, ok := <-lines:
				if !ok {
					t.Fatalf("%s：还没改就结束了：%v", name, stream.Err())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s：收不到日志", name)
			}
		}
		return opened{lines, stream}
	}
	// ended 等流结束，返回结束时的错误码。
	ended := func(name string, o opened) connect.Code {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-o.lines:
				if !ok {
					return connect.CodeOf(o.stream.Err())
				}
			case <-deadline:
				t.Fatalf("%s：改了以后流仍在推送", name)
			}
		}
	}

	// App：用令牌打开。测试服务器的来源地址是 127.0.0.1。
	devID, tok := pairTestApp(t, h.a.Auth, op, "127.0.0.1")
	app := nagiv1connect.NewProjectServiceClient(h.srv.Client(), h.srv.URL)
	bearer := func(r *connect.Request[nagiv1.StreamLogsApiRequest]) { r.Header().Set("Authorization", "Bearer "+tok) }

	inScope()
	o := open("App：移出范围", app, bearer)
	exec(`DELETE FROM user_projects`)
	if code := ended("App：移出范围", o); code != connect.CodePermissionDenied {
		t.Errorf("App 移出范围后应 permission_denied：%v", o.stream.Err())
	}

	inScope()
	o = open("App：退出手机", app, bearer)
	if ok, err := h.a.Auth.RemoveAppDevice(bg, op, "op", devID, "test"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// 令牌随手机一起作废：要重新换取（换取时手机已不存在），或者设备记录已经没了。
	if code := ended("App：退出手机", o); code != connect.CodeUnauthenticated && code != connect.CodePermissionDenied {
		t.Errorf("App 退出手机后应结束：%v", o.stream.Err())
	}

	// 网页会话：照旧。
	jar, _ := cookiejar.New(nil)
	hc := *h.srv.Client()
	hc.Jar = jar
	if _, err := nagiv1connect.NewAuthServiceClient(&hc, h.srv.URL).Login(bg, connect.NewRequest(&nagiv1.LoginRequest{Username: "op", Password: "op-password-1"})); err != nil {
		t.Fatal(err)
	}
	web := nagiv1connect.NewProjectServiceClient(&hc, h.srv.URL)
	inScope()
	o = open("网页：移出范围", web, nil)
	exec(`DELETE FROM user_projects`)
	if code := ended("网页：移出范围", o); code != connect.CodePermissionDenied {
		t.Errorf("网页移出范围后应 permission_denied：%v", o.stream.Err())
	}
	inScope()
	o = open("网页：退出登录", web, nil)
	exec(`DELETE FROM sessions WHERE user_id = ?`, op)
	if code := ended("网页：退出登录", o); code != connect.CodePermissionDenied {
		t.Errorf("网页退出登录后应 permission_denied：%v", o.stream.Err())
	}
}
