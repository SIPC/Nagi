package spec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"strings"

	"golang.org/x/crypto/ssh"
)

// NewDeployKey 生成一对 ed25519 的 SSH deploy key：私钥（OpenSSH 格式）和公钥（authorized_keys 格式）。
func NewDeployKey() (private []byte, public string, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(privKey, "nagi deploy key")
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(block), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " nagi", nil
}

// RandomHex 生成 n 字节的随机数（十六进制）。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
