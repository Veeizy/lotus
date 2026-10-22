package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

/* ---------------- 身份：16 位密钥 → 两把密钥 ---------------- */

// Identity 由 16 位根密钥确定性推导的全部密钥材料。
type Identity struct {
	Secret string // 16 位密钥（只存本机）
	SeedX  []byte // X25519 私钥种子
	PubX   []byte // 明钥（X25519 公钥），随请求/证书对外
	SeedEd []byte // Ed25519 种子（= libp2p identity 种子）
	PubEd  []byte
	Priv   ed25519.PrivateKey
}

const (
	seedEdDomain = "picnet/v1/ed25519|"
	seedXDomain  = "picnet/v1/x25519|"
	keyChars     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// DeriveIdentity 由 16 位密钥推导身份（与 netcrypto.js deriveKeyPair 一致）。
func DeriveIdentity(secret string) (*Identity, error) {
	if len(secret) != 16 {
		return nil, errors.New("密钥必须为 16 位")
	}
	id := &Identity{Secret: secret}
	id.SeedEd = Sha256([]byte(seedEdDomain + secret))
	id.SeedX = Sha256([]byte(seedXDomain + secret))
	id.Priv = ed25519.NewKeyFromSeed(id.SeedEd)
	id.PubEd = id.Priv.Public().(ed25519.PublicKey)
	pub, err := X25519Public(id.SeedX)
	if err != nil {
		return nil, err
	}
	id.PubX = pub
	return id, nil
}

// RandomSecret16 生成 16 位密钥：62 字符表 + 拒绝采样（与旧实现一致）。
func RandomSecret16() (string, error) {
	out := make([]byte, 0, 16)
	for len(out) < 16 {
		buf := make([]byte, 20)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 248 {
				out = append(out, keyChars[int(b)%62])
				if len(out) == 16 {
					break
				}
			}
		}
	}
	return string(out), nil
}

/* ---------------- attestation（成员证书） ----------------
   {id(peerID), name, x(b64u pubX), addr, ts, sig}
   payload = "picnet/v1/att" ‖ 0x1f ‖ id ‖ 0x1f ‖ name ‖ 0x1f ‖ x ‖ 0x1f ‖ addr ‖ 0x1f ‖ ts
   sig = b64u(Ed25519_Sign(SHA256(payload)))                                  */

type Attestation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	X    string `json:"x"`
	Addr string `json:"addr"`
	TS   int64  `json:"ts"`
	Sig  string `json:"sig"`
}

const sep = "\x1f"

func attPayload(a *Attestation) []byte {
	return []byte(strings.Join([]string{
		"picnet/v1/att", a.ID, a.Name, a.X, a.Addr, fmt.Sprintf("%d", a.TS),
	}, sep))
}

// SignAttestation 以本机身份签署证书（peerID 为本节点的 libp2p PeerID）。
func (id *Identity) SignAttestation(peerID, name, addr string, ts int64) *Attestation {
	a := &Attestation{ID: peerID, Name: name, X: B64uEnc(id.PubX), Addr: addr, TS: ts}
	sig := ed25519.Sign(id.Priv, Sha256(attPayload(a)))
	a.Sig = B64uEnc(sig)
	return a
}

