package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/server/store"
)

const approvalIP = "198.51.100.7"

func signApproval(k *ecdsa.PrivateKey, device, id, kind, code string) []byte {
	sum := sha256.Sum256(ApprovalSigned(appHost, device, id, kind, code))
	sig, err := ecdsa.SignASN1(rand.Reader, k, sum[:])
	if err != nil {
		panic(err)
	}
	return sig
}

// appUserOf 用设备密钥换取令牌，返回 App 令牌对应的成员。
func (h *sessionHarness) appUserOf(device string, keys appKeys) User {
	h.t.Helper()
	res, err := h.appToken(device, keys.device, AppKeyDevice, approvalIP)
	if err != nil {
		h.t.Fatal(err)
	}
	u, err := h.s.AppTokenUser(context.Background(), res.Token, approvalIP)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

// 网页的高权限确认发到手机：数字和签名都对才算；只认发起的会话；给自己留登录凭据的操作不认。
func TestPhoneApprovalElevate(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	web := h.login("op", RequestInfo{IP: approvalIP, UserAgent: chromeUA})
	if _, err := h.s.BeginElevateApproval(ctx, approvalIP, web); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("没有手机时应该拒绝：%v", err)
	}
	keys := newAppKeys()
	device := h.pairSoftware("op", keys)
	phone := h.appUserOf(device, keys)

	a, err := h.s.BeginElevateApproval(ctx, approvalIP, web)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, err := h.s.ElevateApprovalStatus(ctx, approvalIP, web, a.ID); err != nil || st != ApprovalPending {
		t.Fatalf("还没确认：%q %v", st, err)
	}
	list := h.s.AppApprovals(phone)
	if len(list) != 1 || list[0].ID != a.ID || list[0].Kind != ApprovalElevate || list[0].IP != approvalIP {
		t.Fatalf("手机看到的请求：%+v", list)
	}
	// 别的成员的手机看不到，也确认不了。
	otherKeys := newAppKeys()
	other := h.appUserOf(h.pairSoftware("admin", otherKeys), otherKeys)
	if len(h.s.AppApprovals(other)) != 0 {
		t.Error("别的成员不该看到这个请求")
	}
	req := AppApprovalRequest{ID: a.ID, Code: a.Code, Host: appHost, Signature: signApproval(otherKeys.confirm, other.DeviceID, a.ID, ApprovalElevate, a.Code)}
	if err := h.s.AppApprove(ctx, approvalIP, other, req, appHosts); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("别的成员确认：%v", err)
	}
	// 数字不对、签名不对（用设备密钥签）、主机不对都拒绝。
	wrong := "00"
	if a.Code == wrong {
		wrong = "01"
	}
	req = AppApprovalRequest{ID: a.ID, Code: wrong, Host: appHost, Signature: signApproval(keys.confirm, device, a.ID, ApprovalElevate, wrong)}
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("数字不对：%v", err)
	}
	req = AppApprovalRequest{ID: a.ID, Code: a.Code, Host: appHost, Signature: signApproval(keys.device, device, a.ID, ApprovalElevate, a.Code)}
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("用设备密钥签：%v", err)
	}
	req.Signature = signApproval(keys.confirm, device, a.ID, ApprovalElevate, a.Code)
	req.Host = "evil.example:9443"
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("主机不对：%v", err)
	}
	req.Host = appHost
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); err != nil {
		t.Fatal(err)
	}
	// 别的会话取不到；换了 IP 不算。
	web2 := h.login("op", RequestInfo{IP: approvalIP, UserAgent: chromeUA})
	if _, _, err := h.s.ElevateApprovalStatus(ctx, approvalIP, web2, a.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("别的会话：%v", err)
	}
	st, until, err := h.s.ElevateApprovalStatus(ctx, approvalIP, web, a.ID)
	if err != nil || st != ApprovalApproved || until.IsZero() {
		t.Fatalf("确认以后：%q %v %v", st, until, err)
	}
	u, err := h.s.SessionUser(ctx, web)
	if err != nil || !u.Elevated() || u.ElevatedBy != ElevatedByApp {
		t.Fatalf("会话：elevated %v by %q，%v", u.Elevated(), u.ElevatedBy, err)
	}
	for _, p := range PasswordElevationProcedures() {
		if u.ElevatedFor(p) {
			t.Errorf("%s 不该认手机的确认", p)
		}
	}
	if !u.ElevatedFor("/nagi.v1.NodeService/ReadNodeFile") {
		t.Error("别的接口应该认手机的确认")
	}
	// 结果只给一次。
	if _, _, err := h.s.ElevateApprovalStatus(ctx, approvalIP, web, a.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("第二次取：%v", err)
	}
	if h.countAudit("elevated_by_phone") != 1 || h.countAudit("app_approved") != 1 {
		t.Errorf("审计：elevated_by_phone %d，app_approved %d", h.countAudit("elevated_by_phone"), h.countAudit("app_approved"))
	}

	// 数字输错 3 次作废；拒绝；换了 IP 取不到。
	a, _ = h.s.BeginElevateApproval(ctx, approvalIP, web)
	for range approvalCodeTries {
		_ = h.s.AppApprove(ctx, approvalIP, phone, AppApprovalRequest{ID: a.ID, Code: "xx", Host: appHost}, appHosts)
	}
	if st, _, _ := h.s.ElevateApprovalStatus(ctx, approvalIP, web, a.ID); st != ApprovalDenied {
		t.Errorf("输错 3 次：%q", st)
	}
	a, _ = h.s.BeginElevateApproval(ctx, approvalIP, web)
	if err := h.s.AppApprove(ctx, approvalIP, phone, AppApprovalRequest{ID: a.ID, Deny: true}, appHosts); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := h.s.ElevateApprovalStatus(ctx, approvalIP, web, a.ID); st != ApprovalDenied {
		t.Errorf("拒绝：%q", st)
	}
	a, _ = h.s.BeginElevateApproval(ctx, approvalIP, web)
	req = AppApprovalRequest{ID: a.ID, Code: a.Code, Host: appHost, Signature: signApproval(keys.confirm, device, a.ID, ApprovalElevate, a.Code)}
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := h.s.ElevateApprovalStatus(ctx, "198.51.100.99", web, a.ID); st == ApprovalApproved {
		t.Error("换了 IP 不该算确认过")
	}
}

