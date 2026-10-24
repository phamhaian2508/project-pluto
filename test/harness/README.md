# Bộ kiểm thử đối chiếu EVM của Pluto

Bộ kiểm thử nằm ngoài các gói production, nên chạy nó không làm thay đổi cách node hoạt động. Harness tạo trạng thái Pebble chỉ dùng cho test, ký giao dịch từ các tài khoản tất định, chạy cùng một block qua executor của Pluto và go-ethereum, rồi báo giao dịch đầu tiên có kết quả khác nhau.

## Các loại tải kiểm thử

- Tập tài khoản tất định và workload chuyển tiền đã ký, có thể đặt tỷ lệ ví gửi thường xuyên.
- Triển khai Counter, tăng biến đếm, tạo xung đột storage và gọi hợp đồng qua hợp đồng khác.
- Triển khai ERC-20 và gọi `transfer`.
- Một AMM tích số không đổi đơn giản, có thể chạy lại các lệnh `swap(uint256)`.
- Giao dịch revert, hết gas và sai nonce.
- Block trộn nhiều loại giao dịch với seed cố định, gồm 10.000 giao dịch.

Fixture trong `differential_workloads_test.go` mô phỏng hành vi của `contracts/SimpleAMM.sol` trực tiếp bằng EVM bytecode, nên CI không cần cài Solidity compiler. Giá trị dự trữ của fixture cũng được kiểm tra độc lập với kết quả so sánh geth.

## Cách chạy

Chạy toàn bộ harness, bao gồm block 10.000 giao dịch có seed cố định:

```powershell
go test ./test/harness -count=1 -v
```

Chạy riêng bài đối chiếu lớn có thể tái lập:

```powershell
go test ./test/harness -run '^TestDifferentialHarnessTenThousandSeededMixedTransactions$' -count=1 -v
```

Chạy fuzz test của Go trong khoảng thời gian tùy chọn:

```powershell
go test ./test/harness -run '^$' -fuzz '^FuzzDifferentialHarnessSeededMixedBlocks$' -fuzztime=5m
```

Đo benchmark các loại tải chuyển tiền, ví nóng, Counter, ERC-20 và AMM:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkDifferentialWorkloads$' -benchmem -count=1
```

Tên mỗi workload được tạo sẽ có seed đi kèm. Khi gặp lỗi, harness in seed và block/giao dịch đầu tiên bị lệch. Go cũng lưu các lỗi fuzz đã được rút gọn trong `test/harness/testdata/fuzz/` để chạy lại.

## Quy tắc đối chiếu

Harness so sánh tính hợp lệ/trạng thái giao dịch, gas, dữ liệu trả về, log, số dư, nonce, runtime code và các storage slot đã khai báo. Các executor của Pluto cũng được so sánh với nhau, bao gồm state root tất định của Pluto. geth tạo state root theo Ethereum Merkle Patricia Trie, còn Pluto hiện dùng application hash riêng; vì vậy hai loại root này không được xem là cùng định dạng. Với geth, harness so sánh trạng thái tài khoản và hợp đồng có thể quan sát được.

`compareAllExecutors` nhận mọi implementation của `evm.Executor`, rồi chạy cùng workload trên geth, Pluto tuần tự và Pluto song song. Harness so kết quả giao dịch, trạng thái tài khoản/hợp đồng và state root Pluto. Executor song song chạy giao dịch EVM trên overlay riêng, xác thực rồi chấp nhận theo `TxIndex`; nếu kết quả dựa trên phiên bản cũ, nó hủy lần chạy đó và thực thi lại giao dịch trên state mới nhất. Các block chỉ gồm chuyển EOA tĩnh đủ điều kiện dùng đường xử lý S ít cấp phát hơn. P vẫn tắt cho đến khi có artifact được chứng nhận.

So sánh engine tuần tự với engine song song dùng 1, 2, 4 và 8 worker trên block có 128, 512 và 1.024 giao dịch chuyển tiền không xung đột:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -benchmem -count=3
```

## M5 checkpoint policy matrix

The M5 differential matrix runs each deterministic workload with the sequential
executor and parallel P0, P1, P2 (`N=100` and `N=1000`), and P3 configurations, then compares
all results with go-ethereum. It covers transaction status, gas, return data,
logs, synthesized receipt consensus fields, balances, nonces, code, declared
storage slots, a shared observed-state root against geth, and Pluto's native
final block state root across all Pluto executors.

Run the policy matrix and its concurrent seeded workload under the race detector:

```powershell
$env:CGO_ENABLED='1'
go test -race -count=1 ./test/harness -run 'TestCheckpointPolicies'
```

Thời gian tạo Pebble và nạp trạng thái ban đầu không tính vào thời gian benchmark; mỗi lượt vẫn bắt đầu từ trạng thái mới. Đây là benchmark chẩn đoán trên máy cục bộ, không phải bằng chứng rằng toàn bộ EVM đã tăng tốc. Khi chia sẻ kết quả, hãy ghi lại máy, phiên bản Go, số worker, seed và workload.

`mutation_test.go` chứa các executor kiểm thử được cố ý viết sai. Các test xác nhận bộ so sánh phát hiện được giao dịch không hợp lệ bị chấp nhận và giá trị storage bị commit sai. Những executor lỗi này chỉ dùng trong test, không sửa executor production.
# Checkpoint policy metrics (M5.5)

Run the five policies on the same deterministic high-conflict workload and
write one stable CSV row per policy from PowerShell:

```powershell
$env:PLUTO_CHECKPOINT_CSV = Join-Path (Get-Location) "checkpoint-metrics.csv"
go test ./test/harness -run '^TestCheckpointPolicyCSVExport$' -count=1 -v
Remove-Item Env:PLUTO_CHECKPOINT_CSV
```

Run the Go benchmark separately:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkCheckpointPoliciesCSV$' -benchtime=1x -count=1
```

Replay duration is observational only. Policy selection remains based solely
on deterministic opcode/read/conflict inputs. The production adapter resumes
top-level geth frames from captured PC/stack/memory/gas state. Unsupported
boundaries, such as contract creation and Verkle/EIP-4762 execution, are
reported as safe P0 full fallbacks.

## Báo cáo dễ đọc: executor tuần tự và song song

Chạy bài sau để tự in bảng so sánh cho 128, 512 và 1.024 giao dịch, với
executor song song dùng 1, 2, 4 và 8 worker:

```powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
```

Bài kiểm tra chạy cùng workload tất định qua executor tuần tự, các cấu hình
song song và geth. Nếu trạng thái tài khoản, nonce, code, storage, kết quả giao
dịch, gas, log, receipt hoặc state root Pluto khác nhau, test sẽ thất bại và in
ra điểm khác đầu tiên. Báo cáo dùng trung vị của nhiều lần đo; cột ổn định cảnh
báo khi độ dao động vượt 10%.

Đây là phép đo `ExecuteBlock` tại executor. Thời gian tạo node, RPC, nạp state
và khởi động mạng không nằm trong phép đo; số byte cấp phát cũng không phải RAM
đỉnh. Vì vậy hãy dùng kết quả để so sánh engine trên đúng workload này, không
diễn giải trực tiếp thành TPS của cả mạng.
