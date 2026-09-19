package bitmap

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// --- helpers ---------------------------------------------------------------

func setOf(vals ...uint32) *Set {
	s := New()
	for _, v := range vals {
		s.Add(v)
	}
	return s
}

func collect(s *Set) []uint32 {
	var out []uint32
	it := s.Iterator()
	for v, ok := it.Next(); ok; v, ok = it.Next() {
		out = append(out, v)
	}
	return out
}

// mapRef is the reference model used to cross-check the library.
type mapRef map[uint32]struct{}

func (r mapRef) sorted() []uint32 {
	out := make([]uint32, 0, len(r))
	for v := range r {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r mapRef) rank(x uint32) uint64 {
	var n uint64
	for v := range r {
		if v <= x {
			n++
		}
	}
	return n
}

// --- representation threshold ----------------------------------------------

func TestThreshold4096And4097RoundTrip(t *testing.T) {
	s := New()
	for i := uint32(0); i < 4096; i++ {
		s.Add(i * 2) // even values in bucket 0
	}
	if got := BucketKind(s, 0); got != "array" {
		t.Fatalf("4096 elements: kind = %s, want array", got)
	}
	s.Add(4096 * 2) // 4097th element
	if got := BucketKind(s, 0); got != "bitmap" {
		t.Fatalf("4097 elements: kind = %s, want bitmap", got)
	}

	// Representation round-trip: bitmap survives marshal/unmarshal as bitmap.
	back, err := Unmarshal(s.MarshalBinary(), 1<<20)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := BucketKind(back, 0); got != "bitmap" {
		t.Fatalf("after round-trip: kind = %s, want bitmap", got)
	}
	if !back.Equal(s) {
		t.Fatal("round-trip changed the set")
	}

	// Shrinking back to 4096 restores the array form, also after round-trip.
	if !s.Remove(4096 * 2) {
		t.Fatal("Remove of present element returned false")
	}
	if got := BucketKind(s, 0); got != "array" {
		t.Fatalf("back to 4096: kind = %s, want array", got)
	}
	back2, err := Unmarshal(s.MarshalBinary(), 1<<20)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := BucketKind(back2, 0); got != "array" {
		t.Fatalf("array round-trip: kind = %s, want array", got)
	}
	if !back2.Equal(s) {
		t.Fatal("array round-trip changed the set")
	}
}

// --- edge values ------------------------------------------------------------

func TestZeroAndMaxUint32(t *testing.T) {
	s := setOf(0, math.MaxUint32)
	if !s.Contains(0) || !s.Contains(math.MaxUint32) {
		t.Fatal("Contains failed for boundary values")
	}
	if s.Contains(1) || s.Contains(math.MaxUint32-1) {
		t.Fatal("Contains false positive near boundaries")
	}
	if got := s.Rank(0); got != 1 {
		t.Fatalf("Rank(0) = %d, want 1", got)
	}
	if got := s.Rank(math.MaxUint32); got != 2 {
		t.Fatalf("Rank(MaxUint32) = %d, want 2", got)
	}
	v, err := s.Select(0)
	if err != nil || v != 0 {
		t.Fatalf("Select(0) = %d, %v; want 0, nil", v, err)
	}
	v, err = s.Select(1)
	if err != nil || v != math.MaxUint32 {
		t.Fatalf("Select(1) = %d, %v; want MaxUint32, nil", v, err)
	}
	if !s.Remove(math.MaxUint32) || s.Contains(math.MaxUint32) {
		t.Fatal("Remove(MaxUint32) failed")
	}
	if !s.Remove(0) || s.BucketCount() != 0 {
		t.Fatal("removing last elements should drop all buckets")
	}
}

func TestEmptySet(t *testing.T) {
	s := New()
	if s.Cardinality() != 0 || s.BucketCount() != 0 {
		t.Fatal("empty set has non-zero cardinality or buckets")
	}
	if s.Contains(0) || s.Contains(math.MaxUint32) {
		t.Fatal("empty set contains something")
	}
	if got := s.Rank(math.MaxUint32); got != 0 {
		t.Fatalf("Rank on empty set = %d, want 0", got)
	}
	if _, err := s.Select(0); !errors.Is(err, ErrSelectOutOfRange) {
		t.Fatalf("Select(0) on empty set: err = %v, want ErrSelectOutOfRange", err)
	}
	if v, ok := s.Iterator().Next(); ok {
		t.Fatalf("empty iterator yielded %d", v)
	}
	// Empty set serializes to just the header and comes back empty.
	buf := s.MarshalBinary()
	if len(buf) != 8 {
		t.Fatalf("empty encoding = %d bytes, want 8", len(buf))
	}
	back, err := Unmarshal(buf, 0)
	if err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if back.Cardinality() != 0 {
		t.Fatal("empty round-trip not empty")
	}
	// Set operations with an empty set.
	full := setOf(1, 2, 3)
	if got := Union(s, full).Cardinality(); got != 3 {
		t.Fatalf("Union(empty, full) = %d, want 3", got)
	}
	if got := Intersect(s, full).Cardinality(); got != 0 {
		t.Fatalf("Intersect(empty, full) = %d, want 0", got)
	}
	if got := Difference(full, s).Cardinality(); got != 3 {
		t.Fatalf("Difference(full, empty) = %d, want 3", got)
	}
}

// --- cross-bucket rank/select ------------------------------------------------

func TestCrossBucketRankSelect(t *testing.T) {
	vals := []uint32{0, 5, 1 << 16, (1 << 16) + 7, 300_000, 1_000_000_000, math.MaxUint32}
	s := setOf(vals...)
	if got := s.BucketCount(); got != 5 {
		t.Fatalf("BucketCount = %d, want 5", got)
	}
	wantRank := map[uint32]uint64{
		0: 1, 4: 1, 5: 2, 6: 2,
		1 << 16: 3, (1 << 16) + 6: 3, (1 << 16) + 7: 4,
		299_999: 4, 300_000: 5, 999_999_999: 5, 1_000_000_000: 6,
		math.MaxUint32 - 1: 6, math.MaxUint32: 7,
	}
	for x, want := range wantRank {
		if got := s.Rank(x); got != want {
			t.Errorf("Rank(%d) = %d, want %d", x, got, want)
		}
	}
	for k, want := range vals {
		got, err := s.Select(uint64(k))
		if err != nil || got != want {
			t.Errorf("Select(%d) = %d, %v; want %d, nil", k, got, err, want)
		}
	}
	if _, err := s.Select(uint64(len(vals))); !errors.Is(err, ErrSelectOutOfRange) {
		t.Fatalf("Select(card) err = %v, want ErrSelectOutOfRange", err)
	}
	// Iteration yields exactly the sorted values.
	got := collect(s)
	if len(got) != len(vals) {
		t.Fatalf("iterated %d values, want %d", len(got), len(vals))
	}
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("iteration[%d] = %d, want %d", i, got[i], vals[i])
		}
	}
}

