package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/hkdf"
)

/* ---------------- base64url（无填充） ---------------- */

func B64uEnc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func B64uDec(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func Sha256(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

/* ---------------- X25519（crypto/ecdh，RFC 7748） ---------------- */

var x25519Curve = ecdh.X25519()

// X25519Public 由 32 字节私钥种子推导公钥。
func X25519Public(priv32 []byte) ([]byte, error) {
	k, err := x25519Curve.NewPrivateKey(priv32)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}

// X25519Shared 计算共享密钥；全零共享密钥（非法公钥）返回错误。
func X25519Shared(priv32, peerPub32 []byte) ([]byte, error) {
	k, err := x25519Curve.NewPrivateKey(priv32)
	if err != nil {
		return nil, fmt.Errorf("x25519 私钥非法: %w", err)
	}
	pub, err := x25519Curve.NewPublicKey(peerPub32)
	if err != nil {
		return nil, fmt.Errorf("x25519 公钥非法: %w", err)
	}
	s, err := k.ECDH(pub)
	if err != nil {
		return nil, err
	}
	allZero := true
	for _, b := range s {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, errors.New("非法公钥（全零共享密钥）")
	}
	return s, nil
}

/* ---------------- HKDF-SHA256（RFC 5869） ---------------- */

func HKDFSha256(ikm, salt, info []byte, length int) ([]byte, error) {
	r := hkdf.New(sha256.New, ikm, salt, info)
	out := make([]byte, length)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}

/* ---------------- ECIES（PNX1 布局，与 netcrypto.js 逐字节兼容） ----------------
   密文：'PNX1'(4) ‖ salt(16) ‖ ephPub(32) ‖ nonce(12) ‖ tag(16) ‖ ciphertext
   key  = HKDF-SHA256(X25519(eph, peerPub), salt, "picnet/ecies-v1", 32)
------------------------------------------------------------------ */

var eciesMagic = []byte("PNX1")

const (
	ECIESHead = 4 + 16 + 32 + 12 + 16 // 80
)

var ErrECIESFormat = errors.New("密文格式错误")

func eciesKey(privSeed32, salt, ephPub []byte) ([]byte, error) {
	shared, err := X25519Shared(privSeed32, ephPub)
	if err != nil {
		return nil, err
	}
	return HKDFSha256(shared, salt, []byte("picnet/ecies-v1"), 32)
}

// EncryptBytes 整段加密（测试与小负载用）。输出 = 头(80B) + 密文，与 JS 互通。
func EncryptBytes(pubX, plain []byte) ([]byte, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	ephPriv := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	if _, err := rand.Read(ephPriv); err != nil {
		return nil, err
	}
	ephPub, err := X25519Public(ephPriv)
	if err != nil {
		return nil, err
	}
	key, err := eciesKey(ephPriv, salt, pubX)
	if err != nil {
		return nil, err
	}
	s, err := NewSealer(key, nonce)
	if err != nil {
		return nil, err
	}
	ct := make([]byte, len(plain))
	copy(ct, plain)
	s.Write(ct)
	tag := s.Tag()
	out := make([]byte, 0, ECIESHead+len(plain))
	out = append(out, eciesMagic...)
	out = append(out, salt...)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = append(out, tag[:]...)
	out = append(out, ct...)
	return out, nil
}

// DecryptBytes 解密整段（格式/密钥/篡改错误在此返回）。
func DecryptBytes(privSeed32, blob []byte) ([]byte, error) {
	if len(blob) < ECIESHead || string(blob[:4]) != string(eciesMagic) {
		return nil, ErrECIESFormat
	}
	off := 4
	salt := blob[off : off+16]
	off += 16
	ephPub := blob[off : off+32]
	off += 32
	nonce := blob[off : off+12]
	off += 12
	tag := blob[off : off+16]
	off += 16
	ct := blob[off:]
	key, err := eciesKey(privSeed32, salt, ephPub)
	if err != nil {
		return nil, err
	}
	o, err := NewOpener(key, nonce)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	copy(pt, ct)
	o.Write(pt)
	if err := o.Verify(tag); err != nil {
		return nil, err
	}
	return pt, nil
}

// EncryptReader 流式加密：先把密文写入 tmp（调用方提供的临时文件工厂），完成后返回
// 80 字节头（含标签）与已回卷到起始位置的密文临时文件。调用方负责关闭并删除 tmp。
// 发送方随后将 hdr + tmp 依次写出到流——全程明文/密文均不整段驻留内存。
func EncryptReader(pubX []byte, src io.Reader, tmpNew func() (*os.File, error)) ([]byte, *os.File, int64, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	ephPriv := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, 0, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, err
	}
	if _, err := rand.Read(ephPriv); err != nil {
		return nil, nil, 0, err
	}
	ephPub, err := X25519Public(ephPriv)
	if err != nil {
		return nil, nil, 0, err
	}
	key, err := eciesKey(ephPriv, salt, pubX)
	if err != nil {
		return nil, nil, 0, err
	}
	s, err := NewSealer(key, nonce)
	if err != nil {
		return nil, nil, 0, err
	}
	tmp, err := tmpNew()
	if err != nil {
		return nil, nil, 0, err
	}
	// 双缓冲交替：Write 就地把明文变成密文，读下一块用另一个缓冲
	bufs := [2][]byte{make([]byte, 512*1024), make([]byte, 512*1024)}
	i := 0
	var ctLen int64
	for {
		n, rerr := src.Read(bufs[i])
		if n > 0 {
			s.Write(bufs[i][:n])
			if _, werr := tmp.Write(bufs[i][:n]); werr != nil {
				tmp.Close()
				return nil, nil, 0, werr
			}
			ctLen += int64(n)
			i = 1 - i
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return nil, nil, 0, rerr
		}
	}
	tag := s.Tag()
	hdr := make([]byte, 0, ECIESHead)
	hdr = append(hdr, eciesMagic...)
	hdr = append(hdr, salt...)
	hdr = append(hdr, ephPub...)
	hdr = append(hdr, nonce...)
	hdr = append(hdr, tag[:]...)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return nil, nil, 0, err
	}
	return hdr, tmp, ctLen, nil
}

