// Package crypto: picnet 的密码学层（对应旧 netcrypto.js，纯 stdlib + x/crypto/hkdf）。
//
// 本文件实现「流式 AES-256-GCM」：ECIES 的 PNX1 布局把认证标签放在密文【前】，
// 单次 Seal 需要整段明文常驻内存。为满足 MIGRATION §6 的内存要求（图片传输禁止
// 整文件进内存），这里按 NIST SP 800-38D 手工实现增量 GCM：
//
//	C = GCTR(inc32(J0), P)；S = GHASH_H(C || pad || lenblock)；T = AES(K, J0) ⊕ S
//
// 输出与 stdlib cipher.NewGCM 的单次 Seal 逐字节一致（有对拍测试保证）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

const gcmBlockSize = 16

var errGCMAuth = errors.New("gcm: 认证标签不匹配")

// gcmMul 计算 GF(2^128) 上的 x·h（GCM 位序：首字节最高位为 X^0 系数）。
func gcmMul(x, h *[16]byte) [16]byte {
	var z [16]byte
	v := *h
	for i := 0; i < 128; i++ {
		if x[i/8]&(0x80>>(uint(i)%8)) != 0 {
			for j := 0; j < 16; j++ {
				z[j] ^= v[j]
			}
		}
		lsb := v[15] & 1
		for j := 15; j > 0; j-- {
			v[j] = (v[j] >> 1) | (v[j-1] << 7)
		}
		v[0] >>= 1
		if lsb == 1 {
			v[0] ^= 0xe1
		}
	}
	return z
}

// ctrStream 是以 32 位大端计数器递增的 AES-CTR（GCM 的 GCTR）。
type ctrStream struct {
	b   cipher.Block
	ctr [16]byte // 下一个要加密的计数器块
	ks  [16]byte // 当前密钥流块
	n   int      // ks 中已消耗的字节
}

func inc32(b *[16]byte) {
	for i := 15; i >= 12; i-- {
		b[i]++
		if b[i] != 0 {
			break
		}
	}
}

func newCTR(b cipher.Block, j0 [16]byte) *ctrStream {
	s := &ctrStream{b: b, ctr: j0}
	inc32(&s.ctr) // 首个密钥流块 = AES(J0+1)
	s.refill()    // 立即填充，避免首块密钥流为空
	return s
}

func (s *ctrStream) refill() {
	s.b.Encrypt(s.ks[:], s.ctr[:])
	inc32(&s.ctr)
	s.n = 0
}

// xor 就地异或并推进密钥流。
func (s *ctrStream) xor(p []byte) {
	for i := range p {
		if s.n == gcmBlockSize {
			s.refill()
		}
		p[i] ^= s.ks[s.n]
		s.n++
	}
}

// gcmState 是增量 GCM 的公共部分：GHASH 累加 + 计数器。
type gcmState struct {
	ctr   *ctrStream
	h     [16]byte
	y     [16]byte // GHASH 累加值（未对齐的尾块暂存 buf）
	buf   [16]byte
	buf_n int
	// j0e = AES(K, J0)，标签 = j0e ⊕ GHASH 终值
	j0e  [16]byte
	ctLn uint64 // 已处理密文字节数
}

func newGCMState(key, nonce []byte) (*gcmState, error) {
	if len(nonce) != 12 {
		return nil, errors.New("gcm: nonce 必须为 12 字节")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var j0 [16]byte
	copy(j0[:], nonce)
	j0[15] = 1
	g := &gcmState{ctr: newCTR(b, j0)}
	var zero [16]byte
	b.Encrypt(g.h[:], zero[:])            // H = AES(K, 0^128)
	b.Encrypt(g.j0e[:], j0[:])            // E(K, J0)
	return g, nil
}

func (g *gcmState) absorbBlock(b *[16]byte) {
	for i := 0; i < 16; i++ {
		g.y[i] ^= b[i]
	}
	g.y = gcmMul(&g.y, &g.h)
}

// absorb 密文块（不完整尾块留在 buf）。
func (g *gcmState) absorb(ct []byte) {
	for len(ct) > 0 {
		n := copy(g.buf[g.buf_n:], ct)
		g.buf_n += n
		ct = ct[n:]
		g.ctLn += uint64(n)
		if g.buf_n == gcmBlockSize {
			g.absorbBlock(&g.buf)
			g.buf_n = 0
		}
	}
}

// finish 补齐尾块、吸收长度块，返回最终认证标签。
func (g *gcmState) finish() [16]byte {
	if g.buf_n > 0 {
		for i := g.buf_n; i < 16; i++ {
			g.buf[i] = 0
		}
		g.absorbBlock(&g.buf)
		g.buf_n = 0
	}
	var lb [16]byte
	binary.BigEndian.PutUint64(lb[8:], g.ctLn*8)
	g.absorbBlock(&lb)
	var tag [16]byte
	for i := 0; i < 16; i++ {
		tag[i] = g.y[i] ^ g.j0e[i]
	}
	return tag
}

// Sealer 流式加密：调用方按任意大小分块写入，最后取 Tag。
type Sealer struct {
	g *gcmState
}

func NewSealer(key, nonce []byte) (*Sealer, error) {
	g, err := newGCMState(key, nonce)
	if err != nil {
		return nil, err
	}
	return &Sealer{g: g}, nil
}

// Write 加密 p（结果写入 p 本身——调用方需先复制明文），并吸收密文进 GHASH。
func (s *Sealer) Write(p []byte) {
	s.g.ctr.xor(p)
	s.g.absorb(p)
}

func (s *Sealer) Tag() [16]byte {
	return s.g.finish()
}

// Opener 流式解密：逐块 Write（结果写入 p 本身），最后 Verify。
type Opener struct {
	g *gcmState
}

func NewOpener(key, nonce []byte) (*Opener, error) {
	g, err := newGCMState(key, nonce)
	if err != nil {
		return nil, err
	}
	return &Opener{g: g}, nil
}

func (o *Opener) Write(c []byte) {
	o.g.absorb(c) // 先吸收密文（GHASH 以密文为输入）
	o.g.ctr.xor(c)
}

// Verify 校验标签；失败时已输出的明文必须视为不可信并丢弃。
func (o *Opener) Verify(tag []byte) error {
	t := o.g.finish()
	if subtle.ConstantTimeCompare(t[:], tag) != 1 {
		return errGCMAuth
	}
	return nil
}

// sealGCM 等价于 cipher.NewGCM(...).Seal，供测试对拍（不对外使用）。
func sealGCM(key, nonce, plain []byte) ([]byte, error) {
	a, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(a)
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, nonce, plain, nil), nil
}
