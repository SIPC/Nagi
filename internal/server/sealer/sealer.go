// Package sealer 用面板主密钥加密面板上保存的敏感数据（例如项目配置备份里的密钥）。
// 主密钥单独存成一个文件，数据库文件泄露也读不出这些数据。
package sealer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// version 是密文格式的版本号，放在密文的第一个字节。
const version = 1

// Sealer 用 AES-256-GCM 加密和解密。
type Sealer struct {
	aead cipher.AEAD
}

// Load 读取主密钥文件，不存在时生成一个新的（32 字节随机数，权限 0600）。
func Load(path string) (*Sealer, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥文件 %s 损坏", path)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal 加密数据。additional 是绑定的上下文（例如 "节点 ID/项目 ID"），解密时必须一致。
func (s *Sealer) Seal(plaintext, additional []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{version}, nonce...)
	return s.aead.Seal(out, nonce, plaintext, additional), nil
}

// Open 解密 Seal 生成的数据。
func (s *Sealer) Open(ciphertext, additional []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(ciphertext) < 1+n || ciphertext[0] != version {
		return nil, errors.New("密文格式无效")
	}
	return s.aead.Open(nil, ciphertext[1:1+n], ciphertext[1+n:], additional)
}
