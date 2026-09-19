// Command demo exercises the bitmap library: sparse/dense container
// transitions, cross-bucket Rank/Select, set algebra, serialization
// round-trips, and two deliberately triggered failures (Select out of
// range and a corrupted payload rejected by Unmarshal).
package main

import (
	"errors"
	"fmt"
	"math/rand"
	"time"

	bitmap "github.com/382868331/gsb-adaptive-bitmap-20260919"
)

func main() {
	start := time.Now()
	fmt.Println("== adaptive bitmap demo ==")

	// 1. Sparse/dense transition inside one bucket (high 16 bits = 0x0007).
	s := bitmap.New()
	for i := uint32(0); i < 4096; i++ {
		s.Add(0x0007_0000 + i*3) // spread out, still sparse
	}
	fmt.Printf("[1] bucket 0x0007 with %d values: %s\n",
		s.Cardinality(), bitmap.BucketKind(s, 0x0007))
	for i := uint32(0); i < 4096; i++ {
		s.Add(0x0007_0000 + i*3 + 1) // 8192 values -> dense
	}
	fmt.Printf("    after growing to %d values: %s\n",
		s.Cardinality(), bitmap.BucketKind(s, 0x0007))
	for i := uint32(0); i < 4096; i++ {
		s.Remove(0x0007_0000 + i*3 + 1) // back to 4096 -> sparse
	}
	fmt.Printf("    after shrinking to %d values: %s (payload %d bytes)\n",
		s.Cardinality(), bitmap.BucketKind(s, 0x0007), s.PayloadBytes())

	// 2. Cross-bucket Rank/Select.
	m := bitmap.New()
	for _, v := range []uint32{0, 5, 1 << 16, (1 << 16) + 7, 300_000, 1_000_000_000, 0xFFFF_FFFF} {
		m.Add(v)
	}
	fmt.Printf("[2] set of %d values across %d buckets\n", m.Cardinality(), m.BucketCount())
	for _, x := range []uint32{4, 1 << 16, 999_999_999, 0xFFFF_FFFF} {
		fmt.Printf("    Rank(%d) = %d\n", x, m.Rank(x))
	}
	for k := uint64(0); k < m.Cardinality(); k++ {
		v, _ := m.Select(k)
		fmt.Printf("    Select(%d) = %d\n", k, v)
	}

	// 3. Set algebra (results are fresh sets; inputs unchanged).
	a := bitmap.New()
	b := bitmap.New()
	rng := rand.New(rand.NewSource(20260919))
	for i := 0; i < 6000; i++ { // crosses the 4096 threshold in bucket 0
		a.Add(rng.Uint32() & 0xFFFF)
	}
	for i := 0; i < 6000; i++ {
		b.Add(rng.Uint32() & 0x1FFFF) // spans two buckets
	}
	u, in, d := bitmap.Union(a, b), bitmap.Intersect(a, b), bitmap.Difference(a, b)
	fmt.Printf("[3] |A|=%d |B|=%d |A∪B|=%d |A∩B|=%d |A\\B|=%d (identities hold: %v)\n",
		a.Cardinality(), b.Cardinality(), u.Cardinality(), in.Cardinality(), d.Cardinality(),
		u.Cardinality() == a.Cardinality()+b.Cardinality()-in.Cardinality() &&
			in.Cardinality()+d.Cardinality() == a.Cardinality())

	// 4. Deterministic serialization round-trip.
	buf := m.MarshalBinary()
	back, err := bitmap.Unmarshal(buf, 1<<20)
	if err != nil {
		fmt.Println("[4] round-trip FAILED:", err)
		return
	}
	fmt.Printf("[4] %d bytes serialized, round-trip equal: %v\n", len(buf), back.Equal(m))

	// 5. Deliberately triggered failure #1: Select out of range.
	if _, err := m.Select(m.Cardinality()); err != nil {
		fmt.Printf("[5] expected failure: Select(%d) -> %v\n", m.Cardinality(), err)
	}

	// 6. Deliberately triggered failure #2: corrupted payload rejected.
	bad := append([]byte(nil), buf...)
	// First bucket record starts at offset 8, its array payload at offset 16:
	// values 0, 5. Overwriting the second value with 0 creates a duplicate.
	bad[18], bad[19] = 0, 0
	if _, err := bitmap.Unmarshal(bad, 1<<20); err != nil {
		fmt.Printf("[6] expected failure: corrupted payload -> %v\n", err)
	}
	// ... and a payload limit that is too small.
	if _, err := bitmap.Unmarshal(buf, 2); errors.Is(err, bitmap.ErrPayloadLimit) {
		fmt.Printf("[6] expected failure: tight payload limit -> %v\n", err)
	}

	fmt.Printf("done in %s\n", time.Since(start).Round(time.Millisecond))
}
