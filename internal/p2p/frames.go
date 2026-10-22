package p2p

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// 帧格式（PROTOCOL §3.1）：u32 大端长度前缀 + JSON。image 应答的 JSON 头之后
// 跟随密文体，密文体不经本函数（由调用方直接流式读写到 EOF）。

const MaxMsgSize = 1 << 20 // 节点流 JSON 头上限

// ChunkDelay 流控模拟钩子（--net-delay 测试开关）：每写/读 chunk 字节后休眠。
// nil = 不模拟。
type ChunkDelay func(chunkBytes int)

func WriteMsg(w io.Writer, data []byte, delay ChunkDelay) error {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	if delay != nil {
		delay(4)
	}
	n, err := w.Write(data)
	if delay != nil {
		delay(n)
	}
	return err
}

func ReadMsg(r io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = MaxMsgSize
	}
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > uint32(max) {
		return nil, fmt.Errorf("消息超长: %d > %d", n, max)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// NewChunkDelay 按「每 32KB 休眠 delayMS」构造钩子；open 时额外休眠一次。
func NewChunkDelay(delayMS int) ChunkDelay {
	if delayMS <= 0 {
		return nil
	}
	d := time.Duration(delayMS) * time.Millisecond
	return func(n int) {
		steps := 1 + n/(32*1024)
		time.Sleep(d * time.Duration(steps))
	}
}