// 登录的第二步改用手机确认：密码要对、账号要开了两步验证；确认后凭发起时的凭据拿到会话，只给一次；拒绝时告警。
func TestPhoneApprovalLogin(t *testing.T) {
	h := newSessionHarness(t)
	ctx := WithMeta(context.Background(), Meta{IP: approvalIP, UserAgent: chromeUA})
	keys := newAppKeys()
	device := h.pairSoftware("op", keys)
	phone := h.appUserOf(device, keys)
	if _, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "wrong"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("密码不对：%v", err)
	}
	if _, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("没开两步验证：%v", err)
	}
	op, _ := h.q.GetUserByUsername(ctx, "op")
	if err := h.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString([]byte("JBSWY3DPEHPK3PXP")), ID: op.ID}); err != nil {
		t.Fatal(err)
	}
	a, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse")
	if err != nil || a.Ticket == "" {
		t.Fatalf("发起：%+v %v", a, err)
	}
	if st, c, err := h.s.CompleteLoginApproval(ctx, approvalIP, a.Ticket); err != nil || st != ApprovalPending || c != nil {
		t.Fatalf("还没确认：%q %v %v", st, c, err)
	}
	if _, _, err := h.s.CompleteLoginApproval(ctx, approvalIP, "guess"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("猜凭据：%v", err)
	}
	if _, _, err := h.s.CompleteLoginApproval(ctx, "198.51.100.99", a.Ticket); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("换了 IP：%v", err)
	}
	req := AppApprovalRequest{ID: a.ID, Code: a.Code, Host: appHost, Signature: signApproval(keys.confirm, device, a.ID, ApprovalLogin, a.Code)}
	// 高权限确认的签名不能拿来确认登录（种类签在里面）。
	bad := req
	bad.Signature = signApproval(keys.confirm, device, a.ID, ApprovalElevate, a.Code)
	if err := h.s.AppApprove(ctx, approvalIP, phone, bad, appHosts); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("种类不对的签名：%v", err)
	}
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); err != nil {
		t.Fatal(err)
	}
	st, cookie, err := h.s.CompleteLoginApproval(ctx, approvalIP, a.Ticket)
	if err != nil || st != ApprovalApproved || cookie == nil {
		t.Fatalf("确认以后：%q %v %v", st, cookie, err)
	}
	if u, err := h.s.SessionUser(ctx, cookie.Value); err != nil || u.Name != "op" {
		t.Fatalf("会话：%+v %v", u, err)
	}
	if _, _, err := h.s.CompleteLoginApproval(ctx, approvalIP, a.Ticket); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("会话只给一次：%v", err)
	}

	// 拒绝：登录页得到 denied，发告警。
	a, _ = h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse")
	if err := h.s.AppApprove(ctx, approvalIP, phone, AppApprovalRequest{ID: a.ID, Deny: true}, appHosts); err != nil {
		t.Fatal(err)
	}
	if st, c, _ := h.s.CompleteLoginApproval(ctx, approvalIP, a.Ticket); st != ApprovalDenied || c != nil {
		t.Errorf("拒绝：%q %v", st, c)
	}
	if !h.alerted("被拒绝") {
		t.Error("拒绝登录请求应该告警")
	}

	// 发起太频繁时拒绝；新的请求让旧的作废。
	var last Approval
	for i := 0; ; i++ {
		x, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse")
		if err != nil {
			if connect.CodeOf(err) != connect.CodeResourceExhausted || i == 0 {
				t.Fatalf("第 %d 次发起：%v", i+1, err)
			}
			break
		}
		if last.Ticket != "" {
			if _, _, err := h.s.CompleteLoginApproval(ctx, approvalIP, last.Ticket); connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("旧的请求应该作废：%v", err)
			}
		}
		last = x
		if i > approvalPerWindow {
			t.Fatal("没有限制发起的频率")
		}
	}
}

