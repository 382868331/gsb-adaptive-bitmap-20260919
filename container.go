package bitmap

import (
	"math/bits"
	"sort"
)

// 阈值与布局常量。
const (
	// arrayMaxCardinality 是数组容器的最大基数；超过则切换为位图。
	arrayMaxCardinality = 4096
	// bitmapWords 是位图容器的 uint64 字数（覆盖低 16 位全域）。
	bitmapWords = 1024
	// bitmapPayloadBytes 是位图容器的载荷字节数（统计口径，非 Go 进程内存）。
	bitmapPayloadBytes = 8192
)

type containerKind byte

const (
	kindArray  containerKind = 0
	kindBitmap containerKind = 1
)

// container 保存一个高 16 位桶内的全部低 16 位值。
// kind==kindArray 时 arr 为升序且无重复；kind==kindBitmap 时 bm 有效。
type container struct {
	kind containerKind
	card int
	arr  []uint16
	bm   *[bitmapWords]uint64
}

func newArrayContainer(vals []uint16) *container {
	return &container{kind: kindArray, card: len(vals), arr: vals}
}

func newBitmapContainer() *container {
	return &container{kind: kindBitmap, bm: &[bitmapWords]uint64{}}
}

func (c *container) payloadBytes() uint64 {
	if c.kind == kindArray {
		return uint64(c.card) * 2
	}
	return bitmapPayloadBytes
}

func (c *container) clone() *container {
	n := &container{kind: c.kind, card: c.card}
	if c.kind == kindArray {
		n.arr = make([]uint16, len(c.arr))
		copy(n.arr, c.arr)
	} else {
		bm := *c.bm
		n.bm = &bm
	}
	return n
}

func (c *container) contains(v uint16) bool {
	if c.kind == kindArray {
		i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= v })
		return i < len(c.arr) && c.arr[i] == v
	}
	return c.bm[v>>6]&(uint64(1)<<(v&63)) != 0
}

// add 原地插入；返回是否发生变化。超过阈值时数组转位图。
func (c *container) add(v uint16) bool {
	if c.kind == kindArray {
		i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= v })
		if i < len(c.arr) && c.arr[i] == v {
			return false
		}
		c.arr = append(c.arr, 0)
		copy(c.arr[i+1:], c.arr[i:])
		c.arr[i] = v
		c.card++
		if c.card > arrayMaxCardinality {
			c.toBitmap()
		}
		return true
	}
	w := &c.bm[v>>6]
	mask := uint64(1) << (v & 63)
	if *w&mask != 0 {
		return false
	}
	*w |= mask
	c.card++
	return true
}

// remove 原地删除；返回是否发生变化。降回阈值时位图转数组。
func (c *container) remove(v uint16) bool {
	if c.kind == kindArray {
		i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= v })
		if i >= len(c.arr) || c.arr[i] != v {
			return false
		}
		copy(c.arr[i:], c.arr[i+1:])
		c.arr = c.arr[:len(c.arr)-1]
		c.card--
		return true
	}
	w := &c.bm[v>>6]
	mask := uint64(1) << (v & 63)
	if *w&mask == 0 {
		return false
	}
	*w &^= mask
	c.card--
	if c.card <= arrayMaxCardinality {
		c.toArray()
	}
	return true
}

func (c *container) toBitmap() {
	bm := &[bitmapWords]uint64{}
	for _, v := range c.arr {
		bm[v>>6] |= uint64(1) << (v & 63)
	}
	c.kind = kindBitmap
	c.arr = nil
	c.bm = bm
}

func (c *container) toArray() {
	arr := make([]uint16, 0, c.card)
	for wi, w := range c.bm {
		for w != 0 {
			t := bits.TrailingZeros64(w)
			arr = append(arr, uint16(wi*64+t))
			w &= w - 1
		}
	}
	c.kind = kindArray
	c.arr = arr
	c.bm = nil
}

// rank 返回容器内 <= v 的元素个数。
func (c *container) rank(v uint16) int {
	if c.kind == kindArray {
		return sort.Search(len(c.arr), func(i int) bool { return c.arr[i] > v })
	}
	wi := int(v >> 6)
	n := 0
	for i := 0; i < wi; i++ {
		n += bits.OnesCount64(c.bm[i])
	}
	n += bits.OnesCount64(c.bm[wi] & (uint64(1)<<(v&63)<<1 - 1))
	return n
}

// selectK 返回容器内第 k 小（0 起始）的值；k 必须小于 card。
func (c *container) selectK(k int) uint16 {
	if c.kind == kindArray {
		return c.arr[k]
	}
	for wi, w := range c.bm {
		pc := bits.OnesCount64(w)
		if k >= pc {
			k -= pc
			continue
		}
		for ; k > 0; k-- {
			w &= w - 1
		}
		return uint16(wi*64 + bits.TrailingZeros64(w))
	}
	panic("selectK: k out of range")
}

