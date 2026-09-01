// Package dbsecrets 把凭据以密文形式存进平台自己的数据库。
//
// 存在的理由是可用性：另外两个后端都要求在平台之外先把私钥放好——DIR 要
// 运维 kubectl 往挂载目录塞文件，SECRET_MANAGER 要一整套 GCP。两条都不是
// "在界面上配一个仓库"（design doc 2026-09-01 §1）。
//
// **加密密钥不在数据库里。** GitRepo.CredentialRef 上那条"凭据永不入库"守的
// 是"能不能从数据库 dump 出凭据"，而这里落的是 AES-GCM 密文，KEK 来自启动
// 配置。拿到完整转储得到的是一堆解不开的字节，那句话仍然成立。
//
// KEK 与密文放在一起就等于没加密——这是本包唯一不能违反的一条。
package dbsecrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// KEKSize 是密钥长度：AES-256。
const KEKSize = 32

// ErrNoKEK 表示没有提供加密密钥。
//
// 单独一个错误值，是为了让装配层能把它与"密钥格式不对"分开报——
// 前者是漏配了一项，后者是配错了内容，处置不同。
var ErrNoKEK = errors.New("dbsecrets: no encryption key")

// ErrUndecryptable 表示这条密文解不开。
//
// **不区分"密钥不对"与"密文被改过"**：两者都意味着这条凭据不可用，而把
// 它们分开报等于告诉攻击者哪一半猜对了。
var ErrUndecryptable = errors.New("dbsecrets: the stored credential cannot be decrypted")

// Cipher 用一把 KEK 加解密凭据。
type Cipher struct {
	aead  cipher.AEAD
	keyID string
}

// NewCipher 用给定的 KEK 构造。
//
// KEK 必须恰好 KEKSize 字节。**不接受短密钥、也不做任何拉伸**：从一个短口令
// 派生出 32 字节是可以做的，但那会让"密钥有多强"取决于调用方填了什么，
// 而这里看不出来。要一把真随机的密钥，就明确要求一把真随机的密钥。
func NewCipher(kek []byte) (*Cipher, error) {
	if len(kek) == 0 {
		return nil, ErrNoKEK
	}
	if len(kek) != KEKSize {
		return nil, fmt.Errorf("dbsecrets: the encryption key must be %d bytes, got %d",
			KEKSize, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("dbsecrets: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("dbsecrets: build gcm: %w", err)
	}
	return &Cipher{aead: aead, keyID: keyIDOf(kek)}, nil
}

// KeyID 是这把 KEK 的稳定标识，落在每一行上供轮换时区分。
//
// 取 KEK 的 SHA-256 前 8 字节：足够区分不同的密钥，而**从它推不回 KEK**。
// 直接存一个人给的名字也行，但那要求填的人保证唯一，而填错的后果是
// 轮换时认错行。
func (c *Cipher) KeyID() string { return c.keyID }

func keyIDOf(kek []byte) string {
	sum := sha256.Sum256(kek)
	return hex.EncodeToString(sum[:8])
}

// Seal 加密一段凭据，返回 nonce 与密文。
//
// ref 作为附加认证数据（AAD）：它不被加密，但被认证。把 A 仓库的密文行
// 改挂到 B 仓库的 ref 上，解密会失败而不是解出一把错的钥匙——那种失败会
// 表现成"仓库不可达"，排查方向完全错。
func (c *Cipher) Seal(ref string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if ref == "" {
		return nil, nil, errors.New("dbsecrets: a credential needs a ref")
	}
	if len(plaintext) == 0 {
		return nil, nil, errors.New("dbsecrets: refusing to store an empty credential")
	}
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		// 取不到随机数就整个失败，不退回一个可预测的 nonce：
		// GCM 下 nonce 重用同时毁掉机密性与完整性。
		return nil, nil, fmt.Errorf("dbsecrets: read nonce: %w", err)
	}
	return nonce, c.aead.Seal(nil, nonce, plaintext, []byte(ref)), nil
}

// Open 解密一段凭据。
func (c *Cipher) Open(ref string, nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) != c.aead.NonceSize() {
		return nil, ErrUndecryptable
	}
	out, err := c.aead.Open(nil, nonce, ciphertext, []byte(ref))
	if err != nil {
		// 底层错误不外带：它区分不出"密钥不对"与"密文被改过"，而带上去
		// 只会让日志里多一行同样区分不出的文字。
		return nil, ErrUndecryptable
	}
	return out, nil
}
