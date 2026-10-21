// Package standby 是主备面板（见 docs/standby.md）：备用面板经面板端口（ALPN nagi-standby，mTLS）连主面板，
// 主面板的数据库有变化就把一份一致的快照发给它，备用面板按表替换自己的数据，保留本地的设置和登录会话。
package standby

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// ALPN 是同步连接使用的 TLS 应用层协议名，面板据此把它和浏览器、节点的连接分开。
const ALPN = "nagi-standby/1"

const (
	codePrefix = "nagi-standby:"
	tokenTTL   = time.Hour
	// checkEvery：主面板多久看一次数据库有没有变化。
	checkEvery = 2 * time.Second
	// heartbeat：没有变化时也定时发一个空帧，备用面板据此知道连接还在。
	heartbeat = 15 * time.Second
	maxFrame  = 512 << 20
)

// LocalSettings 是每个面板自己的设置，不随同步覆盖。
var LocalSettings = []string{"port", "public_address", "panel_domain", "setup_token", "proxy_secret", SettingSeenTerm}

// localTables 是不同步的表：登录会话各面板自己的。
var localTables = map[string]bool{"sessions": true}

// ---- 配对码 ----

type code struct {
	Address     string `json:"a"`
	Fingerprint string `json:"f"`
	Token       string `json:"t"`
}

func encodeCode(c code) string {
	b, _ := json.Marshal(c)
	return codePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeCode(s string) (code, error) {
	var c code
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), codePrefix))
	if err != nil || !strings.HasPrefix(strings.TrimSpace(s), codePrefix) || json.Unmarshal(raw, &c) != nil || c.Address == "" || c.Fingerprint == "" || c.Token == "" {
		return c, errors.New("配对码格式不对，应该是 nagi-standby:...")
	}
	return c, nil
}

// ---- 主面板 ----

// Primary 是主面板这一侧：生成配对码、处理配对、给备用面板发快照。
type Primary struct {
	Q       *store.Queries
	DataDir string
	CA      *tlsutil.CA
	Log     *slog.Logger

	mu     sync.Mutex
	status map[string]*Status // 备用面板 ID → 连接状态（只在内存里：写进数据库会触发下一次同步）
}

// Status 是一个备用面板的同步状态。
type Status struct {
	Connected bool
	LastSync  time.Time
}

// NewCode 生成配对码：主面板的地址、公钥指纹和一次性令牌（1 小时内有效）。
func (p *Primary) NewCode(ctx context.Context, address, fingerprint string) (string, time.Time, error) {
	token, err := auth.RandomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	expires := now.Add(tokenTTL)
	if err := p.Q.InsertStandbyToken(ctx, store.InsertStandbyTokenParams{TokenHash: auth.HashToken(token), CreatedAt: now.Unix(), ExpiresAt: expires.Unix()}); err != nil {
		return "", time.Time{}, err
	}
	return encodeCode(code{Address: address, Fingerprint: fingerprint, Token: token}), expires, nil
}

type pairRequest struct {
	Token   string `json:"token"`
	Name    string `json:"name"`
	Address string `json:"address"`
	// ServerFP 是备用面板服务端公钥的指纹：节点连备用面板时钉住它。
	ServerFP string `json:"server_fp"`
}

type pairResponse struct {
	CACert    []byte `json:"ca_cert"`
	CAKey     []byte `json:"ca_key"`
	MasterKey []byte `json:"master_key"`
}