// fixup 按阈值把容器调整为规范表示；空容器由调用方移除。
func (c *container) fixup() {
	if c.kind == kindArray && c.card > arrayMaxCardinality {
		c.toBitmap()
	} else if c.kind == kindBitmap && c.card <= arrayMaxCardinality {
		c.toArray()
	}
}

// --- 容器级集合运算：均返回新容器，不修改入参；结果为空返回 nil。 ---

func unionContainers(a, b *container) *container {
	if a.kind == kindBitmap || b.kind == kindBitmap {
		var bm *[bitmapWords]uint64
		switch {
		case a.kind == kindBitmap && b.kind == kindBitmap:
			bm = &[bitmapWords]uint64{}
			for i := range bm {
				bm[i] = a.bm[i] | b.bm[i]
			}
		case a.kind == kindBitmap:
			bm = cloneWords(a.bm)
			setBits(bm, b.arr)
		default:
			bm = cloneWords(b.bm)
			setBits(bm, a.arr)
		}
		return bitmapFromWords(bm)
	}
	// 数组并集：归并。
	out := make([]uint16, 0, len(a.arr)+len(b.arr))
	i, j := 0, 0
	for i < len(a.arr) && j < len(b.arr) {
		switch {
		case a.arr[i] < b.arr[j]:
			out = append(out, a.arr[i])
			i++
		case a.arr[i] > b.arr[j]:
			out = append(out, b.arr[j])
			j++
		default:
			out = append(out, a.arr[i])
			i++
			j++
		}
	}
	out = append(out, a.arr[i:]...)
	out = append(out, b.arr[j:]...)
	c := newArrayContainer(out)
	c.fixup()
	return c
}

func intersectContainers(a, b *container) *container {
	if a.kind == kindBitmap && b.kind == kindBitmap {
		bm := &[bitmapWords]uint64{}
		n := 0
		for i := range bm {
			bm[i] = a.bm[i] & b.bm[i]
			n += bits.OnesCount64(bm[i])
		}
		if n == 0 {
			return nil
		}
		c := &container{kind: kindBitmap, card: n, bm: bm}
		c.fixup()
		return c
	}
	if a.kind == kindBitmap || b.kind == kindBitmap {
		// 数组 ∩ 位图：遍历较小的一侧（数组）。
		arr, bm := a.arr, a.bm
		if a.kind == kindBitmap {
			arr, bm = b.arr, a.bm
		}
		out := make([]uint16, 0, len(arr))
		for _, v := range arr {
			if bm[v>>6]&(uint64(1)<<(v&63)) != 0 {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return newArrayContainer(out)
	}
	out := make([]uint16, 0, min(len(a.arr), len(b.arr)))
	i, j := 0, 0
	for i < len(a.arr) && j < len(b.arr) {
		switch {
		case a.arr[i] < b.arr[j]:
			i++
		case a.arr[i] > b.arr[j]:
			j++
		default:
			out = append(out, a.arr[i])
			i++
			j++
		}
	}
	if len(out) == 0 {
		return nil
	}
	return newArrayContainer(out)
}

// differenceContainers 计算 a - b。
func differenceContainers(a, b *container) *container {
	if a.kind == kindBitmap {
		bm := cloneWords(a.bm)
		if b.kind == kindBitmap {
			for i := range bm {
				bm[i] &^= b.bm[i]
			}
		} else {
			for _, v := range b.arr {
				bm[v>>6] &^= uint64(1) << (v & 63)
			}
		}
		return bitmapFromWords(bm)
	}
	if b.kind == kindBitmap {
		out := make([]uint16, 0, len(a.arr))
		for _, v := range a.arr {
			if b.bm[v>>6]&(uint64(1)<<(v&63)) == 0 {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return newArrayContainer(out)
	}
	out := make([]uint16, 0, len(a.arr))
	i, j := 0, 0
	for i < len(a.arr) {
		for j < len(b.arr) && b.arr[j] < a.arr[i] {
			j++
		}
		if j < len(b.arr) && b.arr[j] == a.arr[i] {
			i++
			continue
		}
		out = append(out, a.arr[i])
		i++
	}
	if len(out) == 0 {
		return nil
	}
	return newArrayContainer(out)
}

func cloneWords(src *[bitmapWords]uint64) *[bitmapWords]uint64 {
	bm := *src
	return &bm
}

func setBits(bm *[bitmapWords]uint64, vals []uint16) {
	for _, v := range vals {
		bm[v>>6] |= uint64(1) << (v & 63)
	}
}

// bitmapFromWords 统计基数并按阈值选择规范表示；空则返回 nil。
func bitmapFromWords(bm *[bitmapWords]uint64) *container {
	n := 0
	for _, w := range bm {
		n += bits.OnesCount64(w)
	}
	if n == 0 {
		return nil
	}
	c := &container{kind: kindBitmap, card: n, bm: bm}
	c.fixup()
	return c
}
