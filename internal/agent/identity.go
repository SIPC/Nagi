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

	if id.panels, err = id.readPanels(); err != nil {
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

// panel 返回信任列表里的某个面板。
func (id *identity) panel(fingerprint string) (trustedPanel, bool) {
	for _, p := range id.Panels() {
		if p.Fingerprint == fingerprint {
			return p, true
		}
	}
	return trustedPanel{}, false
}

func (id *identity) readPanels() ([]trustedPanel, error) {
	data, err := os.ReadFile(id.panelsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var panels []trustedPanel
	if err := json.Unmarshal(data, &panels); err != nil {
		return nil, fmt.Errorf("信任列表文件损坏: %w", err)
	}
	return panels, nil
}

// reload 重新读取信任列表文件。节点本地命令会直接修改这个文件。
func (id *identity) reload() error {
	id.mu.Lock()
	defer id.mu.Unlock()
	panels, err := id.readPanels()
	if err != nil {
		return err
	}
	id.panels = panels
	return nil
}

// update 以文件里的最新内容为准修改信任列表，避免覆盖本地命令刚做的修改。
func (id *identity) update(fn func([]trustedPanel) ([]trustedPanel, error)) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	panels, err := id.readPanels()
	if err != nil {
		return err
	}
	if panels, err = fn(panels); err != nil {
		return err
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

func (id *identity) addPanel(p trustedPanel) error {
	return id.update(func(panels []trustedPanel) ([]trustedPanel, error) {
		for i := range panels {
			if panels[i].Fingerprint == p.Fingerprint {
				panels[i] = p
				return panels, nil
			}
		}
		return append(panels, p), nil
	})
}

// errLastPanel 表示要移除的是最后一个面板：移除后节点不再被任何面板管理。
var errLastPanel = errors.New("这是节点信任的最后一个面板，移除后节点将不受任何面板管理")

// errFailoverPanel 表示移除这个面板后，顶替的参与方之间联系不上（见 witnessguard.go）。
var errFailoverPanel = errors.New("移除后，这些项目的顶替参与方之间联系不上")

// removePanel 从信任列表移除面板。which 是公钥指纹或地址。force 时允许移除最后一个面板。
// guard 不为 nil 时先检查这个面板能不能移除（见证人保护）。
func (id *identity) removePanel(which string, force bool, guard func(fingerprint string) error) (trustedPanel, error) {
	var removed trustedPanel
	err := id.update(func(panels []trustedPanel) ([]trustedPanel, error) {
		for i, p := range panels {
			if p.Fingerprint == which || p.Address == which {
				if len(panels) == 1 && !force {
					return nil, errLastPanel
				}
				if guard != nil {
					if err := guard(p.Fingerprint); err != nil {
						return nil, err
					}
				}
				removed = p
				return append(panels[:i:i], panels[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("信任列表里没有 %s", which)
	})
	return removed, err
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

// certificate 返回这个面板签发给节点的证书。
func (p trustedPanel) certificate() (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(p.CertPEM))
	if block == nil {
		return nil, errors.New("信任列表里的证书无效")
	}
	return x509.ParseCertificate(block.Bytes)
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
