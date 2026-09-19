# 自适应压缩位图与秩选择库

离线检索索引需要压缩保存文档ID，并执行集合运算与排名定位。完整规格见 TASK.md。

uint32 集合按高 16 位分桶，桶按高位升序排列。每个桶内低 16 位的表示自适应切换：

- 基数 ≤ 4096：升序 uint16 数组；
- 基数 > 4096：1024 个 uint64（8192 字节）位图。

Add/Remove 及集合运算后按阈值就地转换，空桶自动删除。Rank/Select 利用桶基数与字级 popcount，不扫描整个 uint32 域；集合运算按数组/位图四种组合分别实现，不展开成整数 map。

## 接口

```go
s := bitmap.New()
s.Add(x) bool            // 单元素原地增删
s.Remove(x) bool
s.Contains(x) bool
s.Cardinality() uint64
s.Rank(x) uint64         // 元素 <= x 的数量
s.Select(k) (uint32, error) // 0 起始；越界返回 ErrSelectOutOfRange
it := s.Iterator()       // 递增迭代：v, ok := it.Next()

u := bitmap.Union(a, b)      // 均返回新集合，不改写输入、不共享可写容器
i := bitmap.Intersect(a, b)
d := bitmap.Difference(a, b)

buf := s.MarshalBinary()                 // 确定性二进制编码
s2, err := bitmap.Unmarshal(buf, maxPayloadBytes) // 严格校验 + 载荷上限
s.PayloadBytes() uint64                  // 载荷统计（见下）
bitmap.BucketKind(s, hi) string          // 诊断用：array / bitmap / absent
```

## 二进制格式

全部小端序，同一集合编码唯一：

| 偏移 | 内容 |
|---|---|
| 0 | 魔数 `ABM1`（4 字节） |
| 4 | uint32 桶数量 N |
| 8 | N 条桶记录，高 16 位键严格递增 |

每条桶记录：uint16 键、uint8 类型（0=数组，1=位图）、uint8 保留（须为 0）、uint32 基数，随后是载荷——数组为 `基数` 个严格递增的 uint16，位图为 1024 个 uint64（8192 字节）。

`Unmarshal` 拒绝：重复/乱序桶、重复或乱序数组值、错误基数（数组须 1..4096、位图须 >4096 且与 popcount 一致）、截断、尾随字节、非规范容器。每个容器载荷在分配前按调用者给定的 `maxPayloadBytes` 累计检查，超限返回 `ErrPayloadLimit`。

**载荷统计口径**：固定为数组元素数 × 2 字节或位图 8192 字节，是序列化载荷的逻辑字节数，不是 Go 进程内存占用。

## 环境与命令

Windows 原生 Go 1.26.5，仅标准库，无第三方依赖、外部服务或 Docker。

演示（约 8 秒内完成，含正常结果与实际触发的失败）：

```
go run ./cmd/demo
```

测试：

```
go test ./... -count=1 -timeout=60s
```

测试覆盖：4096/4097 表示往返、0 与 MaxUint32、空集、跨桶 Rank/Select、运算输入不变、各类坏序列化、载荷统计与上限、固定种子小样本对照 map 参考的集合代数与 Rank/Select 验证。
