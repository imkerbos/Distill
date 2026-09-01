package dbsecrets_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/secrets/dbsecrets"
)

func kek(b byte) []byte { return bytes.Repeat([]byte{b}, dbsecrets.KEKSize) }

func newCipher(t *testing.T, k []byte) *dbsecrets.Cipher {
	t.Helper()
	c, err := dbsecrets.NewCipher(k)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestSealOpenRoundTrip(t *testing.T) {
	c := newCipher(t, kek(1))
	want := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END-----\n")
	nonce, ct, err := c.Seal("uat-repo", want)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, want) {
		t.Fatal("密文里能直接找到明文")
	}
	got, err := c.Open("uat-repo", nonce, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("往返之后内容变了")
	}
}

// **AAD 绑定 ref。** 把 A 仓库的密文行改挂到 B 仓库的 ref 上，必须解不开。
//
// 不绑的话，改一行的 ref 就能让 B 仓库拿到 A 的私钥；而那种失败会表现成
// "仓库不可达"，排查方向完全错。
func TestCiphertextIsBoundToItsRef(t *testing.T) {
	c := newCipher(t, kek(1))
	nonce, ct, err := c.Seal("repo-a", []byte("secret-a"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := c.Open("repo-b", nonce, ct); !errors.Is(err, dbsecrets.ErrUndecryptable) {
		t.Errorf("换个 ref 竟然解开了（err=%v）—— 密文没有绑定它属于谁", err)
	}
}

// 换一把 KEK 解不开，且报的是同一个错。
func TestAnotherKeyCannotOpenIt(t *testing.T) {
	nonce, ct, err := newCipher(t, kek(1)).Seal("repo-a", []byte("secret-a"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := newCipher(t, kek(2)).Open("repo-a", nonce, ct); !errors.Is(err, dbsecrets.ErrUndecryptable) {
		t.Errorf("另一把密钥解开了: %v", err)
	}
}

// 密文被改过要解不开 —— GCM 的完整性，这一条守的是"库被人改过会被发现"。
func TestTamperedCiphertextIsRejected(t *testing.T) {
	c := newCipher(t, kek(1))
	nonce, ct, err := c.Seal("repo-a", []byte("secret-a"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ct[0] ^= 0xff
	if _, err := c.Open("repo-a", nonce, ct); !errors.Is(err, dbsecrets.ErrUndecryptable) {
		t.Errorf("篡改过的密文被接受了: %v", err)
	}
}

// 每次加密用不同的 nonce：同一段明文两次落库，密文必须不同。
//
// nonce 重用在 GCM 下同时毁掉机密性与完整性，是这套加密最容易犯、
// 后果最严重的错。
func TestEachSealUsesAFreshNonce(t *testing.T) {
	c := newCipher(t, kek(1))
	n1, c1, _ := c.Seal("repo-a", []byte("same"))
	n2, c2, _ := c.Seal("repo-a", []byte("same"))
	if bytes.Equal(n1, n2) {
		t.Fatal("两次加密用了同一个 nonce")
	}
	if bytes.Equal(c1, c2) {
		t.Error("同一段明文两次加密得到同一段密文")
	}
}

// 密钥长度不对要拒，**不做拉伸**：从短口令派生 32 字节是可以做的，
// 但那让"密钥有多强"取决于调用方填了什么，而这里看不出来。
func TestKeyLengthIsEnforced(t *testing.T) {
	if _, err := dbsecrets.NewCipher(nil); !errors.Is(err, dbsecrets.ErrNoKEK) {
		t.Errorf("空密钥的错误 = %v, want ErrNoKEK", err)
	}
	for _, n := range []int{1, 16, 31, 33, 64} {
		if _, err := dbsecrets.NewCipher(bytes.Repeat([]byte{7}, n)); err == nil {
			t.Errorf("%d 字节的密钥被接受了", n)
		}
	}
}

// KeyID 稳定、区分不同密钥，且推不回 KEK。
func TestKeyIDIsStableAndOpaque(t *testing.T) {
	a1 := newCipher(t, kek(1)).KeyID()
	a2 := newCipher(t, kek(1)).KeyID()
	b := newCipher(t, kek(2)).KeyID()
	if a1 != a2 {
		t.Error("同一把密钥两次算出不同的 KeyID —— 轮换时认不出自己加的行")
	}
	if a1 == b {
		t.Error("两把不同的密钥算出同一个 KeyID")
	}
	if bytes.Contains([]byte(a1), kek(1)) {
		t.Error("KeyID 里能看到密钥本身")
	}
}

// 空凭据不许落库：一段空私钥加密之后仍然是一行合法记录，
// 而它的失败会推迟到写回那一刻，报成"仓库不可达"。
func TestEmptyCredentialIsRefused(t *testing.T) {
	c := newCipher(t, kek(1))
	if _, _, err := c.Seal("repo-a", nil); err == nil {
		t.Error("空凭据被接受了")
	}
	if _, _, err := c.Seal("", []byte("x")); err == nil {
		t.Error("空 ref 被接受了")
	}
}
