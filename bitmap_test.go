package bitmap

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func collect(b *Bitmap) []uint32 {
	var out []uint32
	it := b.Iterator()
	for v, ok := it.Next(); ok; v, ok = it.Next() {
		out = append(out, v)
	}
	return out
}

func fromSlice(vals []uint32) *Bitmap {
	b := New()
	for _, v := range vals {
		b.Add(v)
	}
	return b
}

func TestThreshold4096And4097(t *testing.T) {
	b := New()
	for i := uint32(0); i < 4096; i++ {
		b.Add(i)
	}
	if a, bm := b.BucketStats(); a != 1 || bm != 0 {
		t.Fatalf("4096 elements: want array container, got arrays=%d bitmaps=%d", a, bm)
	}
	if b.Cardinality() != 4096 {
		t.Fatalf("cardinality = %d", b.Cardinality())
	}
	b.Add(4096)
	if a, bm := b.BucketStats(); a != 0 || bm != 1 {
		t.Fatalf("4097 elements: want bitmap container, got arrays=%d bitmaps=%d", a, bm)
	}
	for i := uint32(0); i <= 4096; i++ {
		if !b.Contains(i) {
			t.Fatalf("missing %d after conversion", i)
		}
	}
	// 降回 4096 应切回数组。
	b.Remove(4096)
	if a, bm := b.BucketStats(); a != 1 || bm != 0 {
		t.Fatalf("back to 4096: want array container, got arrays=%d bitmaps=%d", a, bm)
	}
	if b.Cardinality() != 4096 || b.Contains(4096) {
		t.Fatalf("after remove: cardinality=%d contains(4096)=%v", b.Cardinality(), b.Contains(4096))
	}
}

func TestZeroAndMaxUint32(t *testing.T) {
	b := fromSlice([]uint32{0, 1<<32 - 1})
	if !b.Contains(0) || !b.Contains(1<<32-1) {
		t.Fatal("0 or MaxUint32 missing")
	}
	if got := b.Rank(0); got != 1 {
		t.Fatalf("Rank(0) = %d, want 1", got)
	}
	if got := b.Rank(1<<32 - 1); got != 2 {
		t.Fatalf("Rank(MaxUint32) = %d, want 2", got)
	}
	v, err := b.Select(1)
	if err != nil || v != 1<<32-1 {
		t.Fatalf("Select(1) = %d, %v", v, err)
	}
	b.Remove(0)
	b.Remove(1<<32 - 1)
	if b.Cardinality() != 0 {
		t.Fatalf("cardinality after removes = %d", b.Cardinality())
	}
	if a, bm := b.BucketStats(); a != 0 || bm != 0 {
		t.Fatalf("empty set should have no containers, got arrays=%d bitmaps=%d", a, bm)
	}
}

func TestEmptySet(t *testing.T) {
	b := New()
	if b.Cardinality() != 0 || b.Rank(12345) != 0 {
		t.Fatal("empty set should have cardinality and rank 0")
	}
	if _, err := b.Select(0); err == nil {
		t.Fatal("Select(0) on empty set should fail")
	}
	if got := collect(b); len(got) != 0 {
		t.Fatalf("empty set iterated %d values", len(got))
	}
	if b.Contains(0) || b.Remove(0) {
		t.Fatal("empty set should not contain/remove 0")
	}
}

