package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"golang.org/x/crypto/curve25519"
)

// identity 是节点的身份：身份私钥、WireGuard 私钥，以及信任的面板列表。
// 文件放在 <数据目录>/identity 下，私钥不离开节点。
type identity struct {
	dir   string
	key   *ecdsa.PrivateKey
	wgKey []byte

	mu     sync.Mutex
	panels []trustedPanel
}

// trustedPanel 是信任列表里的一个面板。每个面板有自己的 CA，给节点签发各自的证书。
type trustedPanel struct {
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
	NodeID      string `json:"node_id"`
	CertPEM     string `json:"cert"`
	CAPEM       string `json:"ca"`
}

func (id *identity) keyPath() string    { return filepath.Join(id.dir, "key.pem") }
func (id *identity) wgPath() string     { return filepath.Join(id.dir, "wireguard.key") }
func (id *identity) panelsPath() string { return filepath.Join(id.dir, "panels.json") }

// loadIdentity 读取身份，不存在时生成新的私钥。
func loadIdentity(dir string) (*identity, error) {
	id := &identity{dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	key, err := tlsutil.ReadKey(id.keyPath())
	if errors.Is(err, os.ErrNotExist) {
		if key, err = tlsutil.GenerateKey(); err != nil {
			return nil, err
		}
		if err := tlsutil.WriteKey(id.keyPath(), key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	id.key = key

	wg, err := os.ReadFile(id.wgPath())
	if errors.Is(err, os.ErrNotExist) {
		wg = make([]byte, curve25519.ScalarSize)
		if _, err := rand.Read(wg); err != nil {
			return nil, err
		}
		// WireGuard 私钥的标准钳位处理
		wg[0] &= 248
		wg[31] = (wg[31] & 127) | 64
		enc := base64.StdEncoding.EncodeToString(wg)
		if err := os.WriteFile(id.wgPath(), []byte(enc), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if wg, err = base64.StdEncoding.DecodeString(string(wg)); err != nil {
		return nil, fmt.Errorf("WireGuard 私钥文件损坏: %w", err)
	}
	id.wgKey = wg

	data, err := os.ReadFile(id.panelsPath())
	if err == nil {
		if err := json.Unmarshal(data, &id.panels); err != nil {
			return nil, fmt.Errorf("信任列表文件损坏: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return id, nil
}

func (id *identity) wgPublicKey() ([]byte, error) {
	return curve25519.X25519(id.wgKey, curve25519.Basepoint)
}

// Panels 返回信任列表的副本。
func (id *identity) Panels() []trustedPanel {
	id.mu.Lock()
	defer id.mu.Unlock()
	return append([]trustedPanel(nil), id.panels...)
}

// trusts 判断某个面板是否已在信任列表里。
func (id *identity) trusts(fingerprint string) bool {
	for _, p := range id.Panels() {
		if p.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

func (id *identity) addPanel(p trustedPanel) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	panels := append([]trustedPanel(nil), id.panels...)
	replaced := false
	for i := range panels {
		if panels[i].Fingerprint == p.Fingerprint {
			panels[i] = p
			replaced = true
		}
	}
	if !replaced {
		panels = append(panels, p)
	}
	data, err := json.MarshalIndent(panels, "", "  ")
	if err != nil {
		return err
	}
	tmp := id.panelsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, id.panelsPath()); err != nil {
		return err
	}
	id.panels = panels
	return nil
}

// enroll 用一次性令牌加入面板，把面板加入信任列表。
func (id *identity) enroll(ctx context.Context, address, fingerprint, token, hostname string) error {
	csr, err := tlsutil.CreateCSR(id.key, hostname)
	if err != nil {
		return err
	}
	wgPub, err := id.wgPublicKey()
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsutil.PinnedConfig(fingerprint), ForceAttemptHTTP2: true},
	}
	client := nagiv1connect.NewEnrollServiceClient(httpClient, "https://"+address)
	resp, err := client.Enroll(ctx, connect.NewRequest(&nagiv1.EnrollRequest{
		Token:              token,
		Csr:                csr,
		WireguardPublicKey: wgPub,
		Hostname:           hostname,
	}))
	if err != nil {
		return fmt.Errorf("加入面板失败: %w", err)
	}
	return id.addPanel(trustedPanel{
		Address:     address,
		Fingerprint: fingerprint,
		NodeID:      resp.Msg.GetNodeId(),
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.Msg.GetCertificate()})),
		CAPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.Msg.GetCaCertificate()})),
	})
}

// tlsConfig 返回连接某个面板用的 TLS 配置：出示该面板签发的客户端证书，并钉住面板公钥。
func (id *identity) tlsConfig(p trustedPanel, alpn string) (*tls.Config, error) {
	block, _ := pem.Decode([]byte(p.CertPEM))
	if block == nil {
		return nil, errors.New("信任列表里的证书无效")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, err
	}
	cfg := tlsutil.PinnedConfig(p.Fingerprint)
	cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{block.Bytes}, PrivateKey: id.key}}
	cfg.NextProtos = []string{alpn}
	return cfg, nil
}
