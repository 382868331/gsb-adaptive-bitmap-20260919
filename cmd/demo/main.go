// 演示：稀疏/稠密表示转换、跨桶 Rank/Select、集合运算、序列化往返，
// 以及两个实际触发的失败（Select 越界、坏序列化被拒）。
package main

import (
	"bytes"
	"fmt"
	"time"

	bitmap "github.com/382868331/gsb-adaptive-bitmap-20260919"
)

func main() {
	start := time.Now()

	fmt.Println("== 1. 稀疏到稠密的表示转换 ==")
	b := bitmap.New()
	for i := uint32(0); i < 4096; i++ {
		b.Add(i)
	}
	a, bm := b.BucketStats()
	fmt.Printf("加入 4096 个元素: 数组桶=%d 位图桶=%d 载荷=%d 字节\n", a, bm, b.PayloadBytes())
	b.Add(4096)
	a, bm = b.BucketStats()
	fmt.Printf("加入第 4097 个元素: 数组桶=%d 位图桶=%d 载荷=%d 字节（超过阈值，数组→位图）\n", a, bm, b.PayloadBytes())
	b.Remove(4096)
	a, bm = b.BucketStats()
	fmt.Printf("删回 4096 个元素: 数组桶=%d 位图桶=%d（降回阈值，位图→数组）\n", a, bm)

	fmt.Println("\n== 2. 跨桶 Rank / Select ==")
	s := bitmap.New()
	for _, v := range []uint32{0, 5, 65535, 65536, 65537, 200000, 1 << 20, 1<<32 - 1} {
		s.Add(v)
	}
	fmt.Printf("集合基数 = %d\n", s.Cardinality())
	fmt.Printf("Rank(65536) = %d（<=65536 的元素个数）\n", s.Rank(65536))
	fmt.Printf("Rank(MaxUint32) = %d\n", s.Rank(1<<32-1))
	for k := uint64(0); k < s.Cardinality(); k++ {
		v, _ := s.Select(k)
		fmt.Printf("Select(%d) = %d\n", k, v)
	}

	fmt.Println("\n== 3. 集合运算（返回新集合，输入不变） ==")
	x := bitmap.New()
	y := bitmap.New()
	for i := uint32(0); i < 5000; i++ { // x 的 0 号桶会变稠密
		x.Add(i)
	}
	for i := uint32(2500); i < 7500; i++ {
		y.Add(i)
	}
	y.Add(1 << 20) // y 多一个跨桶元素
	fmt.Printf("|x|=%d |y|=%d |x∪y|=%d |x∩y|=%d |x-y|=%d\n",
		x.Cardinality(), y.Cardinality(),
		x.Union(y).Cardinality(), x.Intersect(y).Cardinality(), x.Difference(y).Cardinality())
	fmt.Printf("运算后 |x|=%d |y|=%d（输入未被改写）\n", x.Cardinality(), y.Cardinality())

	fmt.Println("\n== 4. 序列化往返 ==")
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		panic(err)
	}
	back, err := bitmap.Load(bytes.NewReader(buf.Bytes()), 1<<20)
	if err != nil {
		panic(err)
	}
	fmt.Printf("保存 %d 字节，读回基数 %d，Rank(65536)=%d（与原始一致）\n",
		buf.Len(), back.Cardinality(), back.Rank(65536))

	fmt.Println("\n== 5. 实际触发的失败 ==")
	if _, err := s.Select(s.Cardinality()); err != nil {
		fmt.Printf("Select 越界被拒绝: %v\n", err)
	}
	corrupt := append([]byte{}, buf.Bytes()...)
	// 第一个桶的数组从偏移 15 开始（8 字节头 + 7 字节桶头），
	// 值 5 的低字节在偏移 17，改成 0 制造重复数组值。
	corrupt[17] = 0
	if _, err := bitmap.Load(bytes.NewReader(corrupt), 1<<20); err != nil {
		fmt.Printf("坏序列化被拒绝: %v\n", err)
	}
	if _, err := bitmap.Load(bytes.NewReader(buf.Bytes()), 3); err != nil {
		fmt.Printf("载荷超限被拒绝: %v\n", err)
	}

	fmt.Printf("\n演示完成，耗时 %v\n", time.Since(start).Round(time.Millisecond))
}
