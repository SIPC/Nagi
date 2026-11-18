package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth/attestation"
)

// 证明证书的吊销列表（见 docs/android.md#吊销列表）。添加手机时按当前的列表检查；每天从 Google 取一次新的，
// 取到后存进数据目录，再把已经添加的手机复查一遍：被吊销的标明并告警，不退出。

const (
	// revocationEvery 是取到列表以后多久再取；取不到时 revocationRetry 后再试。
	revocationEvery = 24 * time.Hour
	revocationRetry = 6 * time.Hour
)

// Revocations 是当前用的吊销列表：随面板发布的和取到的里较新的一份。
func (s *Service) Revocations() *attestation.Revocations {
	if r := s.revocations.Load(); r != nil {
		return r
	}
	return attestation.Embedded()
}

// SetRevocations 换上一份吊销列表（不比当前的新时不换），换了返回 true。
func (s *Service) SetRevocations(r *attestation.Revocations) bool {
	for {
		raw := s.revocations.Load()
		cur := raw
		if cur == nil {
			cur = attestation.Embedded()
		}
		if !r.Updated.After(cur.Updated) {
			return false
		}
		if s.revocations.CompareAndSwap(raw, r) {
			return true
		}
	}
}

// RevocationLoop 先读数据目录里存的列表、复查一遍已添加的手机，之后每天从 Google 取一次。只在主面板上运行（要写数据库）。
// 添加手机时换了列表的，由 requestRecheck 叫它复查（在这里运行，不占请求，审计也不带那次请求的信息）。
// fetch 为 nil 时用 FetchRevocations。
func (s *Service) RevocationLoop(ctx context.Context, dataDir string, log *slog.Logger, fetch func(context.Context) ([]byte, time.Time, error)) {
	if fetch == nil {
		fetch = FetchRevocations
	}
	dir := filepath.Join(dataDir, "attestation")
	if r, err := loadRevocations(dir); err == nil {
		s.SetRevocations(r)
	} else if errors.Is(err, errFutureRevocations) {
		// 删掉，免得连不上 Google 的面板每次启动都读到它、记一遍警告。
		log.Warn("保存的吊销列表日期晚于现在，已删除，改用随面板发布的列表", "err", err)
		_ = os.Remove(filepath.Join(dir, "status.updated"))
		_ = os.Remove(filepath.Join(dir, "status.json"))
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("读取保存的吊销列表失败", "err", err)
	}
	// recheck：要复查一遍已添加的手机（启动时、换上新列表时、上次复查失败时）。
	recheck := true
	for {
		wait := revocationEvery
		b, updated, err := fetch(ctx)
		if err != nil {
			// 中国大陆的面板通常连不上 Google，这时一直用随面板发布的列表，不当作错误。
			log.Debug("获取吊销列表失败", "err", err)
			wait = revocationRetry
		} else if r, err := attestation.ParseRevocations(b, updated); err != nil {
			// 取到了却读不了：Google 改了格式，面板会一直用旧的列表。
			log.Warn("吊销列表无法解析，继续用当前的列表", "err", err)
			wait = revocationRetry
		} else if s.SetRevocations(r) {
			if err := saveRevocations(dir, b, updated); err != nil {
				log.Warn("保存吊销列表失败", "err", err)
			}
			recheck = true
		}
		next := time.Now().Add(wait)
		for {
			if recheck {
				if err := s.RecheckAppDevices(ctx); err != nil {
					// 复查失败时最多 6 小时后再取一次列表、再复查。
					log.Warn("按吊销列表复查手机失败", "err", err)
					if t := time.Now().Add(revocationRetry); t.Before(next) {
						next = t
					}
				} else {
					recheck = false
				}
			}
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.recheckSignal:
				timer.Stop()
				recheck = true
				continue
			case <-timer.C:
			}
			break
		}
	}
}

// requestRecheck 让 RevocationLoop 尽快复查一遍已添加的手机（不等它，它没在运行时什么也不做）。
func (s *Service) requestRecheck() {
	select {
	case s.recheckSignal <- struct{}{}:
	default:
	}
}