func TestCrossBucketRankSelect(t *testing.T) {
	vals := []uint32{0, 5, 65535, 65536, 65537, 200000, 1 << 20, 1<<32 - 1}
	b := fromSlice(vals)
	for i, v := range vals {
		if got := b.Rank(v); got != uint64(i+1) {
			t.Fatalf("Rank(%d) = %d, want %d", v, got, i+1)
		}
		got, err := b.Select(uint64(i))
		if err != nil || got != v {
			t.Fatalf("Select(%d) = %d, %v, want %d", i, got, err, v)
		}
	}
	if got := b.Rank(100000); got != 5 {
		t.Fatalf("Rank(100000) = %d, want 5", got)
	}
	if _, err := b.Select(uint64(len(vals))); err == nil {
		t.Fatal("Select past end should fail")
	}
	if got := collect(b); !equalU32(got, vals) {
		t.Fatalf("iteration = %v, want %v", got, vals)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOpsInputsUnchanged(t *testing.T) {
	a := fromSlice([]uint32{1, 2, 3, 65536, 65537})
	b := fromSlice([]uint32{3, 4, 65537, 70000})
	// 让 a 的一个桶变成位图，覆盖位图分支。
	for i := uint32(0); i < 5000; i++ {
		a.Add(100<<16 | i)
	}
	snapA, snapB := snapshot(t, a), snapshot(t, b)

	u := a.Union(b)
	if u.Cardinality() != 5007 {
		t.Fatalf("union cardinality = %d, want 5007", u.Cardinality())
	}
	if got := a.Intersect(b).Cardinality(); got != 2 {
		t.Fatalf("intersect cardinality = %d, want 2", got)
	}
	if got := a.Difference(b).Cardinality(); got != 5003 {
		t.Fatalf("difference cardinality = %d, want 5003", got)
	}
	if got := b.Difference(a).Cardinality(); got != 2 {
		t.Fatalf("reverse difference cardinality = %d, want 2", got)
	}
	if snapshot(t, a) != snapA || snapshot(t, b) != snapB {
		t.Fatal("set operation mutated an input")
	}
	// 结果与输入不得共享可写容器：改结果不影响输入。
	u.Add(999999)
	if a.Contains(999999) || b.Contains(999999) {
		t.Fatal("result shares writable container with input")
	}
}

func snapshot(t *testing.T, b *Bitmap) string {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Save(&buf); err != nil {
		t.Fatal(err)
	}
	return string(buf.Bytes())
}

func TestSerializationRoundTrip4097(t *testing.T) {
	b := New()
	for i := uint32(0); i < 4097; i++ {
		b.Add(i)
	}
	b.Add(1<<32 - 1)
	var buf bytes.Buffer
	if err := b.Save(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := Load(bytes.NewReader(buf.Bytes()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a, bm := back.BucketStats(); a != 1 || bm != 1 {
		t.Fatalf("representation not preserved: arrays=%d bitmaps=%d", a, bm)
	}
	if back.Cardinality() != 4098 {
		t.Fatalf("cardinality = %d", back.Cardinality())
	}
	if !equalU32(collect(b), collect(back)) {
		t.Fatal("round trip changed contents")
	}
	// 再次序列化应逐字节一致（确定性）。
	var buf2 bytes.Buffer
	if err := back.Save(&buf2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Fatal("serialization not deterministic")
	}
}

func TestBadSerialization(t *testing.T) {
	good := New()
	for i := uint32(0); i < 10; i++ {
		good.Add(i)
	}
	var buf bytes.Buffer
	if err := good.Save(&buf); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	cases := map[string][]byte{
		"truncated header":  raw[:3],
		"truncated payload": raw[:len(raw)-1],
		"bad magic":         withByte(raw, 0, 'X'),
	}
	// 重复桶键：两个相同键的桶。
	dup := append(append([]byte{}, raw...), raw[8:]...)
	dup[4] = 2 // bucket count = 2
	cases["duplicate bucket key"] = dup
	// 乱序桶：两个桶，第二个键更小。
	unord := buildRaw(t, [][3]uint32{{5, 1, 0}, {3, 1, 0}}, [][]uint16{{7}, {9}})
	cases["unordered bucket keys"] = unord
	// 数组内重复值。
	dupVal := buildRaw(t, [][3]uint32{{0, 2, 0}}, [][]uint16{{4, 4}})
	cases["duplicate array value"] = dupVal
	// 基数错误：头部写 3，实际 2 个值（会在下一桶头部处失败或载荷错位）——直接构造非规范基数更明确。
	badCard := buildRaw(t, [][3]uint32{{0, 5000, 0}}, [][]uint16{asc(5000)})
	cases["array cardinality over threshold"] = badCard
	// 位图基数低于阈值（非规范）。
	lowBm := buildRaw(t, [][3]uint32{{0, 100, 1}}, [][]uint16{nil})
	cases["bitmap cardinality under threshold"] = lowBm
	// 位图基数与实际 popcount 不符。
	wrongPop := buildRaw(t, [][3]uint32{{0, 5000, 1}}, [][]uint16{nil})
	cases["bitmap cardinality mismatch"] = wrongPop
	// 未知容器类型。
	badKind := buildRaw(t, [][3]uint32{{0, 1, 7}}, [][]uint16{{1}})
	cases["unknown container kind"] = badKind

	for name, data := range cases {
		if _, err := Load(bytes.NewReader(data), 1<<20); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
	// 载荷上限：10 个数组元素 = 20 字节，限 19 应拒绝。
	if _, err := Load(bytes.NewReader(raw), 19); err == nil {
		t.Error("payload limit: expected error")
	}
	if _, err := Load(bytes.NewReader(raw), 20); err != nil {
		t.Errorf("payload limit exact: %v", err)
	}
}

func withByte(b []byte, i int, v byte) []byte {
	out := append([]byte{}, b...)
	out[i] = v
	return out
}

func asc(n int) []uint16 {
	out := make([]uint16, n)
	for i := range out {
		out[i] = uint16(i)
	}
	return out
}

// buildRaw 手工构造序列化字节；kind==1 时写全零位图（popcount=0）。
func buildRaw(t *testing.T, hdrs [][3]uint32, arrs [][]uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{'A', 'B', 'M', 1})
	var tmp [8]byte
	putU32 := func(v uint32) {
		tmp[0] = byte(v)
		tmp[1] = byte(v >> 8)
		tmp[2] = byte(v >> 16)
		tmp[3] = byte(v >> 24)
		buf.Write(tmp[:4])
	}
	putU16 := func(v uint16) { tmp[0] = byte(v); tmp[1] = byte(v >> 8); buf.Write(tmp[:2]) }
	putU32(uint32(len(hdrs)))
	for i, h := range hdrs {
		putU16(uint16(h[0]))
		putU32(h[1])
		buf.WriteByte(byte(h[2]))
		if h[2] == 0 {
			for _, v := range arrs[i] {
				putU16(v)
			}
		} else {
			buf.Write(make([]byte, bitmapPayloadBytes))
		}
	}
	return buf.Bytes()
}

func TestPayloadBytes(t *testing.T) {
	b := New()
	for i := uint32(0); i < 100; i++ {
		b.Add(i)
	}
	if got := b.PayloadBytes(); got != 200 {
		t.Fatalf("array payload = %d, want 200", got)
	}
	for i := uint32(100); i <= 4096; i++ {
		b.Add(i)
	}
	if got := b.PayloadBytes(); got != bitmapPayloadBytes {
		t.Fatalf("bitmap payload = %d, want %d", got, bitmapPayloadBytes)
	}
	b2 := New()
	b2.Add(0)
	b2.Add(1 << 20) // 第二个桶
	if got := b2.PayloadBytes(); got != 4 {
		t.Fatalf("two single-element buckets payload = %d, want 4", got)
	}
}

// TestMapReference 用 map 参考实现验证小样本集合代数与 Rank/Select。
func TestMapReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20260919))
	for trial := 0; trial < 30; trial++ {
		var aVals, bVals []uint32
		switch trial % 3 {
		case 0: // 稀疏跨桶
			aVals = randVals(rng, 60, 1<<22)
			bVals = randVals(rng, 60, 1<<22)
		case 1: // 同桶稠密（触发位图）
			base := uint32(rng.Intn(1<<16)) << 16
			aVals = randVals(rng, 300, 6000)
			bVals = randVals(rng, 300, 6000)
			for i := range aVals {
				aVals[i] += base
			}
			for i := range bVals {
				bVals[i] += base
			}
		default: // 混合
			aVals = append(randVals(rng, 40, 1<<20), ascRange(70000, 75000)...)
			bVals = append(randVals(rng, 40, 1<<20), ascRange(72000, 76000)...)
		}
		checkAgainstMap(t, aVals, bVals)
	}
}

func ascRange(lo, hi uint32) []uint32 {
	var out []uint32
	for v := lo; v < hi; v++ {
		out = append(out, v)
	}
	return out
}

func randVals(rng *rand.Rand, n int, mod uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = rng.Uint32() % mod
	}
	return out
}

func checkAgainstMap(t *testing.T, aVals, bVals []uint32) {
	t.Helper()
	a, b := fromSlice(aVals), fromSlice(bVals)
	ma, mb := toMap(aVals), toMap(bVals)

	checkSet := func(name string, got *Bitmap, want map[uint32]struct{}) {
		t.Helper()
		if got.Cardinality() != uint64(len(want)) {
			t.Fatalf("%s cardinality = %d, want %d", name, got.Cardinality(), len(want))
		}
		sorted := make([]uint32, 0, len(want))
		for v := range want {
			sorted = append(sorted, v)
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		if !equalU32(collect(got), sorted) {
			t.Fatalf("%s iteration mismatch", name)
		}
		// Rank/Select 抽查。
		for _, probe := range []uint32{0, 1, 65535, 65536, 1 << 20, 1<<32 - 1} {
			var wantRank uint64
			for _, v := range sorted {
				if v <= probe {
					wantRank++
				}
			}
			if r := got.Rank(probe); r != wantRank {
				t.Fatalf("%s Rank(%d) = %d, want %d", name, probe, r, wantRank)
			}
		}
		for _, k := range []uint64{0, uint64(len(sorted)) / 2, uint64(len(sorted)) - 1} {
			if len(sorted) == 0 {
				break
			}
			v, err := got.Select(k)
			if err != nil || v != sorted[k] {
				t.Fatalf("%s Select(%d) = %d, %v, want %d", name, k, v, err, sorted[k])
			}
		}
		if _, err := got.Select(uint64(len(sorted))); err == nil {
			t.Fatalf("%s Select(cardinality) should fail", name)
		}
	}

	checkSet("union", a.Union(b), unionMap(ma, mb))
	checkSet("intersect", a.Intersect(b), intersectMap(ma, mb))
	checkSet("difference", a.Difference(b), differenceMap(ma, mb))
	checkSet("reverse difference", b.Difference(a), differenceMap(mb, ma))
}

func toMap(vals []uint32) map[uint32]struct{} {
	m := make(map[uint32]struct{}, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

func unionMap(a, b map[uint32]struct{}) map[uint32]struct{} {
	out := make(map[uint32]struct{}, len(a)+len(b))
	for v := range a {
		out[v] = struct{}{}
	}
	for v := range b {
		out[v] = struct{}{}
	}
	return out
}

func intersectMap(a, b map[uint32]struct{}) map[uint32]struct{} {
	out := make(map[uint32]struct{})
	for v := range a {
		if _, ok := b[v]; ok {
			out[v] = struct{}{}
		}
	}
	return out
}

func differenceMap(a, b map[uint32]struct{}) map[uint32]struct{} {
	out := make(map[uint32]struct{})
	for v := range a {
		if _, ok := b[v]; !ok {
			out[v] = struct{}{}
		}
	}
	return out
}
