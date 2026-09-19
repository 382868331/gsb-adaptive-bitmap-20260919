// Package bitmap implements an adaptive compressed bitmap set of uint32
// values. Values are bucketed by their high 16 bits; each bucket stores its
// low 16 bits either as a sorted uint16 array (cardinality <= 4096) or as a
// 1024-word uint64 bitmap (cardinality > 4096). The representation switches
// automatically at the threshold after updates and set operations.
package bitmap

import (
	"errors"
	"math/bits"
	"sort"
)

const (
	// ArrayMaxCardinality is the largest bucket cardinality stored as a
	// sorted uint16 array. Above it the bucket becomes a bitmap.
	ArrayMaxCardinality = 4096
	// bitmapWords is the number of uint64 words in a dense bucket bitmap.
	bitmapWords = 1024
	// BitmapBytes is the payload size of a dense bucket bitmap.
	BitmapBytes = bitmapWords * 8
	// bucketCapacity is the number of distinct low-16-bit values per bucket.
	bucketCapacity = 1 << 16
)

// ErrSelectOutOfRange is returned by Select when k >= Cardinality().
var ErrSelectOutOfRange = errors.New("bitmap: select index out of range")

// container holds the low 16 bits of one bucket in exactly one of two
// canonical forms: a sorted array (card <= ArrayMaxCardinality) or a bitmap.
type container struct {
	isBitmap bool
	arr      []uint16 // sorted, no duplicates; valid when !isBitmap
	bits     []uint64 // len == bitmapWords; valid when isBitmap
	card     int
}

func newArrayContainer(vals []uint16) *container {
	return &container{arr: vals, card: len(vals)}
}

func newBitmapContainer() *container {
	return &container{isBitmap: true, bits: make([]uint64, bitmapWords)}
}

// toBitmap converts an array container to a bitmap container in place.
func (c *container) toBitmap() {
	bits := make([]uint64, bitmapWords)
	for _, v := range c.arr {
		bits[v>>6] |= 1 << (v & 63)
	}
	c.isBitmap = true
	c.bits = bits
	c.arr = nil
}

// toArray converts a bitmap container to an array container in place.
// The caller must guarantee c.card <= ArrayMaxCardinality.
func (c *container) toArray() {
	arr := make([]uint16, 0, c.card)
	for w := 0; w < bitmapWords; w++ {
		word := c.bits[w]
		for word != 0 {
			b := bits.TrailingZeros64(word)
			arr = append(arr, uint16(w<<6|b))
			word &= word - 1
		}
	}
	c.isBitmap = false
	c.arr = arr
	c.bits = nil
}

// clone returns a deep copy; the result shares no writable memory.
func (c *container) clone() *container {
	n := &container{isBitmap: c.isBitmap, card: c.card}
	if c.isBitmap {
		n.bits = make([]uint64, bitmapWords)
		copy(n.bits, c.bits)
	} else {
		n.arr = make([]uint16, len(c.arr))
		copy(n.arr, c.arr)
	}
	return n
}

func (c *container) contains(lo uint16) bool {
	if c.isBitmap {
		return c.bits[lo>>6]&(1<<(lo&63)) != 0
	}
	i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= lo })
	return i < len(c.arr) && c.arr[i] == lo
}

// add inserts lo, reporting whether it was new. Converts to a bitmap when
// the cardinality crosses the threshold.
func (c *container) add(lo uint16) bool {
	if c.isBitmap {
		w := lo >> 6
		m := uint64(1) << (lo & 63)
		if c.bits[w]&m != 0 {
			return false
		}
		c.bits[w] |= m
		c.card++
		return true
	}
	i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= lo })
	if i < len(c.arr) && c.arr[i] == lo {
		return false
	}
	c.arr = append(c.arr, 0)
	copy(c.arr[i+1:], c.arr[i:])
	c.arr[i] = lo
	c.card++
	if c.card > ArrayMaxCardinality {
		c.toBitmap()
	}
	return true
}

// remove deletes lo, reporting whether it was present. Converts to an array
// when the cardinality drops to the threshold.
func (c *container) remove(lo uint16) bool {
	if c.isBitmap {
		w := lo >> 6
		m := uint64(1) << (lo & 63)
		if c.bits[w]&m == 0 {
			return false
		}
		c.bits[w] &^= m
		c.card--
		if c.card <= ArrayMaxCardinality {
			c.toArray()
		}
		return true
	}
	i := sort.Search(len(c.arr), func(i int) bool { return c.arr[i] >= lo })
	if i >= len(c.arr) || c.arr[i] != lo {
		return false
	}
	copy(c.arr[i:], c.arr[i+1:])
	c.arr = c.arr[:len(c.arr)-1]
	c.card--
	return true
}

