package bitmap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Deterministic binary format (all integers little-endian):
//
//	offset  size  field
//	0       4     magic "ABM1"
//	4       4     uint32 bucket count N
//	then N bucket records, in strictly increasing high-16-bit key order:
//	      2     uint16 bucket key (high 16 bits)
//	      1     uint8  container type: 0 = array, 1 = bitmap
//	      1     uint8  reserved, must be 0
//	      4     uint32 container cardinality
//	      ...   payload: array  -> cardinality * uint16 values, strictly increasing
//	                     bitmap -> 1024 * uint64 words (8192 bytes)
//
// Canonical form is required: array iff 1 <= cardinality <= 4096, bitmap iff
// cardinality > 4096; empty buckets are never stored. The same set always
// serializes to the same byte sequence.
var magic = [4]byte{'A', 'B', 'M', '1'}

const (
	headerSize       = 8
	bucketHeaderSize = 8
	containerArray   = 0
	containerBitmap  = 1
)

// ErrPayloadLimit is returned by Unmarshal when the declared container
// payloads would exceed the caller-supplied byte limit.
var ErrPayloadLimit = errors.New("bitmap: payload exceeds caller limit")

// MarshalBinary returns the deterministic binary encoding of the set.
func (s *Set) MarshalBinary() []byte {
	out := make([]byte, 0, headerSize+len(s.buckets)*bucketHeaderSize+int(s.PayloadBytes()))
	out = append(out, magic[:]...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(s.buckets)))
	for i := range s.buckets {
		b := &s.buckets[i]
		out = binary.LittleEndian.AppendUint16(out, b.key)
		if b.c.isBitmap {
			out = append(out, containerBitmap, 0)
		} else {
			out = append(out, containerArray, 0)
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(b.c.card))
		if b.c.isBitmap {
			for _, w := range b.c.bits {
				out = binary.LittleEndian.AppendUint64(out, w)
			}
		} else {
			for _, v := range b.c.arr {
				out = binary.LittleEndian.AppendUint16(out, v)
			}
		}
	}
	return out
}

// Unmarshal decodes data produced by MarshalBinary. Before allocating each
// container payload it checks the running payload total (array elements * 2
// bytes, or 8192 bytes per bitmap) against maxPayloadBytes and fails with
// ErrPayloadLimit instead of allocating. Malformed input — truncation,
// trailing bytes, duplicate or out-of-order buckets, duplicate array values,
// wrong cardinalities, non-canonical containers — is rejected.
func Unmarshal(data []byte, maxPayloadBytes uint64) (*Set, error) {
	r := &reader{data: data}
	head, err := r.take(headerSize)
	if err != nil {
		return nil, fmt.Errorf("bitmap: truncated header: %w", err)
	}
	if string(head[:4]) != string(magic[:]) {
		return nil, errors.New("bitmap: bad magic")
	}
	n := binary.LittleEndian.Uint32(head[4:])

	s := &Set{}
	var payload uint64
	var prevKey uint16
	for i := uint32(0); i < n; i++ {
		bh, err := r.take(bucketHeaderSize)
		if err != nil {
			return nil, fmt.Errorf("bitmap: truncated bucket header %d: %w", i, err)
		}
		key := binary.LittleEndian.Uint16(bh[0:2])
		typ := bh[2]
		if bh[3] != 0 {
			return nil, fmt.Errorf("bitmap: bucket %d: reserved byte must be zero", i)
		}
		card := binary.LittleEndian.Uint32(bh[4:8])
		if i > 0 && key <= prevKey {
			return nil, fmt.Errorf("bitmap: bucket %d: keys not strictly increasing", i)
		}
		prevKey = key

		var c *container
		switch typ {
		case containerArray:
			if card == 0 || card > ArrayMaxCardinality {
				return nil, fmt.Errorf("bitmap: bucket %d: non-canonical array cardinality %d", i, card)
			}
			need := uint64(card) * 2
			if payload+need > maxPayloadBytes {
				return nil, fmt.Errorf("%w: bucket %d needs %d more bytes (limit %d)", ErrPayloadLimit, i, need, maxPayloadBytes)
			}
			payload += need
			raw, err := r.take(int(need))
			if err != nil {
				return nil, fmt.Errorf("bitmap: truncated array payload in bucket %d: %w", i, err)
			}
			arr := make([]uint16, card)
			for j := range arr {
				arr[j] = binary.LittleEndian.Uint16(raw[j*2:])
				if j > 0 && arr[j] <= arr[j-1] {
					return nil, fmt.Errorf("bitmap: bucket %d: array values not strictly increasing", i)
				}
			}
			c = newArrayContainer(arr)
		case containerBitmap:
			if card <= ArrayMaxCardinality || card > bucketCapacity {
				return nil, fmt.Errorf("bitmap: bucket %d: non-canonical bitmap cardinality %d", i, card)
			}
			if payload+BitmapBytes > maxPayloadBytes {
				return nil, fmt.Errorf("%w: bucket %d needs %d more bytes (limit %d)", ErrPayloadLimit, i, BitmapBytes, maxPayloadBytes)
			}
			payload += BitmapBytes
			raw, err := r.take(BitmapBytes)
			if err != nil {
				return nil, fmt.Errorf("bitmap: truncated bitmap payload in bucket %d: %w", i, err)
			}
			n := newBitmapContainer()
			total := 0
			for w := 0; w < bitmapWords; w++ {
				n.bits[w] = binary.LittleEndian.Uint64(raw[w*8:])
				total += bits.OnesCount64(n.bits[w])
			}
			if total != int(card) {
				return nil, fmt.Errorf("bitmap: bucket %d: bitmap popcount %d != cardinality %d", i, total, card)
			}
			n.card = int(card)
			c = n
		default:
			return nil, fmt.Errorf("bitmap: bucket %d: unknown container type %d", i, typ)
		}
		s.buckets = append(s.buckets, bucket{key: key, c: c})
		s.card += uint64(c.card)
	}
	if r.off != len(data) {
		return nil, fmt.Errorf("bitmap: %d trailing bytes after %d buckets", len(data)-r.off, n)
	}
	return s, nil
}

type reader struct {
	data []byte
	off  int
}

func (r *reader) take(n int) ([]byte, error) {
	if len(r.data)-r.off < n {
		return nil, errors.New("unexpected end of data")
	}
	b := r.data[r.off : r.off+n]
	r.off += n
	return b, nil
}