// App 自己的高权限确认：确认密钥签挑战；只对确认时的 IP 有效；退出手机后作废。
func TestAppElevate(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	device := h.pairSoftware("op", keys)
	phone := h.appUserOf(device, keys)
	if phone.Elevated() {
		t.Fatal("还没确认")
	}
	sign := func(k *ecdsa.PrivateKey, ch string) []byte {
		sum := sha256.Sum256(AppElevateSigned(appHost, device, ch))
		sig, _ := ecdsa.SignASN1(rand.Reader, k, sum[:])
		return sig
	}
	ch, _ := h.s.AppChallenge(ctx, approvalIP, device)
	if _, err := h.s.AppElevate(ctx, approvalIP, phone, ch, appHost, sign(keys.device, ch), appHosts); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("用设备密钥签：%v", err)
	}
	if _, err := h.s.AppElevate(ctx, approvalIP, phone, ch, appHost, sign(keys.confirm, ch), appHosts); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("挑战只能用一次：%v", err)
	}
	ch, _ = h.s.AppChallenge(ctx, approvalIP, device)
	// 换取令牌的签名不能拿来做高权限确认（用途签在里面）。
	if _, err := h.s.AppElevate(ctx, approvalIP, phone, ch, appHost, signAppToken(keys.confirm, appHost, device, ch), appHosts); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("换取令牌的签名：%v", err)
	}
	ch, _ = h.s.AppChallenge(ctx, approvalIP, device)
	if _, err := h.s.AppElevate(ctx, approvalIP, phone, ch, appHost, sign(keys.confirm, ch), appHosts); err != nil {
		t.Fatal(err)
	}
	phone = h.appUserOf(device, keys)
	if !phone.Elevated() || phone.ElevatedBy != ElevatedByApp {
		t.Fatalf("确认以后：elevated %v by %q", phone.Elevated(), phone.ElevatedBy)
	}
	for _, p := range PasswordElevationProcedures() {
		if phone.ElevatedFor(p) {
			t.Errorf("%s 不该认手机的确认", p)
		}
	}
	if until := h.s.appElevatedUntil(device, "198.51.100.99"); until != 0 {
		t.Error("换了 IP 不该算确认过")
	}
	if ok, err := h.s.RemoveAppDevice(ctx, phone.ID, "op", device, "test"); err != nil || !ok {
		t.Fatal(err)
	}
	if until := h.s.appElevatedUntil(device, approvalIP); until != 0 {
		t.Error("退出手机后确认应该作废")
	}
}