// --- set operations ----------------------------------------------------------

func TestOpsDoNotMutateInputs(t *testing.T) {
	a := setOf(1, 2, 3, 1 << 16, math.MaxUint32)
	b := setOf(3, 4, 1<<16, 2<<16)
	// Grow a into a bitmap bucket so all container combinations are exercised.
	for i := uint32(0); i < 5000; i++ {
		a.Add(7_000_000 + i)
	}
	aBefore := a.MarshalBinary()
	bBefore := b.MarshalBinary()

	u, in, d := Union(a, b), Intersect(a, b), Difference(a, b)
	if got := u.Cardinality(); got != a.Cardinality()+b.Cardinality()-in.Cardinality() {
		t.Fatalf("union cardinality inconsistent: %d", got)
	}
	if got := in.Cardinality() + d.Cardinality(); got != a.Cardinality() {
		t.Fatalf("intersect+difference = %d, want |a| = %d", got, a.Cardinality())
	}
	if string(a.MarshalBinary()) != string(aBefore) {
		t.Fatal("Union/Intersect/Difference mutated input a")
	}
	if string(b.MarshalBinary()) != string(bBefore) {
		t.Fatal("Union/Intersect/Difference mutated input b")
	}
	// Mutating a result must not leak into the inputs (no shared containers).
	u.Add(123456789)
	if a.Contains(123456789) || b.Contains(123456789) {
		t.Fatal("result shares writable containers with an input")
	}
}

