# RegionFence P0

## 目标

在 go-ethereum master 上实现 RegionFence 研究原型 P0 与评测工具：夹具装载、串行基线、Block-STM 式 OCC、RegionFence（PASS / WAIT_FINAL / WAIT_PREFIX / DEFER_TX）、跨块 Beta 先验、固定 C 的常驻绑核 worker、CSV 计时与状态/收据核对。

## 约束

- 不改现有串行 / BAL 并行处理器。
- 不做 delta / 可交换更新，不用 EIP-7928 BAL。
- 正确性不靠读集重验证（OCC 基线除外）。
- 计时只含执行（含系统调用），不含装载、状态根、Commit。KZG 上下文在计时前初始化。
- 基座用 master：PR 目标是 master，且主网配置已覆盖 Cancun 到 Osaka。

## 完成标准

- fixtures-a 与 fixtures-b 共十个块上 serial 与 receipts 一致。
- occ 与 rf 在 C=1,2,4,8 上最终状态与收据相对 serial 一致。
- 其中 26060000、26061000 为 Osaka/BPO2。
- `cmd/rfbench` 可输出 CSV。
- 账本/围栏有单元测试；子集可跑 race。

## 步骤

1. **已完成** 设计阅读与执行路径核对（`StateDB`、`ApplyMessage`、夹具 JSON 形状）。
2. **已完成** `core/rfstate`：多版本账本、TxView、学习器、绑核池。`DropEstimates` 清掉未重写的 ESTIMATE。
3. **已完成** `core/rfexec`：夹具、串行、OCC、RegionFence、核对。
4. **已完成** `cmd/rfbench` 与 README。
5. **已完成** 单测、五块夹具 C=1,2,4,8、`go test -race`、`rfbench` CSV。golangci-lint 对新包 0 issues，`check_baddeps` 通过。
6. **已完成** 提交 `0ff2f1509` 已推送。草稿 PR：https://github.com/fengjy73/go-ethereum/pull/1

## Stage 1b

目标：砍掉单线程开销和多核争用，仍走同一分支 / PR #1。串行路径与十块正确性矩阵（C=1/2/4/8，含 race）保持通过。

1. **已完成** 读写集限定 DropEstimates / RemoveReaders / Retract / MarkEstimate。键索引按 GOMAXPROCS 分片。
2. **已完成** 代码哈希随账户和代码版本携带，代码字节共享，不在加载时重算 keccak。
3. **已完成** 每个 worker 复用一个 EVM；块内共享 `core.NewJumpDestCache`。
4. **已完成** TxView 池化；系统调用视图用完归还。余额在加载时拷一次，`GetBalance` 返回该指针。
5. **已完成** 空闲 worker 在条件变量上等待。池大小等于最大 C。已设置的 `GOMAXPROCS` 不被覆盖。
6. **已完成** carry 来自计时并行跑的冲突，并按 33/34 衰减。未围栏的键不再为了 `LowerProducer` 再锁一次键。
7. **已完成** 十块正确性与 `go test -race` 通过（rfexec 约 78s）。本机 `GOMAXPROCS=C`、K=3 中位数：C=1 相对串行几何平均约 1.49x（目标 1.3x，未达到）；十个块的 rf(C=4) 中位数都快于 rf(C=1)。分配从约 150–520 MiB/块降到约 13–25 MiB。

## Stage 2（进行中）

目标：C=1 开销降到串行的 1.3 倍以内；学习得到的活跃 worker 数；热点键在便宜时提前发布。同一分支 / PR #1。十块正确性与 race 保持通过。

1. **已完成（待测）** 尝试内读缓存；费用前缀和；`pickLocked` 从 frontier 起；最终化时 `ReadKeys` + `ObserveSafeBatch`（只给已有后验的键加 beta）；`Store.peek` 不拷贝账户。
2. **已完成** `crew`：结构宽度是尚未 final 的不同发送者数；爬坡起点是跨块先验（不是上限）；量子是第一笔完成交易的 gas；变慢则退回最佳试验并停止（不再向反方向探一步）；aborts>completions 或空转超过 `(active-1)*wall` 时缩小，并且本块不再爬回去。`Step` 在 `worker >= active` 时返回。`LayoutCPUs` 按最高级 cache 的 `shared_cpu_list` 分组，不把组大小写死成 8。
3. **已完成** `v.early` 只在 `ModeRF` 且 `Parallel()` 时为真。围栏等待、前缀等待和同一发送者停靠会 `retract`，因为 signal panic 不跑 journal。自毁或清空账户时撤掉本交易提前发布的槽。
4. **已完成** 十块 `TestFixtureEngines` 与 `go test -race`（rfexec 约 81s）通过。RF C=2 与 C=4 各 K=30、C=8 K=15 退出码 0。本机 K=3、`GOMAXPROCS=C` 的中位数几何平均：rf C=1 / serial = 1.31（目标 1.30，差 0.01）。剖面与表写在 PR #1。

## 验证记录

- `go test ./core/rfstate` 通过，含 `TestDropEstimatesRemovesUnpublishedKey`。
- `RF_FIXTURES=/tmp/fixa go test ./core/rfexec -run 'TestFixtureEngines|TestSynthetic|TestLoad'` 通过（约 13s，KZG 预热前）。22418000 串行约 1.87s 来自首次 point-evaluation 的 KZG 初始化，已移到计时区外。
- **2026-09-26 续** fixtures-b（19951808、20058000、20361898、26060000、26061000）并入 `TestFixtureEngines`。十块一次通过；`rfbench -fixtures /tmp/fixa,/tmp/fixb` 退出码 0。Osaka 系统调用沿用已有 Prague pre/post 路径，未改执行器。