// FetchRevocations 从 Google 取吊销列表，返回内容和日期（Last-Modified，没有时是现在）。
func FetchRevocations(ctx context.Context) ([]byte, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, attestation.StatusURL, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, attestation.MaxStatusBytes+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	updated := time.Now().UTC().Truncate(time.Second)
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil && !t.After(updated) {
		updated = t.UTC()
	}
	return b, updated, nil
}

// errFutureRevocations：保存的列表日期晚于现在（见 loadRevocations）。
var errFutureRevocations = errors.New("保存的吊销列表日期晚于现在")

// 数据目录里存的列表：status.json 是 Google 返回的原文，status.updated 是它的日期。
func loadRevocations(dir string) (*attestation.Revocations, error) {
	d, err := os.ReadFile(filepath.Join(dir, "status.updated"))
	if err != nil {
		return nil, err
	}
	updated, err := time.Parse(time.RFC3339, strings.TrimSpace(string(d)))
	if err != nil {
		return nil, err
	}
	// 日期晚于现在（面板的时钟曾经快过，或者文件被改过）时不用这份：按现在算的话，每次启动它都和启动时一样新，
	// 随新版本发布的列表永远换不上它。改用随面板发布的，下次取到新列表时覆盖这份文件。
	if updated.After(time.Now()) {
		return nil, fmt.Errorf("%w：%s", errFutureRevocations, updated.Format(time.RFC3339))
	}
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return nil, err
	}
	return attestation.ParseRevocations(b, updated)
}

func saveRevocations(dir string, b []byte, updated time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 先写内容再写日期：中途失败时日期还是旧的，下次读到的要么是旧的一对，要么解析失败（用随面板发布的）。
	if err := writeFileAtomic(filepath.Join(dir, "status.json"), b); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "status.updated"), []byte(updated.UTC().Format(time.RFC3339)+"\n"))
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RecheckAppDevices 按当前的吊销列表复查已经添加的手机：新查到被吊销的记下原因、记审计、告警一次（不退出，由成员或管理员决定）；
// 不再在列表里的清掉标记。查询或写入失败时返回错误（其余的照常复查），调用方稍后重试。
func (s *Service) RecheckAppDevices(ctx context.Context) error {
	s.recheckMu.Lock()
	defer s.recheckMu.Unlock()
	list, err := s.q.AppDevicesWithSerials(ctx)
	if err != nil {
		return err
	}
	rev := s.Revocations()
	var failed error
	for _, d := range list {
		serial, reason, revoked := rev.Check(strings.Fields(d.AttestationSerials))
		switch {
		case revoked && d.RevokedReason == "":
			if err := s.q.SetAppDeviceRevoked(ctx, d.ID, reason); err != nil {
				failed = err
				continue
			}
			at, aid, an := userActor(d.UserID, d.Username)
			b, _ := json.Marshal(map[string]string{"device": d.Name, "serial": serial, "reason": reason})
			s.loginEvent(ctx, audit.Event{Category: audit.Security, Action: "app_attestation_revoked", ActorType: at, ActorID: aid,
				ActorName: an, IP: d.LastIP, Via: audit.ViaApp, Client: d.Name, Session: audit.SessionTag("app:" + d.ID), Params: string(b)})
			if s.Alert != nil {
				s.Alert(context.WithoutCancel(ctx), "app-revoked:"+d.ID, i18n.New("手机的证明证书已被吊销").Error(),
					i18n.Errorf("账号 %s 的手机 %s 的证明证书已被 Google 吊销（%s），它的密钥可能不在可信的硬件里。请确认后在「已登录的设备」里退出这台手机。", d.Username, d.Name, reason).Error())
			}
		case !revoked && d.RevokedReason != "":
			if err := s.q.SetAppDeviceRevoked(ctx, d.ID, ""); err != nil {
				failed = err
			}
		}
	}
	return failed
}