// rank returns the number of elements <= lo.
func (c *container) rank(lo uint16) int {
	if c.isBitmap {
		w := int(lo >> 6)
		n := 0
		for i := 0; i < w; i++ {
			n += bits.OnesCount64(c.bits[i])
		}
		// Keep bits 0..lo&63 inclusive.
		n += bits.OnesCount64(c.bits[w] & (^uint64(0) >> (63 - (lo & 63))))
		return n
	}
	return sort.Search(len(c.arr), func(i int) bool { return c.arr[i] > lo })
}

// selectAt returns the k-th (0-based) element. k must be < c.card.
func (c *container) selectAt(k int) uint16 {
	if !c.isBitmap {
		return c.arr[k]
	}
	for w := 0; w < bitmapWords; w++ {
		pc := bits.OnesCount64(c.bits[w])
		if k >= pc {
			k -= pc
			continue
		}
		word := c.bits[w]
		for ; k > 0; k-- {
			word &= word - 1
		}
		return uint16(w<<6 | bits.TrailingZeros64(word))
	}
	panic("bitmap: selectAt index out of range")
}

// normalize returns nil for an empty container and converts a bitmap at or
// below the threshold back to an array, restoring canonical form.
func normalize(c *container) *container {
	if c.card == 0 {
		return nil
	}
	if c.isBitmap && c.card <= ArrayMaxCardinality {
		c.toArray()
	}
	return c
}

// bucket pairs a high-16-bit key with its container.
type bucket struct {
	key uint16
	c   *container
}

// Set is a set of uint32 values. The zero value is ready to use, but
// set operations always return freshly allocated sets.
type Set struct {
	buckets []bucket // sorted by key, no empty containers
	card    uint64
}

// New returns an empty set.
func New() *Set { return &Set{} }

// Cardinality returns the total number of elements.
func (s *Set) Cardinality() uint64 { return s.card }

// find locates the bucket for key, returning its index and whether it exists.
func (s *Set) find(key uint16) (int, bool) {
	i := sort.Search(len(s.buckets), func(i int) bool { return s.buckets[i].key >= key })
	return i, i < len(s.buckets) && s.buckets[i].key == key
}

// Add inserts x, reporting whether it was new. Safe to call in place.
func (s *Set) Add(x uint32) bool {
	hi, lo := uint16(x>>16), uint16(x)
	i, ok := s.find(hi)
	if !ok {
		s.buckets = append(s.buckets, bucket{})
		copy(s.buckets[i+1:], s.buckets[i:])
		s.buckets[i] = bucket{key: hi, c: newArrayContainer([]uint16{lo})}
		s.card++
		return true
	}
	if s.buckets[i].c.add(lo) {
		s.card++
		return true
	}
	return false
}

// Remove deletes x, reporting whether it was present. Safe to call in place.
func (s *Set) Remove(x uint32) bool {
	hi, lo := uint16(x>>16), uint16(x)
	i, ok := s.find(hi)
	if !ok {
		return false
	}
	if !s.buckets[i].c.remove(lo) {
		return false
	}
	s.card--
	if s.buckets[i].c.card == 0 {
		copy(s.buckets[i:], s.buckets[i+1:])
		s.buckets = s.buckets[:len(s.buckets)-1]
	}
	return true
}

// Contains reports whether x is in the set.
func (s *Set) Contains(x uint32) bool {
	i, ok := s.find(uint16(x >> 16))
	return ok && s.buckets[i].c.contains(uint16(x))
}

// Rank returns the number of elements <= x.
func (s *Set) Rank(x uint32) uint64 {
	hi, lo := uint16(x>>16), uint16(x)
	var n uint64
	i, _ := s.find(hi)
	for b := 0; b < i; b++ {
		n += uint64(s.buckets[b].c.card)
	}
	if i < len(s.buckets) && s.buckets[i].key == hi {
		n += uint64(s.buckets[i].c.rank(lo))
	}
	return n
}

// Select returns the k-th smallest element (0-based). It returns
// ErrSelectOutOfRange when k >= Cardinality().
func (s *Set) Select(k uint64) (uint32, error) {
	if k >= s.card {
		return 0, ErrSelectOutOfRange
	}
	for i := range s.buckets {
		b := &s.buckets[i]
		if k < uint64(b.c.card) {
			return uint32(b.key)<<16 | uint32(b.c.selectAt(int(k))), nil
		}
		k -= uint64(b.c.card)
	}
	return 0, ErrSelectOutOfRange
}