// HandlePair 处理配对请求（POST /api/standby/pair）：校验并作废令牌，登记备用面板（地址和服务端公钥指纹），
// 把面板 CA 和主密钥交给它（主备面板共用 CA 和主密钥）。连接由备用面板钉住主面板的公钥指纹。
func (p *Primary) HandlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	var req pairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求格式不对", http.StatusBadRequest)
		return
	}
	if req.ServerFP == "" || req.Address == "" {
		http.Error(w, "缺少备用面板的地址或公钥指纹", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	n, err := p.Q.UseStandbyToken(ctx, store.UseStandbyTokenParams{TokenHash: auth.HashToken(req.Token), ExpiresAt: time.Now().Unix()})
	if err != nil || n == 0 {
		http.Error(w, "配对码无效或已过期，请在主面板上重新生成", http.StatusForbidden)
		return
	}
	id, err := auth.RandomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id = strings.ToLower(id[:12])
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Address
	}
	if err := p.Q.InsertStandbyPanel(ctx, store.InsertStandbyPanelParams{
		ID: id, Name: name, Address: req.Address, ServerFp: req.ServerFP, CreatedAt: time.Now().Unix(),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := AddMember(ctx, p.Q, req.ServerFP); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	caKey, err := os.ReadFile(filepath.Join(p.DataDir, "pki", "ca.key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	master, err := os.ReadFile(filepath.Join(p.DataDir, "pki", "master.key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := pairResponse{CACert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA.Cert.Raw}), CAKey: caKey, MasterKey: master}
	p.Log.Info("备用面板已配对", "standby", id, "name", name, "address", req.Address)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// StatusOf 返回备用面板的同步状态。
func (p *Primary) StatusOf(id string) Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.status[id]; st != nil {
		return *st
	}
	return Status{}
}

func (p *Primary) setStatus(id string, f func(*Status)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == nil {
		p.status = map[string]*Status{}
	}
	if p.status[id] == nil {
		p.status[id] = &Status{}
	}
	f(p.status[id])
}

// Serve 处理一个备用面板的同步连接：确认它是登记过的备用面板，先发一份完整快照，之后数据库一有变化就再发。
func (p *Primary) Serve(ctx context.Context, conn *tls.Conn) {
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return
	}
	// 客户端证书用的是备用面板的服务端密钥，按它的指纹认出是哪个备用面板。
	fp := tlsutil.Fingerprint(state.PeerCertificates[0])
	sp, err := p.Q.GetStandbyPanelByServerFP(ctx, fp)
	if err != nil {
		p.Log.Warn("拒绝未登记或已移除的备用面板", "fingerprint", fp, "remote", conn.RemoteAddr())
		return
	}
	id := sp.ID
	// 另开一个连接看数据库有没有变化：data_version 只在别的连接提交时才变。
	watch, err := db.Open(filepath.Join(p.DataDir, "panel.db"))
	if err != nil {
		p.Log.Error("打开数据库失败", "err", err)
		return
	}
	defer watch.Close()
	p.setStatus(id, func(s *Status) { s.Connected = true })
	defer p.setStatus(id, func(s *Status) { s.Connected = false })
	p.Log.Info("备用面板已连接", "standby", id, "name", sp.Name)

	var last int64 = -1
	lastSent := time.Time{}
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		// 在主面板上移除以后立即断开。
		if _, err := p.Q.GetStandbyPanel(ctx, id); err != nil {
			p.Log.Info("备用面板已移除，断开同步连接", "standby", id)
			return
		}
		var v int64
		if err := watch.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
			return
		}
		var frame []byte
		if v != last {
			if frame, err = snapshot(ctx, watch, p.DataDir); err != nil {
				p.Log.Warn("生成数据库快照失败", "err", err)
				return
			}
			last = v
		} else if time.Since(lastSent) < heartbeat {
			goto wait
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
		if err := writeFrame(conn, frame); err != nil {
			return
		}
		lastSent = time.Now()
		if frame != nil {
			p.setStatus(id, func(s *Status) { s.LastSync = time.Now() })
		}
	wait:
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// snapshot 生成一份一致的数据库快照（VACUUM INTO）。
func snapshot(ctx context.Context, d *sql.DB, dataDir string) ([]byte, error) {
	tmp := filepath.Join(dataDir, fmt.Sprintf(".standby-snapshot-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := d.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp)
}

func writeFrame(w io.Writer, b []byte) error {
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint64(hdr[:])
	if n > maxFrame {
		return nil, errors.New("快照太大")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

// ---- 备用面板 ----

// Info 是备用面板跟随的主面板，存在 <数据目录>/standby.json。有这个文件就是备用面板。
type Info struct {
	PrimaryAddress     string `json:"primary_address"`
	PrimaryFingerprint string `json:"primary_fingerprint"`
}

// GetPrimaryAddress 返回主面板的地址；info 为 nil（不是备用面板）时为空。
func (i *Info) GetPrimaryAddress() string {
	if i == nil {
		return ""
	}
	return i.PrimaryAddress
}

func infoPath(dataDir string) string { return filepath.Join(dataDir, "standby.json") }

// Load 读取备用面板的配对信息；不是备用面板时返回 nil。
func Load(dataDir string) (*Info, error) {
	b, err := os.ReadFile(infoPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info := &Info{}
	return info, json.Unmarshal(b, info)
}

// Pair 用配对码把一个全新的数据目录配成备用面板：生成客户端密钥、向主面板登记，写下 CA、主密钥、客户端证书和配对信息。
func Pair(ctx context.Context, dataDir, codeText, name, address string) error {
	c, err := decodeCode(codeText)
	if err != nil {
		return err
	}
	pki := filepath.Join(dataDir, "pki")
	for _, f := range []string{"ca.pem", "ca.key", "master.key"} {
		if _, err := os.Stat(filepath.Join(pki, f)); err == nil {
			return fmt.Errorf("数据目录 %s 已经有面板数据，备用面板要用一个空的数据目录", dataDir)
		}
	}
	if err := os.MkdirAll(pki, 0o700); err != nil {
		return err
	}
	// 服务端私钥现在就生成（面板启动时沿用），把它的指纹告诉主面板：节点连本面板时钉住它。
	serverKey, err := tlsutil.GenerateKey()
	if err != nil {
		return err
	}
	serverFP, err := tlsutil.KeyFingerprint(serverKey.Public())
	if err != nil {
		return err
	}
	body, _ := json.Marshal(pairRequest{Token: c.Token, Name: name, Address: address, ServerFP: serverFP})
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsutil.PinnedConfig(c.Fingerprint)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.Address+"/api/standby/pair", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接主面板 %s: %w", c.Address, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("主面板拒绝配对：%s", strings.TrimSpace(string(msg)))
	}
	var resp pairResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return err
	}
	files := map[string][]byte{"ca.pem": resp.CACert, "ca.key": resp.CAKey, "master.key": resp.MasterKey}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(pki, name), data, 0o600); err != nil {
			return err
		}
	}
	if err := tlsutil.WriteKey(filepath.Join(pki, "server.key"), serverKey); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dataDir, pairedMarker), nil, 0o600); err != nil {
		return err
	}
	return Follow(dataDir, c.Address, c.Fingerprint)
}

// pairedMarker 表示数据目录是配对来的（不是第一个主面板）：这样的面板不带内嵌节点，提升以后也不带。
const pairedMarker = "paired"

// Paired 判断数据目录是不是配对来的。
func Paired(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pairedMarker))
	return err == nil
}