func TestContainerCombinationOps(t *testing.T) {
	// Build one array-backed and one bitmap-backed set in the same bucket.
	arr := New()
	for i := uint32(0); i < 100; i++ {
		arr.Add(i * 10) // 0..990, sparse
	}
	dense := New()
	for i := uint32(0); i < 5000; i++ {
		dense.Add(i * 2) // evens 0..9998, bitmap
	}
	if BucketKind(arr, 0) != "array" || BucketKind(dense, 0) != "bitmap" {
		t.Fatal("setup: wrong container kinds")
	}
	arrRef, denseRef := mapRef{}, mapRef{}
	for _, v := range collect(arr) {
		arrRef[v] = struct{}{}
	}
	for _, v := range collect(dense) {
		denseRef[v] = struct{}{}
	}
	check := func(name string, got *Set, want mapRef) {
		t.Helper()
		gotVals := collect(got)
		wantVals := want.sorted()
		if len(gotVals) != len(wantVals) {
			t.Fatalf("%s: got %d values, want %d", name, len(gotVals), len(wantVals))
		}
		for i := range wantVals {
			if gotVals[i] != wantVals[i] {
				t.Fatalf("%s: value[%d] = %d, want %d", name, i, gotVals[i], wantVals[i])
			}
		}
	}
	unionRef := mapRef{}
	for v := range arrRef {
		unionRef[v] = struct{}{}
	}
	for v := range denseRef {
		unionRef[v] = struct{}{}
	}
	interRef := mapRef{}
	for v := range arrRef {
		if _, ok := denseRef[v]; ok {
			interRef[v] = struct{}{}
		}
	}
	diffRef := mapRef{}
	for v := range arrRef {
		if _, ok := denseRef[v]; !ok {
			diffRef[v] = struct{}{}
		}
	}
	diffRef2 := mapRef{}
	for v := range denseRef {
		if _, ok := arrRef[v]; !ok {
			diffRef2[v] = struct{}{}
		}
	}
	check("array ∪ bitmap", Union(arr, dense), unionRef)
	check("bitmap ∪ array", Union(dense, arr), unionRef)
	check("array ∩ bitmap", Intersect(arr, dense), interRef)
	check("bitmap ∩ array", Intersect(dense, arr), interRef)
	check("array \\ bitmap", Difference(arr, dense), diffRef)
	check("bitmap \\ array", Difference(dense, arr), diffRef2)
	check("array ∪ array", Union(arr, setOf(5, 15, 25)), func() mapRef {
		r := mapRef{}
		for v := range arrRef {
			r[v] = struct{}{}
		}
		for _, v := range []uint32{5, 15, 25} {
			r[v] = struct{}{}
		}
		return r
	}())
	check("bitmap ∩ bitmap", Intersect(dense, dense), denseRef)
	check("bitmap \\ bitmap (self)", Difference(dense, dense), mapRef{})
}

// --- randomized cross-check against a map reference ---------------------------

func TestRandomizedAgainstMapReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20260919))
	for trial := 0; trial < 30; trial++ {
		// Small samples: values drawn from a few buckets, sometimes dense.
		span := uint32(1 << (4 + rng.Intn(16)))
		mask := span - 1
		nA, nB := 1+rng.Intn(300), 1+rng.Intn(300)
		a, b := New(), New()
		ra, rb := mapRef{}, mapRef{}
		for i := 0; i < nA; i++ {
			v := rng.Uint32() & mask
			a.Add(v)
			ra[v] = struct{}{}
		}
		for i := 0; i < nB; i++ {
			v := rng.Uint32() & mask
			b.Add(v)
			rb[v] = struct{}{}
		}
		// Occasionally force one bucket dense to hit bitmap paths.
		if trial%3 == 0 {
			for i := uint32(0); i < 4500; i++ {
				v := (uint32(trial) << 16) | (i * 13 % 65536)
				a.Add(v)
				ra[v] = struct{}{}
			}
		}

		checkSet := func(name string, s *Set, ref mapRef) {
			t.Helper()
			if s.Cardinality() != uint64(len(ref)) {
				t.Fatalf("trial %d %s: cardinality %d, want %d", trial, name, s.Cardinality(), len(ref))
			}
			got, want := collect(s), ref.sorted()
			if len(got) != len(want) {
				t.Fatalf("trial %d %s: iterated %d, want %d", trial, name, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("trial %d %s: value[%d] = %d, want %d", trial, name, i, got[i], want[i])
				}
			}
			// Rank and Select against the reference.
			for _, x := range []uint32{0, span / 2, span - 1, math.MaxUint32, rng.Uint32() & mask} {
				if got, want := s.Rank(x), ref.rank(x); got != want {
					t.Fatalf("trial %d %s: Rank(%d) = %d, want %d", trial, name, x, got, want)
				}
			}
			for k := 0; k < len(want); k += 1 + len(want)/7 {
				v, err := s.Select(uint64(k))
				if err != nil || v != want[k] {
					t.Fatalf("trial %d %s: Select(%d) = %d, %v; want %d", trial, name, k, v, err, want[k])
				}
			}
			// Round-trip through serialization.
			back, err := Unmarshal(s.MarshalBinary(), 1<<24)
			if err != nil {
				t.Fatalf("trial %d %s: unmarshal: %v", trial, name, err)
			}
			if !back.Equal(s) {
				t.Fatalf("trial %d %s: round-trip mismatch", trial, name)
			}
		}

		unionRef := mapRef{}
		for v := range ra {
			unionRef[v] = struct{}{}
		}
		for v := range rb {
			unionRef[v] = struct{}{}
		}
		interRef := mapRef{}
		for v := range ra {
			if _, ok := rb[v]; ok {
				interRef[v] = struct{}{}
			}
		}
		diffRef := mapRef{}
		for v := range ra {
			if _, ok := rb[v]; !ok {
				diffRef[v] = struct{}{}
			}
		}
		checkSet("a", a, ra)
		checkSet("a∪b", Union(a, b), unionRef)
		checkSet("a∩b", Intersect(a, b), interRef)
		checkSet("a\\b", Difference(a, b), diffRef)
	}
}

// --- serialization validation -------------------------------------------------

