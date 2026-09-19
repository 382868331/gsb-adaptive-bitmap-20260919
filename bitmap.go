// Package bitmap 实现自适应压缩位图：uint32 集合按高 16 位分桶，
// 桶内基数 <= 4096 用升序 uint16 数组，否则用 1024 个 uint64 位图。
// 支持集合运算、Rank/Select、递增迭代与确定性二进制序列化。
package bitmap

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"
)

// ErrSelectOutOfRange 在 Select 的 k 超出 [0, Cardinality) 时返回。
var ErrSelectOutOfRange = errors.New("bitmap: select index out of range")

type bucket struct {
	key uint16
	c   *container
}

// Bitmap 是 uint32 的有序集合。零值不可用，请用 New 创建。
type Bitmap struct {
	buckets []bucket // 按 key 严格递增
	card    uint64
}

// New 返回空集合。
func New() *Bitmap {
	return &Bitmap{}
}

// Cardinality 返回元素总数。
func (b *Bitmap) Cardinality() uint64 {
	return b.card
}

// find 返回 key 应处的桶下标及是否已存在。
func (b *Bitmap) find(key uint16) (int, bool) {
	i := sort.Search(len(b.buckets), func(i int) bool { return b.buckets[i].key >= key })
	if i < len(b.buckets) && b.buckets[i].key == key {
		return i, true
	}
	return i, false
}

// Add 原地插入 x；返回是否新增。
func (b *Bitmap) Add(x uint32) bool {
	key, lo := uint16(x>>16), uint16(x)
	i, ok := b.find(key)
	if !ok {
		b.buckets = append(b.buckets, bucket{})
		copy(b.buckets[i+1:], b.buckets[i:])
		b.buckets[i] = bucket{key: key, c: newArrayContainer([]uint16{lo})}
		b.card++
		return true
	}
	if b.buckets[i].c.add(lo) {
		b.card++
		return true
	}
	return false
}

// Remove 原地删除 x；返回是否删除成功。
func (b *Bitmap) Remove(x uint32) bool {
	key, lo := uint16(x>>16), uint16(x)
	i, ok := b.find(key)
	if !ok {
		return false
	}
	if !b.buckets[i].c.remove(lo) {
		return false
	}
	b.card--
	if b.buckets[i].c.card == 0 {
		copy(b.buckets[i:], b.buckets[i+1:])
		b.buckets = b.buckets[:len(b.buckets)-1]
	}
	return true
}

// Contains 报告 x 是否在集合中。
func (b *Bitmap) Contains(x uint32) bool {
	i, ok := b.find(uint16(x >> 16))
	return ok && b.buckets[i].c.contains(uint16(x))
}

// Rank 返回集合中 <= x 的元素个数。
func (b *Bitmap) Rank(x uint32) uint64 {
	key, lo := uint16(x>>16), uint16(x)
	var n uint64
	for i := range b.buckets {
		k := b.buckets[i].key
		if k > key {
			break
		}
		if k == key {
			return n + uint64(b.buckets[i].c.rank(lo))
		}
		n += uint64(b.buckets[i].c.card)
	}
	return n
}

// Select 返回第 k 小（0 起始）的元素；k 越界时返回 ErrSelectOutOfRange。
func (b *Bitmap) Select(k uint64) (uint32, error) {
	if k >= b.card {
		return 0, fmt.Errorf("%w: k=%d cardinality=%d", ErrSelectOutOfRange, k, b.card)
	}
	for i := range b.buckets {
		c := b.buckets[i].c
		if k < uint64(c.card) {
			return uint32(b.buckets[i].key)<<16 | uint32(c.selectK(int(k))), nil
		}
		k -= uint64(c.card)
	}
	panic("bitmap: select unreachable")
}

// PayloadBytes 返回载荷字节统计：数组容器按 元素数×2，位图容器按 8192。
// 这是序列化/存储口径的固定统计，不是 Go 进程内存占用。
func (b *Bitmap) PayloadBytes() uint64 {
	var n uint64
	for i := range b.buckets {
		n += b.buckets[i].c.payloadBytes()
	}
	return n
}

// BucketStats 返回数组容器与位图容器的数量，用于观察稀疏/稠密转换。
func (b *Bitmap) BucketStats() (arrays, bitmaps int) {
	for i := range b.buckets {
		if b.buckets[i].c.kind == kindArray {
			arrays++
		} else {
			bitmaps++
		}
	}
	return arrays, bitmaps
}