// Iterator iterates over a set in increasing order. It is not safe to
// mutate the set during iteration.
type Iterator struct {
	s     *Set
	bi    int    // bucket index
	ai    int    // array index within the bucket
	wi    int    // next word index to load from the bucket bitmap
	wbase int    // word index of the bits currently in word
	word  uint64 // remaining bits of the current word
}

// Iterator returns an iterator over the set in increasing order.
func (s *Set) Iterator() *Iterator { return &Iterator{s: s} }

// Next advances the iterator, returning the next value and whether it exists.
func (it *Iterator) Next() (uint32, bool) {
	for it.bi < len(it.s.buckets) {
		b := &it.s.buckets[it.bi]
		c := b.c
		if !c.isBitmap {
			if it.ai < len(c.arr) {
				v := c.arr[it.ai]
				it.ai++
				return uint32(b.key)<<16 | uint32(v), true
			}
		} else {
			for {
				if it.word != 0 {
					bit := bits.TrailingZeros64(it.word)
					it.word &= it.word - 1
					return uint32(b.key)<<16 | uint32(it.wbase<<6|bit), true
				}
				if it.wi >= bitmapWords {
					break
				}
				it.wbase = it.wi
				it.word = c.bits[it.wi]
				it.wi++
			}
		}
		it.bi++
		it.ai, it.wi, it.wbase, it.word = 0, 0, 0, 0
	}
	return 0, false
}

// IsBitmapBucket reports whether the bucket for high-16-bit key hi uses the
// bitmap representation. It returns false for a missing bucket; intended for
// tests, demos and diagnostics.
func (s *Set) IsBitmapBucket(hi uint16) bool {
	i, ok := s.find(hi)
	return ok && s.buckets[i].c.isBitmap
}

// BucketCount returns the number of non-empty buckets.
func (s *Set) BucketCount() int { return len(s.buckets) }

// Equal reports whether two sets contain exactly the same elements.
func (s *Set) Equal(o *Set) bool {
	if s.card != o.card || len(s.buckets) != len(o.buckets) {
		return false
	}
	for i := range s.buckets {
		a, b := &s.buckets[i], &o.buckets[i]
		if a.key != b.key || a.c.card != b.c.card || a.c.isBitmap != b.c.isBitmap {
			return false
		}
		if a.c.isBitmap {
			for w := 0; w < bitmapWords; w++ {
				if a.c.bits[w] != b.c.bits[w] {
					return false
				}
			}
		} else {
			for j, v := range a.c.arr {
				if b.c.arr[j] != v {
					return false
				}
			}
		}
	}
	return true
}

// BucketKind reports the container representation ("array", "bitmap", or
// "absent") of the bucket for high-16-bit key hi. Intended for demos,
// diagnostics and tests.
func BucketKind(s *Set, hi uint16) string {
	i, ok := s.find(hi)
	if !ok {
		return "absent"
	}
	if s.buckets[i].c.isBitmap {
		return "bitmap"
	}
	return "array"
}

// PayloadBytes returns the logical payload size in bytes: for each bucket,
// 2 bytes per array element or exactly 8192 bytes per bitmap. This is a
// fixed accounting of serialized container payloads, not Go process memory.
func (s *Set) PayloadBytes() uint64 {
	var n uint64
	for i := range s.buckets {
		n += s.buckets[i].c.payloadBytes()
	}
	return n
}

func (c *container) payloadBytes() uint64 {
	if c.isBitmap {
		return BitmapBytes
	}
	return uint64(c.card) * 2
}

// --- container-level set operations; each returns a fresh container ---

func unionContainers(a, b *container) *container {
	switch {
	case a.isBitmap && b.isBitmap:
		n := newBitmapContainer()
		card := 0
		for i := 0; i < bitmapWords; i++ {
			w := a.bits[i] | b.bits[i]
			n.bits[i] = w
			card += bits.OnesCount64(w)
		}
		n.card = card
		return n
	case a.isBitmap || b.isBitmap:
		bm, ar := a, b
		if !a.isBitmap {
			bm, ar = b, a
		}
		n := bm.clone()
		for _, v := range ar.arr {
			w := v >> 6
			m := uint64(1) << (v & 63)
			if n.bits[w]&m == 0 {
				n.bits[w] |= m
				n.card++
			}
		}
		return n
	default:
		merged := mergeUnion(a.arr, b.arr)
		if len(merged) > ArrayMaxCardinality {
			n := newBitmapContainer()
			for _, v := range merged {
				n.bits[v>>6] |= 1 << (v & 63)
			}
			n.card = len(merged)
			return n
		}
		return newArrayContainer(merged)
	}
}