func TestBadSerialization(t *testing.T) {
	good := setOf(1, 2, 3, 1<<16, math.MaxUint32).MarshalBinary()

	cases := map[string]func() []byte{
		"empty input":   func() []byte { return nil },
		"bad magic":     func() []byte { b := append([]byte(nil), good...); b[0] = 'X'; return b },
		"truncated":     func() []byte { return good[:len(good)-1] },
		"trailing junk": func() []byte { return append(append([]byte(nil), good...), 0) },
		"out-of-order buckets": func() []byte {
			// Swap the two bucket records (keys 0x0000 and 0x0001, both arrays).
			b := append([]byte(nil), good...)
			// header 8 bytes; bucket0: 8-byte header + 3*2 payload; bucket1: 8 + 2
			rec0 := append([]byte(nil), b[8:22]...)
			rec1 := append([]byte(nil), b[22:32]...)
			copy(b[8:], rec1)
			copy(b[8+len(rec1):], rec0)
			return b
		},
		"duplicate array value": func() []byte {
			b := append([]byte(nil), good...)
			// First bucket payload starts at offset 16: values 1,2,3 -> make 1,1,3.
			b[18], b[19] = b[16], b[17]
			return b
		},
		"wrong cardinality": func() []byte {
			b := append([]byte(nil), good...)
			b[12] = 4 // first bucket claims 4 elements, payload has 3
			return b
		},
		"non-canonical container": func() []byte {
			// An array bucket claiming 0 elements.
			b := []byte{'A', 'B', 'M', '1', 1, 0, 0, 0}
			b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
			return b
		},
		"bitmap with array-range cardinality": func() []byte {
			b := []byte{'A', 'B', 'M', '1', 1, 0, 0, 0}
			b = append(b, 0, 0, 1, 0)          // key 0, type bitmap
			b = append(b, 100, 0, 0, 0)        // cardinality 100 <= 4096
			return append(b, make([]byte, BitmapBytes)...)
		},
		"bitmap popcount mismatch": func() []byte {
			s := New()
			for i := uint32(0); i < 5000; i++ {
				s.Add(i)
			}
			b := s.MarshalBinary()
			b[12]-- // claim one fewer element than the popcount
			return b
		},
		"unknown container type": func() []byte {
			b := append([]byte(nil), good...)
			b[10] = 9
			return b
		},
		"reserved byte set": func() []byte {
			b := append([]byte(nil), good...)
			b[11] = 1
			return b
		},
	}
	for name, makeBad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Unmarshal(makeBad(), 1<<20); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
	// The good payload must still parse.
	if _, err := Unmarshal(good, 1<<20); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

func TestPayloadLimitEnforcedBeforeAllocation(t *testing.T) {
	s := New()
	for i := uint32(0); i < 5000; i++ {
		s.Add(i) // one bitmap bucket: 8192 payload bytes
	}
	s.Add(1 << 16) // one array bucket: 2 payload bytes
	buf := s.MarshalBinary()
	if _, err := Unmarshal(buf, s.PayloadBytes()); err != nil {
		t.Fatalf("exact limit should pass: %v", err)
	}
	if _, err := Unmarshal(buf, s.PayloadBytes()-1); !errors.Is(err, ErrPayloadLimit) {
		t.Fatalf("limit-1: err = %v, want ErrPayloadLimit", err)
	}
	if _, err := Unmarshal(buf, 0); !errors.Is(err, ErrPayloadLimit) {
		t.Fatalf("limit 0: err = %v, want ErrPayloadLimit", err)
	}
}

// --- payload accounting --------------------------------------------------------

func TestPayloadStats(t *testing.T) {
	s := New()
	if got := s.PayloadBytes(); got != 0 {
		t.Fatalf("empty payload = %d, want 0", got)
	}
	// Array bucket: 3 elements -> 6 bytes.
	for _, v := range []uint32{10, 20, 30} {
		s.Add(v)
	}
	if got := s.PayloadBytes(); got != 6 {
		t.Fatalf("array payload = %d, want 6", got)
	}
	// Add a dense bucket: 5000 elements -> 8192 bytes.
	for i := uint32(0); i < 5000; i++ {
		s.Add((1 << 16) | i)
	}
	if got := s.PayloadBytes(); got != 6+BitmapBytes {
		t.Fatalf("mixed payload = %d, want %d", got, 6+BitmapBytes)
	}
	// Shrink the dense bucket back to an array: 4096 * 2 bytes.
	for i := uint32(4096); i < 5000; i++ {
		s.Remove((1 << 16) | i)
	}
	if got := s.PayloadBytes(); got != 6+4096*2 {
		t.Fatalf("shrunk payload = %d, want %d", got, 6+4096*2)
	}
}

// --- determinism ---------------------------------------------------------------

func TestMarshalDeterministic(t *testing.T) {
	build := func() *Set {
		s := New()
		rng := rand.New(rand.NewSource(7))
		for i := 0; i < 6000; i++ {
			s.Add(rng.Uint32() & 0xFFFFF)
		}
		return s
	}
	a, b := build(), build()
	if string(a.MarshalBinary()) != string(b.MarshalBinary()) {
		t.Fatal("same contents produced different encodings")
	}
}
