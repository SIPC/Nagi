package auth

import (
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/auth/attestation"
)

// revocationList 是一份吊销列表，updated 用来决定新旧。
func revocationList(t *testing.T, updated time.Time, serials ...string) *attestation.Revocations {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"entries": {`)
	for i, s := range serials {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + s + `": {"status": "REVOKED", "reason": "KEY_COMPROMISE"}`)
	}
	b.WriteString(`}}`)
	r, err := attestation.ParseRevocations([]byte(b.String()), updated)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 添加时证明链里有被吊销的证书就拒绝；添加以后被吊销的标明、记审计、告警一次，不退出；从列表里拿掉以后清掉标明。
func TestAppRevocation(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var alerts []string
	h.s.Alert = func(_ context.Context, key, title, _ string) {
		mu.Lock()
		defer mu.Unlock()
		alerts = append(alerts, key+" "+title)
	}
	// 开发模式：接受自己的根证书，只看吊销列表。fakeChain 的根证书序列号是 1。
	h.s.SetDevAppKeys(true)
	keys := newAppKeys()
	chains := func(code string) [][][]byte {
		ch := sha256.Sum256([]byte(code))
		null := asn1.RawValue{Tag: asn1.TagNull}
		sw := []asn1.RawValue{tagged(t, 709, appID(t, AppPackage))}
		return [][][]byte{
			fakeChain(t, keys.device, attestation.Software, ch[:], append(sw, tagged(t, 509, null)), nil),
			fakeChain(t, keys.confirm, attestation.Software, ch[:], append(sw, tagged(t, 504, 3)), nil),
			fakeChain(t, keys.push, attestation.Software, ch[:], append(sw, tagged(t, 503, null)), nil),
		}
	}
	base := attestation.Embedded().Updated
	if !h.s.SetRevocations(revocationList(t, base.Add(time.Hour), "1")) {
		t.Fatal("新的列表没有换上")
	}
	if h.s.SetRevocations(revocationList(t, base)) {
		t.Error("旧的列表不该换上")
	}

	code, _, err := h.s.CreateAppPairing(ctx, h.webUser("op"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, chains(code)), AppDeviceInfo{Model: "Redmi"}); err == nil || !strings.Contains(err.Error(), "吊销") {
		t.Fatalf("证明证书被吊销时应该拒绝添加：%v", err)
	}
	if n := h.countAudit("app_attestation_failed"); n != 1 {
		t.Errorf("app_attestation_failed 记了 %d 条", n)
	}

	h.s.SetRevocations(revocationList(t, base.Add(2*time.Hour)))
	res, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, chains(code)), AppDeviceInfo{Model: "Redmi"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := h.q.AppDeviceByID(ctx, res.DeviceID)
	// 开发模式下链追溯到自己的根证书，不算检查过吊销，但照样记下序列号、复查。
	if d.AttestationSerials != "1" || d.RevocationChecked || d.RevokedReason != "" {
		t.Fatalf("设备记录：序列号 %q，检查过 %v，吊销 %q", d.AttestationSerials, d.RevocationChecked, d.RevokedReason)
	}

	// 添加以后被吊销（列表里的写法可以带前导 0、大写）：标明、审计、告警一次，不退出。
	mu.Lock()
	alerts = nil
	mu.Unlock()
	h.s.SetRevocations(revocationList(t, base.Add(3*time.Hour), "01"))
	h.s.RecheckAppDevices(ctx)
	h.s.RecheckAppDevices(ctx)
	d, err = h.q.AppDeviceByID(ctx, res.DeviceID)
	if err != nil {
		t.Fatalf("被吊销后不该退出：%v", err)
	}
	if d.RevokedReason != "KEY_COMPROMISE" {
		t.Errorf("吊销原因 %q", d.RevokedReason)
	}
	if n := h.countAudit("app_attestation_revoked"); n != 1 {
		t.Errorf("app_attestation_revoked 记了 %d 条，想要 1 条", n)
	}
	mu.Lock()
	if len(alerts) != 1 || !strings.HasPrefix(alerts[0], "app-revoked:"+res.DeviceID) {
		t.Errorf("告警 %v", alerts)
	}
	mu.Unlock()

	// 从列表里拿掉以后清掉标明。
	h.s.SetRevocations(revocationList(t, base.Add(4*time.Hour)))
	h.s.RecheckAppDevices(ctx)
	if d, _ = h.q.AppDeviceByID(ctx, res.DeviceID); d.RevokedReason != "" {
		t.Errorf("不在列表里了还标着吊销：%q", d.RevokedReason)
	}

	// 没有证明（开发模式的软件密钥）：不算检查过吊销，不记序列号。
	id := h.pairSoftware("op", newAppKeys())
	if d, _ = h.q.AppDeviceByID(ctx, id); d.RevocationChecked || d.AttestationSerials != "" {
		t.Errorf("没有证明的手机：检查过 %v，序列号 %q", d.RevocationChecked, d.AttestationSerials)
	}
}

// RevocationLoop 取到新列表后存进数据目录，下次启动先读它。
func TestRevocationLoop(t *testing.T) {
	h := newSessionHarness(t)
	dir := t.TempDir()
	// 比随面板发布的新、又不晚于现在（读回时日期晚于现在的不用）。
	updated := time.Now().Add(-time.Second).UTC().Truncate(time.Second)
	body := []byte(`{"entries": {"abc": {"status": "REVOKED", "reason": "KEY_COMPROMISE"}}}`)
	fetched := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.RevocationLoop(ctx, dir, discardLogger(), func(context.Context) ([]byte, time.Time, error) {
			select {
			case fetched <- struct{}{}:
			default:
			}
			return body, updated, nil
		})
	}()
	<-fetched
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, ok := h.s.Revocations().Check([]string{"abc"}); ok {
			if _, err := os.Stat(filepath.Join(dir, "attestation", "status.updated")); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("取到的列表没有换上或没有保存")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	// 重新启动：先读保存的列表；取不到新的时一直用它。
	s2 := newSessionHarness(t).s
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	tried := make(chan struct{}, 1)
	go func() {
		defer close(done2)
		s2.RevocationLoop(ctx2, dir, discardLogger(), func(context.Context) ([]byte, time.Time, error) {
			select {
			case tried <- struct{}{}:
			default:
			}
			return nil, time.Time{}, context.DeadlineExceeded
		})
	}()
	<-tried
	if _, _, ok := s2.Revocations().Check([]string{"abc"}); !ok {
		t.Error("没有读到保存的列表")
	}
	if !s2.Revocations().Updated.Equal(updated) {
		t.Errorf("保存的列表日期 %v，想要 %v", s2.Revocations().Updated, updated)
	}
	cancel2()
	<-done2
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// 数据目录里的日期写成将来时不用这份（按现在算的话，之后的列表都换不上它），RevocationLoop 启动时删掉它。
func TestLoadRevocationsFutureDate(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "attestation")
	if err := saveRevocations(dir, []byte(`{"entries": {}}`), time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRevocations(dir); !errors.Is(err, errFutureRevocations) {
		t.Errorf("日期晚于现在的列表应该不用：%v", err)
	}
	h := newSessionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	fetched := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		h.s.RevocationLoop(ctx, dataDir, discardLogger(), func(context.Context) ([]byte, time.Time, error) {
			once.Do(func() { close(fetched) })
			return nil, time.Time{}, context.DeadlineExceeded
		})
	}()
	<-fetched
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(dir, "status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("日期晚于现在的列表应该删掉：%v", err)
	}
	if h.s.Revocations() != attestation.Embedded() {
		t.Error("应该用随面板发布的列表")
	}
}

// requestRecheck 让正在运行的 RevocationLoop 复查：换了列表以后，不等下一次取列表就标出被吊销的手机。
func TestRevocationRecheckSignal(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	h.s.SetDevAppKeys(true)
	keys := newAppKeys()
	code, _, err := h.s.CreateAppPairing(ctx, h.webUser("op"))
	if err != nil {
		t.Fatal(err)
	}
	ch := sha256.Sum256([]byte(code))
	null := asn1.RawValue{Tag: asn1.TagNull}
	sw := []asn1.RawValue{tagged(t, 709, appID(t, AppPackage))}
	chains := [][][]byte{
		fakeChain(t, keys.device, attestation.Software, ch[:], append(sw, tagged(t, 509, null)), nil),
		fakeChain(t, keys.confirm, attestation.Software, ch[:], append(sw, tagged(t, 504, 3)), nil),
		fakeChain(t, keys.push, attestation.Software, ch[:], append(sw, tagged(t, 503, null)), nil),
	}
	res, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, chains), AppDeviceInfo{Model: "Redmi"})
	if err != nil {
		t.Fatal(err)
	}
	// 首轮复查结束的标志：先把这台手机标成吊销（当前列表里没有它），首轮复查会清掉。
	if err := h.q.SetAppDeviceRevoked(ctx, res.DeviceID, "STALE"); err != nil {
		t.Fatal(err)
	}
	reason := func() string { d, _ := h.q.AppDeviceByID(ctx, res.DeviceID); return d.RevokedReason }
	waitFor := func(want, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for reason() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s：吊销原因 %q，想要 %q", what, reason(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.RevocationLoop(loopCtx, t.TempDir(), discardLogger(), func(context.Context) ([]byte, time.Time, error) {
			return nil, time.Time{}, context.DeadlineExceeded
		})
	}()
	defer func() { cancel(); <-done }()
	waitFor("", "首轮复查")
	// 首轮复查之后换列表：取列表要 6 小时后，不叫复查就不会标出。
	h.s.SetRevocations(revocationList(t, attestation.Embedded().Updated.Add(time.Hour), "1"))
	time.Sleep(200 * time.Millisecond)
	if r := reason(); r != "" {
		t.Fatalf("没叫复查就标出了（%q），测不到复查通知", r)
	}
	h.s.requestRecheck()
	waitFor("KEY_COMPROMISE", "叫复查以后")
}
