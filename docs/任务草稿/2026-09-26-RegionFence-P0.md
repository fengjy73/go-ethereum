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

## Stage 2b（已提交并推送到 PR #1；用户尚未验收）

目标：修好 rf-auto 的跨块先验和 `GOMAXPROCS` 竞态，让自动 C 按依赖前沿爬坡；同时再砍 C=1 的 TxView 开销。同一分支 / PR #1。十块正确性与 race 保持通过。ict21 上的 10% 与 1.30x 目标本机不能代替。

1. **已完成** 执行器不再调用 `runtime.GOMAXPROCS`。进程上限只读一次，活跃 worker 由池的 `SetActiveIf(gen)` 控制。过期的 `Drive` 代际被丢掉。`TestAutoDoesNotChangeGOMAXPROCS`、`TestSetActiveIfIgnoresStaleGen` 通过。
2. **已完成** 结构宽度改为前沿上就绪或正在执行、且没有被同发送者前序挡住、也没有停在围栏上的交易数。尾部收缩和看门狗都不写 `CrewBest`。下一块从体部测到的最佳 C 开始。`TestFrontierWidthCountsReadyHeads`、`TestCrewTailDrainKeepsBest`、`TestAutoPriorSurvivesFixtureTail` 通过。
3. **已完成** 爬坡：两窗基线，横杆取较快的一窗且不被同 C 的慢切片拉低；点估计是整窗 gas/wall；明显更快才 ×2 或 /2，否则立刻回到最佳再 ±1。看门狗只在中止风暴或持续空转时减半，本块不再爬回去，也不改记录的最佳 C。`TestCrewDoublesOnFaster`、`TestCrewSlowerRefines`、`TestCrewNoiseStays`、`TestCrewAbortStorm`、`TestCrewIdleStorm`、`TestCrewRefineTriesOtherSide` 通过。
4. **已完成** C=1 跳过 fence 和 abort 原子读；账户 wipe 只读一次；固定 C=1 的 `ObserveSafeBatch` 延到块末。`TxView` 本来就实现 `vm.StateDB`，没有适配层可删。
5. **已完成（数字未达目标）** `TestFixtureEngines` 在最终策略后通过。`go test -race` 下 `core/rfstate` 1.0s、`core/rfexec` 88.8s，退出码 0。RF C=2 与 C=4 各 K=30、C=8 K=15 在最终策略之前的固定 C 路径上退出码 0（固定 C 不走 crew）。lint 0 issues，`check_baddeps` 通过，`make all` 退出码 0。本机 K=3、`GOMAXPROCS=C`、cpus 0-3 的第二次中位数：rf C=1 / serial 几何平均 1.35（更早一次同 C=1 代码、旧 crew 的几何平均是 1.275，stage 2 提交是 1.31；1.30 没有站住）。rf-auto reset / 每块最佳固定 rf C = 1.19，carry = 1.116，都没有进 10%。22418000 的 reset 从 1 起是 1.66，carry 从 4 起是 1.03。C=1 剖面采样 10ms，TxView / Ledger / sched 的 flat 都在一两格采样里，不能用来声称桶下降；ALLOC 相对 stage 2 剖面略高。

## Stage 2c（已提交 `3f5b0bb48`，待用户验收）

目标：先修 rf-auto 在收缩到 1 时漏掉 coinbase 费用的正确性；再用代价模型选 C，而不是用短窗 wall 爬坡。同一分支 / PR #1，基线 `c7c741970`。十块正确性与 race 保持通过。

1. **已完成** C=1 快路径只在整块固定单 worker（`solo`）时启用。rf-auto 即使活跃数落到 1 也不跳过 fence。
2. **已完成** coinbase 余额读登记为费用合计的读者，提交前再核对费用前缀。去掉修复后 `TestCoinbaseReadSurvivesShrink` 失败（槽值 `0x0de000cd866f8000` 对串行 `0x0de013e6f7f9d000`），恢复后通过。
3. **已完成** 代价模型替换 wall 爬坡。开块曲线写入 CSV。速率用块末实际 gas 的 L，未知交易用 limit×util。尾部不改 `CrewBest`。
4. **已完成** `procGate` 代际匹配后才改 `GOMAXPROCS`，`ExecAuto` 返回前恢复 cap。`TestProcGateRestoresAndIgnoresStale`、`TestAutoDoesNotChangeGOMAXPROCS`。
5. **已完成** `-pin-coordinator` 与 README 里的 ict21 命令。本机只有一个 L3 组，量不出跨 CCX 的 10%。
6. **已完成代码与本机测量** 1000Hz 请求、24 遍的 C=1 剖面；固定 C=1 的冷读不再插入 keyState。压力：rf-auto K=30 两轮（reset/carry）各 300 行，rf C=2/4 K=30 各 300 行，C=8 K=15 共 150 行，逐次检查通过。race 见验证记录。ict21 的 1.39× 和 “CPU>2×” 没有在 ict21 上重测。

