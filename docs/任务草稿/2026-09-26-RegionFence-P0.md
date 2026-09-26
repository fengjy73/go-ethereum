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

## Stage 2b（代码已验证，待提交；用户尚未验收）

目标：修好 rf-auto 的跨块先验和 `GOMAXPROCS` 竞态，让自动 C 按依赖前沿爬坡；同时再砍 C=1 的 TxView 开销。同一分支 / PR #1。十块正确性与 race 保持通过。ict21 上的 10% 与 1.30x 目标本机不能代替。

1. **已完成** 执行器不再调用 `runtime.GOMAXPROCS`。进程上限只读一次，活跃 worker 由池的 `SetActiveIf(gen)` 控制。过期的 `Drive` 代际被丢掉。`TestAutoDoesNotChangeGOMAXPROCS`、`TestSetActiveIfIgnoresStaleGen` 通过。
2. **已完成** 结构宽度改为前沿上就绪或正在执行、且没有被同发送者前序挡住、也没有停在围栏上的交易数。尾部收缩和看门狗都不写 `CrewBest`。下一块从体部测到的最佳 C 开始。`TestFrontierWidthCountsReadyHeads`、`TestCrewTailDrainKeepsBest`、`TestAutoPriorSurvivesFixtureTail` 通过。
3. **已完成** 爬坡：两窗基线，横杆取较快的一窗且不被同 C 的慢切片拉低；点估计是整窗 gas/wall；明显更快才 ×2 或 /2，否则立刻回到最佳再 ±1。看门狗只在中止风暴或持续空转时减半，本块不再爬回去，也不改记录的最佳 C。`TestCrewDoublesOnFaster`、`TestCrewSlowerRefines`、`TestCrewNoiseStays`、`TestCrewAbortStorm`、`TestCrewIdleStorm`、`TestCrewRefineTriesOtherSide` 通过。
4. **已完成** C=1 跳过 fence 和 abort 原子读；账户 wipe 只读一次；固定 C=1 的 `ObserveSafeBatch` 延到块末。`TxView` 本来就实现 `vm.StateDB`，没有适配层可删。
5. **已完成（数字未达目标）** `TestFixtureEngines` 在最终策略后通过。`go test -race` 下 `core/rfstate` 1.0s、`core/rfexec` 88.8s，退出码 0。RF C=2 与 C=4 各 K=30、C=8 K=15 在最终策略之前的固定 C 路径上退出码 0（固定 C 不走 crew）。lint 0 issues，`check_baddeps` 通过，`make all` 退出码 0。本机 K=3、`GOMAXPROCS=C`、cpus 0-3 的第二次中位数：rf C=1 / serial 几何平均 1.35（更早一次同 C=1 代码、旧 crew 的几何平均是 1.275，stage 2 提交是 1.31；1.30 没有站住）。rf-auto reset / 每块最佳固定 rf C = 1.19，carry = 1.116，都没有进 10%。22418000 的 reset 从 1 起是 1.66，carry 从 4 起是 1.03。C=1 剖面采样 10ms，TxView / Ledger / sched 的 flat 都在一两格采样里，不能用来声称桶下降；ALLOC 相对 stage 2 剖面略高。

## 验证记录

- `go test ./core/rfstate` 通过，含 `TestDropEstimatesRemovesUnpublishedKey`。
- `RF_FIXTURES=/tmp/fixa go test ./core/rfexec -run 'TestFixtureEngines|TestSynthetic|TestLoad'` 通过（约 13s，KZG 预热前）。22418000 串行约 1.87s 来自首次 point-evaluation 的 KZG 初始化，已移到计时区外。
- **2026-09-26 续** fixtures-b（19951808、20058000、20361898、26060000、26061000）并入 `TestFixtureEngines`。十块一次通过；`rfbench -fixtures /tmp/fixa,/tmp/fixb` 退出码 0。Osaka 系统调用沿用已有 Prague pre/post 路径，未改执行器。