// VerifyAttestation 用 peerID 内嵌的 Ed25519 公钥（identity multihash）验证。
// idPub 由调用方从 libp2p peer.ID.ExtractPublicKey() 提取；为 nil 时仅验签名格式。
func VerifyAttestation(a *Attestation, idPub ed25519.PublicKey) bool {
	if idPub == nil {
		return false
	}
	sig, err := B64uDec(a.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(idPub, Sha256(attPayload(a)), sig)
}

/* ---------------- receipt（内容凭证） ----------------
   payload = "picnet/v1/receipt" ‖ 0x1f ‖ node ‖ 0x1f ‖ path ‖ 0x1f ‖ sha256 ‖ 0x1f ‖ ts */

type Receipt struct {
	Node   string `json:"node"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	TS     int64  `json:"ts"`
	Sig    string `json:"sig"`
}

func receiptPayload(r *Receipt) []byte {
	return []byte(strings.Join([]string{
		"picnet/v1/receipt", r.Node, r.Path, r.Sha256, fmt.Sprintf("%d", r.TS),
	}, sep))
}

// SignReceipt 由文件提供方签署「节点+路径+内容哈希」。
func (id *Identity) SignReceipt(nodeID, path, shaHex string, ts int64) *Receipt {
	r := &Receipt{Node: nodeID, Path: path, Sha256: shaHex, TS: ts}
	r.Sig = B64uEnc(ed25519.Sign(id.Priv, Sha256(receiptPayload(r))))
	return r
}

func VerifyReceipt(r *Receipt, nodePub ed25519.PublicKey) bool {
	if nodePub == nil {
		return false
	}
	sig, err := B64uDec(r.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(nodePub, Sha256(receiptPayload(r)), sig)
}

// AttestationJSON / ReceiptJSON 紧凑序列化（gossip 与缓存索引用）。
func AttestationJSON(a *Attestation) []byte {
	b, _ := json.Marshal(a)
	return b
}

func ParseAttestation(b []byte) (*Attestation, error) {
	var a Attestation
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

/* ---------------- 启动自检 ----------------
   RFC 7748 / RFC 8032 官方向量 + ECIES/签名往返与防篡改 + 流式 GCM 对拍。
   返回非 nil 表示失败——启动时失败则禁用全部网络功能。 */

func hexBytes(s string) []byte {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		out[i] = (hexVal(s[2*i]) << 4) | hexVal(s[2*i+1])
	}
	return out
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func SelfTest() error {
	// RFC 7748 §6.1 Diffie-Hellman 向量
	alicePriv := hexBytes("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	alicePub, err := X25519Public(alicePriv)
	if err != nil {
		return fmt.Errorf("x25519 公钥推导失败: %w", err)
	}
	if fmt.Sprintf("%x", alicePub) != "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a" {
		return errors.New("x25519 公钥推导失败")
	}
	shared, err := X25519Shared(alicePriv, hexBytes("de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"))
	if err != nil {
		return fmt.Errorf("x25519 共享密钥失败: %w", err)
	}
	if fmt.Sprintf("%x", shared) != "4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742" {
		return errors.New("x25519 共享密钥失败")
	}
	// RFC 8032 §7.1 TEST 1（Ed25519 空消息）
	edSeed := hexBytes("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	edPriv := ed25519.NewKeyFromSeed(edSeed)
	if fmt.Sprintf("%x", edPriv.Public().(ed25519.PublicKey)) != "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a" {
		return errors.New("ed25519 公钥推导失败")
	}
	sig := ed25519.Sign(edPriv, []byte{})
	if fmt.Sprintf("%x", sig) != "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b" {
		return errors.New("ed25519 签名向量失败")
	}
	if !ed25519.Verify(edPriv.Public().(ed25519.PublicKey), []byte{}, sig) {
		return errors.New("ed25519 验签失败")
	}

	// 身份推导一致性 + 签名往返
	a, err := DeriveIdentity("Ab3dEf6hIj9kLm2N")
	if err != nil {
		return err
	}
	b, err := DeriveIdentity("Zx9cVb7nMk3jQp1W")
	if err != nil {
		return err
	}
	msg := Sha256([]byte("auth-test"))
	if !ed25519.Verify(a.PubEd, msg, ed25519.Sign(a.Priv, msg)) {
		return errors.New("ed25519 身份签名往返失败")
	}
	if ed25519.Verify(b.PubEd, msg, ed25519.Sign(a.Priv, msg)) {
		return errors.New("ed25519 用别人的公钥竟然验签通过")
	}

	// ECIES 往返 + 错误密钥必须失败 + 篡改必须失败
	plain := []byte("不是所有的牛奶都叫特仑苏 🖼 test-image-bytes")
	blob, err := EncryptBytes(a.PubX, plain)
	if err != nil {
		return err
	}
	got, err := DecryptBytes(a.SeedX, blob)
	if err != nil || string(got) != string(plain) {
		return errors.New("ecies 解密失败")
	}
	blob2, _ := EncryptBytes(b.PubX, plain)
	if _, err := DecryptBytes(a.SeedX, blob2); err == nil {
		return errors.New("ecies 错误密钥竟然解开了")
	}
	tampered := append([]byte{}, blob...)
	tampered[ECIESHead+3] ^= 0xff
	if _, err := DecryptBytes(b.SeedX, tampered); err == nil {
		return errors.New("ecies 篡改密文竟然解开了")
	}

	// attestation / receipt 往返 + 防伪造
	att := a.SignAttestation("12D3KooWTestPeerID", "节点A", "/ip4/192.168.1.1/udp/3457/quic-v1", 1700000000000)
	if !VerifyAttestation(att, a.PubEd) {
		return errors.New("attestation 验签失败")
	}
	if VerifyAttestation(att, b.PubEd) {
		return errors.New("attestation 用别人的公钥竟然验签通过")
	}
	attBad := *att
	attBad.Name = "冒名"
	if VerifyAttestation(&attBad, a.PubEd) {
		return errors.New("attestation 篡改字段竟然验签通过")
	}
	rc := a.SignReceipt("peer-a", "D:\\pic\\1.png", "abc", 1700000000001)
	if !VerifyReceipt(rc, a.PubEd) {
		return errors.New("receipt 验签失败")
	}

	// 流式 GCM 与 stdlib 对拍（多种长度，含非 16 倍数边界）
	sizes := []int{0, 1, 15, 16, 17, 31, 32, 33, 255, 4096, 1<<20 + 13}
	key := Sha256([]byte("gcm-stream-test-key"))
	nonce := Sha256([]byte("nonce"))[:12]
	for _, sz := range sizes {
		pt := make([]byte, sz)
		if _, err := rand.Read(pt); err != nil {
			return err
		}
		want, err := sealGCM(key, nonce, pt)
		if err != nil {
			return err
		}
		s2, err := NewSealer(key, nonce)
		if err != nil {
			return err
		}
		ct := make([]byte, sz)
		copy(ct, pt)
		// 分块写入（模拟流式）
		for off := 0; off < sz; off += 1000 {
			end := off + 1000
			if end > sz {
				end = sz
			}
			s2.Write(ct[off:end])
		}
		tag := s2.Tag()
		if string(tag[:]) != string(want[sz:]) {
			return fmt.Errorf("流式 GCM 密文/标签与 stdlib 不一致 (len=%d)", sz)
		}
		if string(ct) != string(want[:sz]) {
			return fmt.Errorf("流式 GCM 密文与 stdlib 不一致 (len=%d)", sz)
		}
		// 解密对拍
		o2, err := NewOpener(key, nonce)
		if err != nil {
			return err
		}
		pt2 := make([]byte, sz)
		copy(pt2, ct)
		for off := 0; off < sz; off += 777 {
			end := off + 777
			if end > sz {
				end = sz
			}
			o2.Write(pt2[off:end])
		}
		if err := o2.Verify(want[sz:]); err != nil {
			return fmt.Errorf("流式 GCM 解密校验失败 (len=%d): %w", sz, err)
		}
		if string(pt2) != string(pt) {
			return fmt.Errorf("流式 GCM 解密明文不一致 (len=%d)", sz)
		}
	}

	// EncryptReader / DecryptStream 往返（临时文件路径）
	big := make([]byte, 3<<20) // 3MB，跨越多个 512KB 块
	if _, err := rand.Read(big); err != nil {
		return err
	}
	tmpNew := func() (*os.File, error) { return os.CreateTemp("", "picnet-ecies-*.tmp") }
	hdr, tmp, ctLen, err := EncryptReader(a.PubX, bytes.NewReader(big), tmpNew)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if ctLen != int64(len(big)) {
		return errors.New("ecies 流式密文长度不符")
	}
	var out bytes.Buffer
	shaHex, err := DecryptStream(a.SeedX, hdr, tmp, &out)
	if err != nil {
		return fmt.Errorf("ecies 流式解密失败: %w", err)
	}
	if !bytes.Equal(out.Bytes(), big) {
		return errors.New("ecies 流式解密明文不一致")
	}
	if shaHex != fmt.Sprintf("%x", Sha256(big)) {
		return errors.New("ecies 流式 sha256 不符")
	}
	return nil
}

/* ---------------- leave notice（优雅下线通知，WP 增补） ----------------
   {"kind":"leave","id","ts","sig"}
   payload = "picnet/v1/leave" ‖ 0x1f ‖ id ‖ 0x1f ‖ ts
   sig = b64u(Ed25519_Sign(SHA256(payload)))。接收方验签后立即移除该成员——
   只有持有身份私钥的节点本人能宣告自己下线，无法伪造他人下线。 */

type LeaveNotice struct {
	Kind string `json:"kind"` // 恒为 "leave"
	ID   string `json:"id"`
	TS   int64  `json:"ts"`
	Sig  string `json:"sig"`
}

func leavePayload(id string, ts int64) []byte {
	return []byte(strings.Join([]string{"picnet/v1/leave", id, fmt.Sprintf("%d", ts)}, sep))
}

// SignLeaveNotice 本机签署自己的下线通知。
func (id *Identity) SignLeaveNotice(peerID string, ts int64) *LeaveNotice {
	n := &LeaveNotice{Kind: "leave", ID: peerID, TS: ts}
	n.Sig = B64uEnc(ed25519.Sign(id.Priv, Sha256(leavePayload(peerID, ts))))
	return n
}

func VerifyLeaveNotice(n *LeaveNotice, pub ed25519.PublicKey) bool {
	if n == nil || n.Kind != "leave" || pub == nil {
		return false
	}
	sig, err := B64uDec(n.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, Sha256(leavePayload(n.ID, n.TS)), sig)
}