// 项目分组和节点标签决定成员的访问范围：令牌要勾「管理面板」才能改（2026-11-19-87c02b5#15）。
func TestTokenScopeAdminNeedsPanel(t *testing.T) {
	for _, p := range []string{"/nagi.v1.ProjectService/SetProjectGroup", "/nagi.v1.NodeService/SetNodeTags"} {
		if TokenAllowed(User{TokenID: 1, AllowWrite: true}, p) {
			t.Errorf("%s：只勾「可修改」的令牌不该能调用", p)
		}
		if !TokenAllowed(User{TokenID: 1, AllowWrite: true, AllowPanel: true}, p) {
			t.Errorf("%s：勾了「管理面板」和「可修改」的令牌应该能调用", p)
		}
	}
}

// 别处发起的登录请求顶不掉成员自己这边的，也占不掉他的额度（2026-11-19-87c02b5#12）。
func TestPhoneApprovalSources(t *testing.T) {
	h := newSessionHarness(t)
	ctx := WithMeta(context.Background(), Meta{IP: approvalIP, UserAgent: chromeUA})
	keys := newAppKeys()
	device := h.pairSoftware("op", keys)
	phone := h.appUserOf(device, keys)
	op, _ := h.q.GetUserByUsername(ctx, "op")
	if err := h.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString([]byte("JBSWY3DPEHPK3PXP")), ID: op.ID}); err != nil {
		t.Fatal(err)
	}
	mine, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	const other = "203.0.113.50"
	for i := 0; i < approvalPerWindow+2; i++ {
		_, _ = h.s.BeginLoginApproval(ctx, other, "op", "correct horse")
	}
	if st, _, err := h.s.CompleteLoginApproval(ctx, approvalIP, mine.Ticket); err != nil || st != ApprovalPending {
		t.Fatalf("自己的请求被别处的顶掉了：%q %v", st, err)
	}
	if n := len(h.s.AppApprovals(phone)); n != 2 {
		t.Errorf("手机上应该看到两个来源各一个请求，实际 %d 个", n)
	}
	// 别处的额度用完了，自己这边照常能发起。
	if _, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse"); err != nil {
		t.Errorf("自己这边的额度不该被占掉：%v", err)
	}
}

// 别的来源再多也顶不掉已有的请求：满了就拒绝新的（2026-11-19-402545d#3）。
func TestPhoneApprovalFullKeepsExisting(t *testing.T) {
	h := newSessionHarness(t)
	ctx := WithMeta(context.Background(), Meta{IP: approvalIP, UserAgent: chromeUA})
	keys := newAppKeys()
	device := h.pairSoftware("op", keys)
	phone := h.appUserOf(device, keys)
	op, _ := h.q.GetUserByUsername(ctx, "op")
	if err := h.q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: base64.StdEncoding.EncodeToString([]byte("JBSWY3DPEHPK3PXP")), ID: op.ID}); err != nil {
		t.Fatal(err)
	}
	mine, err := h.s.BeginLoginApproval(ctx, approvalIP, "op", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	// 手机上已经确认、网页还没来取。
	req := AppApprovalRequest{ID: mine.ID, Code: mine.Code, Host: appHost, Signature: signApproval(keys.confirm, device, mine.ID, ApprovalLogin, mine.Code)}
	if err := h.s.AppApprove(ctx, approvalIP, phone, req, appHosts); err != nil {
		t.Fatal(err)
	}
	rejected := 0
	for i := 0; i < approvalMaxPending+3; i++ {
		if _, err := h.s.BeginLoginApproval(ctx, fmt.Sprintf("203.0.%d.9", 100+i), "op", "correct horse"); connect.CodeOf(err) == connect.CodeResourceExhausted {
			rejected++
		}
	}
	if rejected == 0 {
		t.Error("来源太多时应该拒绝新的请求")
	}
	if st, cookie, err := h.s.CompleteLoginApproval(ctx, approvalIP, mine.Ticket); err != nil || st != ApprovalApproved || cookie == nil {
		t.Fatalf("已经确认的请求被别处的顶掉了：%q %v", st, err)
	}
}