// Follow 让本面板作为备用面板跟随（同步）某个主面板：写 standby.json。
func Follow(dataDir, address, fingerprint string) error {
	b, _ := json.MarshalIndent(Info{PrimaryAddress: address, PrimaryFingerprint: fingerprint}, "", "  ")
	tmp := infoPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, infoPath(dataDir))
}

// Client 是备用面板这一侧：连着主面板，收到快照就应用。
type Client struct {
	Info    *Info
	DataDir string
	DB      *sql.DB
	Log     *slog.Logger

	mu        sync.Mutex
	connected bool
	lastSync  time.Time
	lastError string
	stop      context.CancelFunc
	done      chan struct{}
}

// ClientStatus 是备用面板看到的同步状态。
type ClientStatus struct {
	Connected bool
	LastSync  time.Time
	LastError string
}

func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClientStatus{Connected: c.connected, LastSync: c.lastSync, LastError: c.lastError}
}

// Run 一直连着主面板同步，断了就重连，直到 ctx 取消或 Stop。
func (c *Client) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.stop, c.done = cancel, make(chan struct{})
	done := c.done
	c.mu.Unlock()
	defer close(done)
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.syncOnce(ctx)
		c.mu.Lock()
		c.connected = false
		if err != nil && ctx.Err() == nil {
			c.lastError = err.Error()
		}
		c.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			c.Log.Warn("与主面板的同步连接断开，稍后重连", "primary", c.Info.PrimaryAddress, "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// Stop 停止同步并等它退出（提升为主面板前调用：之后不会再有快照覆盖本地数据）。
func (c *Client) Stop() {
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// clientCert 用本面板的服务端密钥签一张客户端证书（各面板都有 CA），主面板据公钥指纹认出本面板。
func clientCert(dataDir string) (tls.Certificate, error) {
	pki := filepath.Join(dataDir, "pki")
	caCert, err := tlsutil.ReadCert(filepath.Join(pki, "ca.pem"))
	if err != nil {
		return tls.Certificate{}, err
	}
	caKey, err := tlsutil.ReadKey(filepath.Join(pki, "ca.key"))
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := tlsutil.ReadKey(filepath.Join(pki, "server.key"))
	if err != nil {
		return tls.Certificate{}, err
	}
	csr, err := tlsutil.CreateCSR(key, "standby")
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := (&tlsutil.CA{Cert: caCert, Key: caKey}).SignCSR(csr, "standby", 24*time.Hour)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}, nil
}

func (c *Client) syncOnce(ctx context.Context) error {
	cert, err := clientCert(c.DataDir)
	if err != nil {
		return err
	}
	cfg := tlsutil.PinnedConfig(c.Info.PrimaryFingerprint)
	cfg.Certificates = []tls.Certificate{cert}
	cfg.NextProtos = []string{ALPN}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", c.Info.PrimaryAddress, cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	first := true
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * heartbeat))
		frame, err := readFrame(conn)
		if err != nil {
			if first && errors.Is(err, io.EOF) {
				return errors.New("主面板拒绝了同步（本面板可能已在主面板上被移除）")
			}
			return err
		}
		if first {
			first = false
			c.mu.Lock()
			c.connected, c.lastError = true, ""
			c.mu.Unlock()
			c.Log.Info("已连接主面板", "primary", c.Info.PrimaryAddress)
		}
		if len(frame) == 0 {
			continue // 心跳
		}
		if err := c.apply(ctx, frame); err != nil {
			c.mu.Lock()
			c.lastError = "应用主面板的数据失败：" + err.Error()
			c.mu.Unlock()
			c.Log.Error("应用主面板的数据失败", "err", err)
			continue
		}
		c.mu.Lock()
		c.lastSync = time.Now()
		c.mu.Unlock()
	}
}