## Stage 2d（代码已在本机测过，待用户验收）

目标：按段记账并换掉「只探 2×」的策略，让 rf-auto 的预测包含跨发送者 RAW、固定开销和直接测到的膨胀。同一分支 / PR #1，基线 `3f5b0bb48`。十块正确性保持通过。ict21 上的数字以用户已测的 `3f5b0bb48` 为准，本机不能代替。

1. **已完成** 每个活跃 C 的片段单独记 wall、进程 CPU 和实际 gas。放弃的探针记在它真正跑过的 C 上。尾部不更新速率。前沿窄于 C 的段只记 0.25 个样本，不改 rate。`TestAbandonedProbeUpdatesItsOwnC`、`TestTailSegmentDoesNotMoveRate`。
2. **已完成** 开块用 `T*(1-0.35/sqrt(samples+1))`，而且只给真正缩短关键路径的 C 加探索奖励，纯链不会为了奖励跑到更宽的 C。检查点按后验均值重选。未测到的 C 膨胀保持 1，不从邻居抄一个被夹到 8 的值。`TestExploreNotStuckAtOne`、`TestTerribleInflationIsNotRepicked`。
3. **已完成** 跨发送者 RAW 用收缩后的冲突率做成软链（最重的一笔加上 `p` 乘其余权重），不是把整份合约收成一条发送者链。选择器向 gas limit 收缩。C=1 的大段才写 base 和 infl。`TestHotContractLengthensCriticalPath`、`TestInBlockRAWChainsContract`、`TestSelectorShrinksTowardLimit`、`TestFixedCostOnTinyPrediction`。
4. **已完成** 默认不 `LockOSThread`。当前线程只对第一组 L3 做亲和提示。`-pin-coordinator` 仍可用，并在 stderr 说明 `GOMAXPROCS=C` 时更慢。README 里固定引擎是 `GOMAXPROCS=C+1`，rf-auto 不用 +1。
5. **未改解释器** 22102250 / 26061000 / 22018250 的 C=1 差距没有新的便宜热点。2c 对 26061000 的剖面里 `Ledger.Read` 已经没有了，剩下的是解释器、keccak、jumpdest。本机 K=3 的 rf C=1/serial 是 1.223、1.132、0.860，几何平均 1.137；这不是 ict21，也不是这次砍出来的。

## Stage 2e（代码已在本机测过，待用户验收）

目标：rf-auto 的主信号不再是按段的 infl/base/固定开销回归。跨块用几何网格上的 Thompson sampling 选一个 C，整块体部 wall/gas 记到这个臂上；块内只允许缩小的守卫。围栏只在期望等待短于它省下的重执行时才选 `WAIT_FINAL`。同一分支 / PR #1，基线 `6b1e19701`。ict21 的 10% 目标本机不能代替。

1. **已完成** 臂集合是 cap 以内的 2 的幂，再加上 cap 本身。未试过的臂先验是 `1+0.06*log2(C)^2`（C=4 约 1.24，一次大约 1.4 倍的样本就能翻；C=32 是 2.5）。没有任何样本时不开噪声，第一块是 C=1。奖励是体部 wall/gas，尾部不算，守卫缩小不改臂、不改 `CrewBest`。
2. **已完成** 太小的块（冷启动 `nTx<48`，之后 `gas*serialRate < 2*startupNs`）整块固定 C=1 并走 Solo。守卫只缩小。
3. **已完成** `Choose`：两边都有纳秒样本时，`E[wait] < pc * E[reexec]` 才等待。冷先验保持 PASS。
4. **已完成测量** 本机 K=3 表和 22418000 的计数见验证记录。没有上 ict21。`WAIT_FINAL` 在 rf C=4 上几乎是 0，所以这条差距不是选错了围栏。

