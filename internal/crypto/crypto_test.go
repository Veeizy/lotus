package crypto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSelfTest(t *testing.T) {
	if err := SelfTest(); err != nil {
		t.Fatalf("自检失败: %v", err)
	}
}

func TestRandomSecret16(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := RandomSecret16()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 16 {
			t.Fatalf("长度错误: %q", s)
		}
		for _, c := range s {
			if !strings.ContainsRune(keyChars, c) {
				t.Fatalf("非法字符: %q", s)
			}
		}
		if seen[s] {
			t.Fatalf("重复密钥: %q", s)
		}
		seen[s] = true
	}
}

// TestCrossVectorNode 用 Node 版 netcrypto.js 生成密文，Go 解密；Go 加密，Node 解密。
// 证明两个实现逐字节互通。无 node 环境时跳过。
func TestCrossVectorNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机无 node，跳过跨实现向量")
	}
	script := `
const nc = require(%q);
const seed = Buffer.from('a'.repeat(32));
const peer = nc.deriveKeyPair('Ab3dEf6hIj9kLm2N');
const plain = Buffer.from('cross-vector-你好-123', 'utf8');
// JS 加密给 Go 的身份（Go 的 pubX 由命令行传入）
const blobJs = nc.eciesEncrypt(Buffer.from(process.argv[1], 'base64url'), plain);
// Go 加密给 JS 的身份（固定种子），JS 解密
const goBlob = Buffer.from(process.argv[2], 'base64url');
let goPlain = null, goErr = null;
try { goPlain = nc.eciesDecrypt(seed, goBlob).toString('utf8'); } catch (e) { goErr = e.message; }
console.log(JSON.stringify({ jsBlob: blobJs.toString('base64url'), goPlain, goErr }));
`
	a, _ := DeriveIdentity("Ab3dEf6hIj9kLm2N")
	// Go 固定种子 'a'*32 推导 X25519 公钥
	goSeed := bytes.Repeat([]byte{'a'}, 32)
	goPub, err := X25519Public(goSeed)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("go-to-js-cross-你好-456")
	goBlob, err := EncryptBytes(goPub, plain)
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(script, strings.ReplaceAll(`D:\work_space\vscode\vibe\pic\netcrypto.js`, `\`, `\\`))
	cmd := exec.Command(node, "-e", src, B64uEnc(a.PubX), B64uEnc(goBlob))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行失败: %v\n%s", err, out)
	}
	var res struct {
		JsBlob string `json:"jsBlob"`
		GoPlain string `json:"goPlain"`
		GoErr  string `json:"goErr"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("解析 node 输出失败: %v\n%s", err, out)
	}
	if res.GoErr != "" || res.GoPlain != string(plain) {
		t.Fatalf("Go→Node 解密失败: err=%q plain=%q", res.GoErr, res.GoPlain)
	}
	blob, err := B64uDec(res.JsBlob)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptBytes(a.SeedX, blob)
	if err != nil {
		t.Fatalf("Node→Go 解密失败: %v", err)
	}
	if string(got) != "cross-vector-你好-123" {
		t.Fatalf("Node→Go 明文不符: %q", got)
	}
	t.Log("Node↔Go ECIES 互通验证通过")
}

func TestB64uRoundtrip(t *testing.T) {
	for _, n := range []int{0, 1, 31, 32, 33, 64} {
		b := make([]byte, n)
		hex.Decode(b, []byte(fmt.Sprintf("%0*d", n*2, 0)))
		enc := B64uEnc(b)
		dec, err := B64uDec(enc)
		if err != nil || !bytes.Equal(dec, b) {
			t.Fatalf("b64u 往返失败 len=%d", n)
		}
		if strings.ContainsAny(enc, "+/=") {
			t.Fatalf("b64u 含填充或标准字符: %q", enc)
		}
	}
}

func TestDecryptStreamTamper(t *testing.T) {
	a, _ := DeriveIdentity("Ab3dEf6hIj9kLm2N")
	plain := bytes.Repeat([]byte("tamper-me"), 100000)
	tmp, err := os.CreateTemp("", "t-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hdr, tmp2, _, err := EncryptReader(a.PubX, bytes.NewReader(plain), func() (*os.File, error) { return tmp, nil })
	if err != nil {
		t.Fatal(err)
	}
	// 篡改密文中部
	hdrB := append([]byte{}, hdr...)
	// 读出密文并翻转一字节
	ct, _ := io_ReadAll(tmp2)
	ct[12345] ^= 0x01
	r := bytes.NewReader(ct)
	var out bytes.Buffer
	_, err = DecryptStream(a.SeedX, hdrB, r, &out)
	if err == nil {
		t.Fatal("篡改密文竟然解密成功")
	}
}

// io_ReadAll 小包装避免单独 import io
func io_ReadAll(f *os.File) ([]byte, error) {
	var out bytes.Buffer
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	_, err := out.ReadFrom(f)
	return out.Bytes(), err
}