// Clone 返回深拷贝；两集合不共享任何可写容器。
func (b *Bitmap) Clone() *Bitmap {
	n := &Bitmap{card: b.card, buckets: make([]bucket, len(b.buckets))}
	for i, bk := range b.buckets {
		n.buckets[i] = bucket{key: bk.key, c: bk.c.clone()}
	}
	return n
}

// Union 返回新集合 a ∪ b；不修改入参，也不与入参共享可写容器。
func (b *Bitmap) Union(o *Bitmap) *Bitmap {
	out := &Bitmap{}
	i, j := 0, 0
	for i < len(b.buckets) && j < len(o.buckets) {
		bk, ok := b.buckets[i], o.buckets[j]
		switch {
		case bk.key < ok.key:
			out.push(bk.key, bk.c.clone())
			i++
		case bk.key > ok.key:
			out.push(ok.key, ok.c.clone())
			j++
		default:
			out.push(bk.key, unionContainers(bk.c, ok.c))
			i++
			j++
		}
	}
	for ; i < len(b.buckets); i++ {
		out.push(b.buckets[i].key, b.buckets[i].c.clone())
	}
	for ; j < len(o.buckets); j++ {
		out.push(o.buckets[j].key, o.buckets[j].c.clone())
	}
	return out
}

// Intersect 返回新集合 a ∩ b；不修改入参。
func (b *Bitmap) Intersect(o *Bitmap) *Bitmap {
	out := &Bitmap{}
	i, j := 0, 0
	for i < len(b.buckets) && j < len(o.buckets) {
		bk, ok := b.buckets[i], o.buckets[j]
		switch {
		case bk.key < ok.key:
			i++
		case bk.key > ok.key:
			j++
		default:
			if c := intersectContainers(bk.c, ok.c); c != nil {
				out.push(bk.key, c)
			}
			i++
			j++
		}
	}
	return out
}

// Difference 返回新集合 a - b；不修改入参。
func (b *Bitmap) Difference(o *Bitmap) *Bitmap {
	out := &Bitmap{}
	i, j := 0, 0
	for i < len(b.buckets) && j < len(o.buckets) {
		bk, ok := b.buckets[i], o.buckets[j]
		switch {
		case bk.key < ok.key:
			out.push(bk.key, bk.c.clone())
			i++
		case bk.key > ok.key:
			j++
		default:
			if c := differenceContainers(bk.c, ok.c); c != nil {
				out.push(bk.key, c)
			}
			i++
			j++
		}
	}
	for ; i < len(b.buckets); i++ {
		out.push(b.buckets[i].key, b.buckets[i].c.clone())
	}
	return out
}

func (b *Bitmap) push(key uint16, c *container) {
	b.buckets = append(b.buckets, bucket{key: key, c: c})
	b.card += uint64(c.card)
}

// Iterator 按递增序遍历集合。迭代期间不得修改集合。
type Iterator struct {
	b    *Bitmap
	bi   int
	ai   int    // 数组容器下标
	wi   int    // 位图容器字下标
	word uint64 // 当前字的剩余位
}

// Iterator 返回从最小元素开始的迭代器。
func (b *Bitmap) Iterator() *Iterator {
	return &Iterator{b: b}
}

// Next 返回下一个元素；耗尽时 ok=false。
func (it *Iterator) Next() (v uint32, ok bool) {
	for it.bi < len(it.b.buckets) {
		bk := &it.b.buckets[it.bi]
		c := bk.c
		if c.kind == kindArray {
			if it.ai < len(c.arr) {
				v = uint32(bk.key)<<16 | uint32(c.arr[it.ai])
				it.ai++
				return v, true
			}
		} else {
			for {
				if it.word != 0 {
					t := bits.TrailingZeros64(it.word)
					it.word &= it.word - 1
					return uint32(bk.key)<<16 | uint32((it.wi-1)*64+t), true
				}
				if it.wi >= bitmapWords {
					break
				}
				it.word = c.bm[it.wi]
				it.wi++
			}
		}
		it.bi++
		it.ai, it.wi, it.word = 0, 0, 0
	}
	return 0, false
}