// ParseHeader 解析 80 字节密文头，返回 salt/ephPub/nonce/tag（不派生密钥）。
func ParseHeader(hdr []byte) (salt, ephPub, nonce, tag []byte, err error) {
	if len(hdr) != ECIESHead || string(hdr[:4]) != string(eciesMagic) {
		return nil, nil, nil, nil, ErrECIESFormat
	}
	return hdr[4:20], hdr[20:52], hdr[52:64], hdr[64:80], nil
}

// DecryptStream 解密密文体（r 从头之后开始），明文写入 w 并顺带计算 sha256（hex）。
// 流结束后校验标签——错误时已写出的明文必须视为不可信。
// privSeed32 为接收方 X25519 种子。
func DecryptStream(privSeed32 []byte, hdr []byte, r io.Reader, w io.Writer) (shaHex string, err error) {
	salt, ephPub, nonce, tag, err := ParseHeader(hdr)
	if err != nil {
		return "", err
	}
	// 解密密钥 = HKDF(X25519(我的私钥, ephPub))
	if len(privSeed32) != 32 {
		return "", errors.New("x25519 种子长度错误")
	}
	shared, err := X25519Shared(privSeed32, ephPub)
	if err != nil {
		return "", err
	}
	key, err := HKDFSha256(shared, salt, []byte("picnet/ecies-v1"), 32)
	if err != nil {
		return "", err
	}
	o, err := NewOpener(key, nonce)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 512*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			o.Write(buf[:n]) // 就地解密
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", werr
			}
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if err := o.Verify(tag); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}