结构模型只给先验当特征。十个块不够再按交易数分桶，后验是全体块共用的一份，外加「太小 / 其余」。

## 验证记录

- `go test ./core/rfstate` 通过，含 `TestDropEstimatesRemovesUnpublishedKey`。
- `RF_FIXTURES=/tmp/fixa go test ./core/rfexec -run 'TestFixtureEngines|TestSynthetic|TestLoad'` 通过（约 13s，KZG 预热前）。22418000 串行约 1.87s 来自首次 point-evaluation 的 KZG 初始化，已移到计时区外。
- **2026-09-26 续** fixtures-b（19951808、20058000、20361898、26060000、26061000）并入 `TestFixtureEngines`。十块一次通过；`rfbench -fixtures /tmp/fixa,/tmp/fixb` 退出码 0。Osaka 系统调用沿用已有 Prague pre/post 路径，未改执行器。
- **2026-09-27 Stage 2c** `TestFixtureEngines` 7.2s 通过。`go test -race`：`core/rfstate` 1.0s，`core/rfexec` 86.3s，退出码 0。rf-auto reset/carry K=30 各 300 行，rf C=2/4 K=30 各 300 行，C=8 K=15 共 150 行，逐次对串行通过。本机 K=3 中位数几何平均 rf C=1/serial = 1.213（stage 2b 同机是 1.35；ict21 上一次是 1.39，这次没有上 ict21）。rf-auto reset 相对每块最佳固定 rf C（1/2/4）几何平均 1.02，10 块里 7 块在 10% 内；carry 1.09，5/10。第一块 `model_pred_ns` 为 0。lint 0 issues，`check_baddeps` 通过，`make all` 退出码 0。
- **2026-09-27 Stage 2d** `go test ./core/rfstate ./core/rfexec` 通过（含十块 `TestFixtureEngines`）。`go test -race`：`core/rfstate` 1.0s，`core/rfexec` 81.4s，退出码 0。模型单测覆盖放弃的探针、尾段、探索、热点软链、选择器收缩和固定项。`model_pred_ns` 与开块曲线上 `model_c` 的 60 行全部一致。lint 0 issues，`check_baddeps` 通过。本机 4 核、K=3、`GOMAXPROCS=C`、cpus 为 `0..C-1`、没有 `-pin-coordinator`。相对 serial 的中位数几何平均：occ 1/2/4 = 1.201/0.939/0.903，rf 1/2/4 = 1.137/0.993/0.940，rf-auto reset 1.016，carry 0.927。相对每块最佳固定 rf C：reset 几何平均 1.139（5/10 在 10% 内），carry 1.039（8/10）。预测对 wall 的块中位误差中位数：reset -5%，carry -21%；20361898 仍约 -70%（它排在学会固定项之前），22018250 carry +65%。没有上 ict21。
- **2026-09-27 Stage 2e** 网格、悲观先验、守卫不改臂、尾部不进奖励、1.4 倍样本能采纳 C=4、以及「等待不便宜就 PASS」的单测通过。十块 `TestFixtureEngines` 通过。`go test -race`：`core/rfstate` 1.0s，`core/rfexec` 87.5s。lint 0 issues，`check_baddeps` 通过，`make all` 退出码 0。本机 4 核、K=3、`GOMAXPROCS=C`、cpus `0..C-1`、没有 `-pin-coordinator`，固定 rf 用 `-prior carry`。相对 serial 的中位数几何平均：occ 1/2/4 = 1.332/1.047/0.987，rf 1/2/4 = 1.254/1.098/1.019，rf-auto reset 1.131，carry 1.257。相对每块最佳固定 rf C：reset 几何平均 1.172（4/10 在 10% 内），carry 1.302（2/10）。第一块没有样本，臂是 1。其后 reset 多数块的臂是 4；20361898（27 笔）保持 1。22418000 上 rf C=4 的 `wait_final` 中位数是 0（三次是 1/0/0），occ C=4 是约 100 次估计等待。rf 的执行和回滚都更少，wall 中位数仍是 38ms 对 occ 的 33ms（occ/rf ≈ 0.88）。剖面里 `fence` 只占大约 2% 的样本，主要时间在解释器。没有上 ict21。
