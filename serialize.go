package bitmap

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
)

// 确定性二进制格式（全部小端）：
//
//	4 字节  魔数 "ABM1"
//	4 字节  uint32 桶数量 N
//	随后 N 个桶，桶按高位键严格递增：
//	  2 字节  uint16 高位键
//	  4 字节  uint32 桶内基数 card
//	  1 字节  容器类型：0=数组，1=位图
//	  载荷    数组：card 个 uint16（严格递增，共 card*2 字节）
//	          位图：1024 个 uint64（8192 字节）
//
// 载荷字节统计固定为 数组元素数×2 或 位图 8192 字节，与 Go 进程内存无关。
// Load 在分配任何载荷缓冲前，先按 maxPayloadBytes 校验累计载荷。

var magic = [4]byte{'A', 'B', 'M', '1'}

// Save 把集合按确定性格式写入 w。
func (b *Bitmap) Save(w io.Writer) error {
	hdr := make([]byte, 8)
	copy(hdr, magic[:])
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(b.buckets)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	var bh [7]byte
	for i := range b.buckets {
		bk := &b.buckets[i]
		binary.LittleEndian.PutUint16(bh[0:2], bk.key)
		binary.LittleEndian.PutUint32(bh[2:6], uint32(bk.c.card))
		bh[6] = byte(bk.c.kind)
		if _, err := w.Write(bh[:]); err != nil {
			return err
		}
		if bk.c.kind == kindArray {
			var buf [2]byte
			for _, v := range bk.c.arr {
				binary.LittleEndian.PutUint16(buf[:], v)
				if _, err := w.Write(buf[:]); err != nil {
					return err
				}
			}
		} else {
			var buf [8]byte
			for _, w64 := range bk.c.bm {
				binary.LittleEndian.PutUint64(buf[:], w64)
				if _, err := w.Write(buf[:]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Load 从 r 读取集合。maxPayloadBytes 是调用者给出的载荷字节上限；
// 每个桶的载荷在分配前计入累计，超限即拒绝。非规范输入一律报错：
// 重复/乱序桶、重复数组值、错误基数、截断、非规范容器。
func Load(r io.Reader, maxPayloadBytes uint64) (*Bitmap, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("bitmap: load header: %w", errTrunc(err))
	}
	if string(hdr[0:4]) != string(magic[:]) {
		return nil, fmt.Errorf("bitmap: bad magic %q", hdr[0:4])
	}
	n := binary.LittleEndian.Uint32(hdr[4:])
	// 每个桶至少 2 字节载荷，先挡住超大桶数导致的切片分配。
	if uint64(n)*2 > maxPayloadBytes {
		return nil, fmt.Errorf("bitmap: %d buckets exceed payload limit %d", n, maxPayloadBytes)
	}
	out := &Bitmap{}
	var total uint64
	var prevKey uint16
	var bh [7]byte
	for i := uint32(0); i < n; i++ {
		if _, err := io.ReadFull(r, bh[:]); err != nil {
			return nil, fmt.Errorf("bitmap: bucket %d header: %w", i, errTrunc(err))
		}
		key := binary.LittleEndian.Uint16(bh[0:2])
		card := binary.LittleEndian.Uint32(bh[2:6])
		kind := containerKind(bh[6])
		if i > 0 && key <= prevKey {
			return nil, fmt.Errorf("bitmap: bucket %d key %d not strictly increasing", i, key)
		}
		prevKey = key
		var payload uint64
		switch kind {
		case kindArray:
			if card == 0 || card > arrayMaxCardinality {
				return nil, fmt.Errorf("bitmap: bucket %d non-canonical array cardinality %d", i, card)
			}
			payload = uint64(card) * 2
		case kindBitmap:
			if card <= arrayMaxCardinality || card > 65536 {
				return nil, fmt.Errorf("bitmap: bucket %d non-canonical bitmap cardinality %d", i, card)
			}
			payload = bitmapPayloadBytes
		default:
			return nil, fmt.Errorf("bitmap: bucket %d unknown container kind %d", i, bh[6])
		}
		if total+payload > maxPayloadBytes {
			return nil, fmt.Errorf("bitmap: payload %d exceeds limit %d", total+payload, maxPayloadBytes)
		}
		total += payload
		var c *container
		if kind == kindArray {
			arr := make([]uint16, card)
			var buf [2]byte
			for j := range arr {
				if _, err := io.ReadFull(r, buf[:]); err != nil {
					return nil, fmt.Errorf("bitmap: bucket %d array: %w", i, errTrunc(err))
				}
				arr[j] = binary.LittleEndian.Uint16(buf[:])
				if j > 0 && arr[j] <= arr[j-1] {
					return nil, fmt.Errorf("bitmap: bucket %d array value %d not strictly increasing", i, arr[j])
				}
			}
			c = newArrayContainer(arr)
		} else {
			bm := &[bitmapWords]uint64{}
			var buf [8]byte
			var got uint32
			for j := range bm {
				if _, err := io.ReadFull(r, buf[:]); err != nil {
					return nil, fmt.Errorf("bitmap: bucket %d bitmap: %w", i, errTrunc(err))
				}
				bm[j] = binary.LittleEndian.Uint64(buf[:])
				got += uint32(bits.OnesCount64(bm[j]))
			}
			if got != card {
				return nil, fmt.Errorf("bitmap: bucket %d bitmap cardinality %d, header says %d", i, got, card)
			}
			c = &container{kind: kindBitmap, card: int(card), bm: bm}
		}
		out.buckets = append(out.buckets, bucket{key: key, c: c})
		out.card += uint64(card)
	}
	return out, nil
}

func errTrunc(err error) error {
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return fmt.Errorf("truncated input (%v)", err)
	}
	return err
}