func mergeUnion(a, b []uint16) []uint16 {
	out := make([]uint16, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

func intersectContainers(a, b *container) *container {
	switch {
	case a.isBitmap && b.isBitmap:
		n := newBitmapContainer()
		card := 0
		for i := 0; i < bitmapWords; i++ {
			w := a.bits[i] & b.bits[i]
			n.bits[i] = w
			card += bits.OnesCount64(w)
		}
		n.card = card
		return normalize(n)
	case a.isBitmap || b.isBitmap:
		bm, ar := a, b
		if !a.isBitmap {
			bm, ar = b, a
		}
		out := make([]uint16, 0, len(ar.arr))
		for _, v := range ar.arr {
			if bm.bits[v>>6]&(1<<(v&63)) != 0 {
				out = append(out, v)
			}
		}
		return normalize(newArrayContainer(out))
	default:
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
		return normalize(newArrayContainer(out))
	}
}

// differenceContainers returns a \ b as a fresh container.
func differenceContainers(a, b *container) *container {
	switch {
	case a.isBitmap && b.isBitmap:
		n := newBitmapContainer()
		card := 0
		for i := 0; i < bitmapWords; i++ {
			w := a.bits[i] &^ b.bits[i]
			n.bits[i] = w
			card += bits.OnesCount64(w)
		}
		n.card = card
		return normalize(n)
	case a.isBitmap: // bitmap minus array
		n := a.clone()
		for _, v := range b.arr {
			w := v >> 6
			m := uint64(1) << (v & 63)
			if n.bits[w]&m != 0 {
				n.bits[w] &^= m
				n.card--
			}
		}
		return normalize(n)
	case b.isBitmap: // array minus bitmap
		out := make([]uint16, 0, len(a.arr))
		for _, v := range a.arr {
			if b.bits[v>>6]&(1<<(v&63)) == 0 {
				out = append(out, v)
			}
		}
		return normalize(newArrayContainer(out))
	default:
		out := make([]uint16, 0, len(a.arr))
		i, j := 0, 0
		for i < len(a.arr) {
			for j < len(b.arr) && b.arr[j] < a.arr[i] {
				j++
			}
			if j >= len(b.arr) || b.arr[j] != a.arr[i] {
				out = append(out, a.arr[i])
			}
			i++
		}
		return normalize(newArrayContainer(out))
	}
}

// opSet applies a binary container operation bucket-wise. keep(a, b *container)
// receives nil for a missing bucket; a nil result drops the bucket.
func opSet(a, b *Set, keep func(x, y *container) *container) *Set {
	out := &Set{}
	ia, ib := 0, 0
	for ia < len(a.buckets) || ib < len(b.buckets) {
		var key uint16
		var ca, cb *container
		switch {
		case ib >= len(b.buckets) || (ia < len(a.buckets) && a.buckets[ia].key < b.buckets[ib].key):
			key = a.buckets[ia].key
			ca = a.buckets[ia].c
			ia++
		case ia >= len(a.buckets) || b.buckets[ib].key < a.buckets[ia].key:
			key = b.buckets[ib].key
			cb = b.buckets[ib].c
			ib++
		default:
			key = a.buckets[ia].key
			ca, cb = a.buckets[ia].c, b.buckets[ib].c
			ia++
			ib++
		}
		if c := keep(ca, cb); c != nil {
			out.buckets = append(out.buckets, bucket{key: key, c: c})
			out.card += uint64(c.card)
		}
	}
	return out
}

// Union returns a new set with every element of a or b. Inputs are not
// modified and no writable container memory is shared with the result.
func Union(a, b *Set) *Set {
	return opSet(a, b, func(x, y *container) *container {
		switch {
		case x == nil:
			return y.clone()
		case y == nil:
			return x.clone()
		default:
			return unionContainers(x, y)
		}
	})
}

// Intersect returns a new set with the elements present in both a and b.
func Intersect(a, b *Set) *Set {
	return opSet(a, b, func(x, y *container) *container {
		if x == nil || y == nil {
			return nil
		}
		return intersectContainers(x, y)
	})
}

// Difference returns a new set with the elements of a that are not in b.
func Difference(a, b *Set) *Set {
	return opSet(a, b, func(x, y *container) *container {
		switch {
		case x == nil:
			return nil
		case y == nil:
			return x.clone()
		default:
			return differenceContainers(x, y)
		}
	})
}
