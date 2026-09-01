package dbsecrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/secrets"
)

// maxCredentialBytes 是单条凭据的上限，与列宽一致。
//
// 一把 RSA 4096 私钥约 3.2 KB，8 KB 留了余量。超限直接拒，不截断——
// 截断之后存进去的是一把解析不了的私钥，而它的失败会推迟到写回那一刻。
const maxCredentialBytes = 8192

// Store 把凭据以密文存进平台数据库，并按 ref 取回。
//
// 同时实现读（secrets.Resolver）与写：两端必须用同一个 Cipher，
// 拆成两个类型就多了一个"加密用的密钥与解密用的不是同一把"的位置。
type Store struct {
	db     *sql.DB
	cipher *Cipher
}

// New 构造。cipher 为 nil 直接报错，不退回明文存储。
func New(db *sql.DB, cipher *Cipher) (*Store, error) {
	if db == nil {
		return nil, errors.New("dbsecrets: no database")
	}
	if cipher == nil {
		return nil, ErrNoKEK
	}
	return &Store{db: db, cipher: cipher}, nil
}

// 编译期确认它仍然是一个凭据解析器。
var _ secrets.Resolver = (*Store)(nil)

// Put 写入一条凭据，已存在则覆盖。
//
// **覆盖而不是报冲突**：换一把私钥是常规运维动作，而"先删再建"会在两步
// 之间留下一个仓库没有凭据的窗口。
func (s *Store) Put(ctx context.Context, ref string, plaintext []byte) error {
	if err := secrets.ValidateRef(ref); err != nil {
		return err
	}
	if len(plaintext) > maxCredentialBytes {
		return fmt.Errorf("dbsecrets: the credential is larger than %d bytes", maxCredentialBytes)
	}
	nonce, ciphertext, err := s.cipher.Seal(ref, plaintext)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO stored_secret (ref, nonce, ciphertext, key_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   nonce      = VALUES(nonce),
		   ciphertext = VALUES(ciphertext),
		   key_id     = VALUES(key_id),
		   updated_at = VALUES(updated_at)`,
		ref, nonce, ciphertext, s.cipher.KeyID(), now, now); err != nil {
		// 底层错误不外带：它可能带上参数值，而其中一个参数是密文。
		return errors.New("dbsecrets: cannot store the credential")
	}
	return nil
}

// Resolve 取回一条凭据的明文。
//
// **这是唯一的取出路径，且只服务平台自己**：没有 HTTP 接口透出它，
// 也没有回显。私钥写进去之后，除了平台拿去连 Git，谁都取不出来。
func (s *Store) Resolve(ctx context.Context, ref string) ([]byte, error) {
	if err := secrets.ValidateRef(ref); err != nil {
		return nil, err
	}
	var nonce, ciphertext []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT nonce, ciphertext FROM stored_secret WHERE ref = ?`, ref).
		Scan(&nonce, &ciphertext)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 与"解不开"分开：前者是没配过，后者是配过但用不了，处置不同。
		return nil, secrets.ErrNotFound
	case err != nil:
		return nil, errors.New("dbsecrets: cannot read the credential")
	}
	return s.cipher.Open(ref, nonce, ciphertext)
}

// Delete 删除一条凭据。仓库被删时一并清掉，不留孤儿密文。
func (s *Store) Delete(ctx context.Context, ref string) error {
	if err := secrets.ValidateRef(ref); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM stored_secret WHERE ref = ?`, ref); err != nil {
		return errors.New("dbsecrets: cannot delete the credential")
	}
	return nil
}

// Has 报告某个 ref 是否已经配过凭据。
//
// 供接口回答"这个仓库配没配凭据"——那是个可以说的事实，
// 而凭据内容不是。
func (s *Store) Has(ctx context.Context, ref string) (bool, error) {
	if err := secrets.ValidateRef(ref); err != nil {
		return false, err
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM stored_secret WHERE ref = ?`, ref).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, errors.New("dbsecrets: cannot read the credential")
	}
	return true, nil
}