// apply 用快照替换本地数据：除了本地的表和设置，每张表清空后从快照复制。整个过程在一个事务里。
func (c *Client) apply(ctx context.Context, snap []byte) error {
	tmp := filepath.Join(c.DataDir, ".standby-received.db")
	if err := os.WriteFile(tmp, snap, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	conn, err := c.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS snap", tmp); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "DETACH DATABASE snap")
	var theirs, ours int
	if err := conn.QueryRowContext(ctx, "PRAGMA snap.user_version").Scan(&theirs); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA main.user_version").Scan(&ours); err != nil {
		return err
	}
	if theirs != ours {
		return fmt.Errorf("主面板的数据库版本（%d）和本面板（%d）不同，请把两个面板升级到同一个版本", theirs, ours)
	}
	// 表名取本面板自己的数据库里有的（两边版本相同，表一样）：不拿快照里的名字拼 SQL。
	rows, err := conn.QueryContext(ctx, "SELECT name FROM main.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' "+
		"AND name IN (SELECT name FROM snap.sqlite_master WHERE type = 'table')")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, n)
	}
	rows.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	local := "'" + strings.Join(LocalSettings, "','") + "'"
	for _, t := range tables {
		if localTables[t] {
			continue
		}
		q := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if t == "settings" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM main.settings WHERE key NOT IN ("+local+")"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO main.settings SELECT * FROM snap.settings WHERE key NOT IN ("+local+")"); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM main."+q); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO main."+q+" SELECT * FROM snap."+q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
