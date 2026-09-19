# 自适应压缩位图与秩选择库

离线检索索引需要压缩保存文档ID，并执行集合运算与排名定位。本库实现 uint32 有序集合：按高 16 位分桶，桶内基数 ≤ 4096 时用升序 uint16 数组，否则用 1024 个 uint64 位图，更新与集合运算后按阈值自动转换，桶按高位键排序。

## 接口

包 `github.com/382868331/gsb-adaptive-bitmap-20260919`（包名 `bitmap`）：

- `New() *Bitmap`；`Add(x) bool`、`Remove(x) bool`（原地单元素增删，返回是否变化）；`Contains(x) bool`
- `Union(o)`、`Intersect(o)`、`Difference(o)`：返回新集合，不改写也不与输入共享可写容器
- `Iterator()` / `Next() (uint32, bool)`：递增迭代
- `Rank(x) uint64`：≤ x 的元素个数（利用桶基数与字级 popcount，不扫描 uint32 域）
- `Select(k) (uint32, error)`：0 起始第 k 小；越界返回 `ErrSelectOutOfRange`
- `Cardinality() uint64`、`PayloadBytes() uint64`（载荷统计：数组元素数×2 或位图 8192 字节，非 Go 进程内存）、`BucketStats()`、`Clone()`

## 二进制格式

`Save(w io.Writer)` / `Load(r io.Reader, maxPayloadBytes uint64)`，全部小端：

```
4B  魔数 "ABM1"
4B  uint32 桶数量 N
×N  桶（高位键严格递增）：
    2B uint16 高位键
    4B uint32 桶内基数
    1B 容器类型（0=数组，1=位图）
    载荷：数组 card×2 字节（uint16 严格递增）/ 位图 8192 字节
```

`Load` 拒绝：重复/乱序桶键、重复数组值、基数与内容不符、截断、非规范容器（数组 >4096、位图 ≤4096、空桶、未知类型）。累计载荷在分配前按 `maxPayloadBytes` 校验，超限即拒绝。

## 运行

Windows 原生 Go 1.26.5，仅标准库，无第三方依赖、无网络。

```
go run ./cmd/demo                        # 演示：表示转换、跨桶 Rank/Select、集合运算、序列化往返、实际触发的失败
go test ./... -count=1 -timeout=60s      # 测试
```

测试覆盖：4096/4097 往返、0 与 MaxUint32、空集、跨桶 Rank/Select、运算输入不变、坏序列化各情形、载荷统计，以及固定种子 map 参考的集合代数与 Rank/Select 随机验证。
