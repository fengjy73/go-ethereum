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

- 五个夹具上 serial 与 receipts 一致。
- occ 与 rf 在 C=1,2,4,8 上最终状态与收据相对 serial 一致。
- `cmd/rfbench` 可输出 CSV。
- 账本/围栏有单元测试；子集可跑 race。

## 步骤

1. **已完成** 设计阅读与执行路径核对（`StateDB`、`ApplyMessage`、夹具 JSON 形状）。
2. **已完成** `core/rfstate`：多版本账本、TxView、学习器、绑核池。`DropEstimates` 清掉未重写的 ESTIMATE。
3. **已完成** `core/rfexec`：夹具、串行、OCC、RegionFence、核对。
4. **已完成** `cmd/rfbench` 与 README。
5. **已完成** 单测、五块夹具 C=1,2,4,8、`go test -race`、`rfbench` CSV。golangci-lint 对新包 0 issues，`check_baddeps` 通过。
6. **已完成** 提交 `0ff2f1509` 已推送。草稿 PR：https://github.com/fengjy73/go-ethereum/pull/1

## 验证记录

- `go test ./core/rfstate` 通过，含 `TestDropEstimatesRemovesUnpublishedKey`。
- `RF_FIXTURES=/tmp/fixa go test ./core/rfexec -run 'TestFixtureEngines|TestSynthetic|TestLoad'` 通过（约 13s，KZG 预热前）。22418000 串行约 1.87s 来自首次 point-evaluation 的 KZG 初始化，已移到计时区外。
