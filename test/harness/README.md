# Hướng dẫn kiểm thử Pluto

Tài liệu này hướng dẫn chạy các bài kiểm thử và phép đo trong dự án Pluto. Chạy lệnh từ thư mục gốc repository bằng PowerShell. Kiểm thử xác nhận hành vi của mã nguồn; benchmark đo hiệu năng trong cấu hình đã nêu và không tự chứng minh tính đúng đắn.

Báo cáo giải thích ma trận đối chiếu Sequential, OCC, Grouped, Predicted-OCC và trạng thái Geth evm t8n: [Báo cáo kiểm chứng tính đúng đắn](../../docs/bao-cao-kiem-chung-tinh-dung-dan-cac-executor-pluto.md).

## 1. Chọn nhóm lệnh

| Mục đích | Lệnh / test |
|---|---|
| Toàn bộ repository | go test ./... -count=1 |
| Đúng đắn của bốn chế độ thực thi | TestAllExecutionModesMatchSequentialAndGeth |
| Workload harness: Sequential, OCC và Geth | TestAllWorkloadsMatchSequentialParallelAndGeth |
| 10.000 giao dịch hỗn hợp có seed cố định | TestDifferentialHarnessTenThousandSeededMixedTransactions |
| Benchmark chuyển khoản độc lập | BenchmarkSequentialVsParallelIndependentTransfers |
| Báo cáo tải tranh chấp và Access List | Các lệnh ở mục 6 |
| Checkpoint và resume | Các lệnh ở mục 7 |
| Race detector | Các lệnh ở mục 8 |

Một số tên test cũ vẫn có chữ Parallel. Trong các test đó, ParallelExecutor là tên tương thích của OCC; không có nghĩa đây là executor thứ năm. Bốn chế độ được đề cập ở đây là Sequential, OCC, Grouped và Predicted-OCC.

## 2. Chạy toàn repository và toàn harness

Chạy tất cả test Go, không dùng kết quả cache:

~~~powershell
go test ./... -count=1
~~~

Chạy mọi test trong harness với log chi tiết:

~~~powershell
go test ./test/harness -count=1 -v
~~~

Khi khoanh vùng lỗi, chạy test cụ thể bằng cờ -run. Test đơn lẻ qua không đồng nghĩa toàn repository đã qua.

## 3. Kiểm chứng tính đúng đắn giữa các chế độ

### 3.1. Ma trận bốn chế độ

Test này chạy cùng đầu vào qua Sequential, OCC, Grouped và Predicted-OCC, so sánh các kết quả với nhau và với Geth được tích hợp trong harness:

~~~powershell
go test ./test/harness -run '^TestAllExecutionModesMatchSequentialAndGeth$' -count=1 -v
~~~

Ma trận bao gồm chuyển khoản độc lập, Counter tranh chấp, Access List đúng/sai, ERC-20, AMM, workload hỗn hợp, gọi hợp đồng lồng nhau, revert, hết gas và giao dịch không hợp lệ. Các assertion so sánh những kết quả do fixture cung cấp như receipt, log, số dư, nonce, code, storage và state root. Đây là đối chiếu với Geth tích hợp trong harness; nó không thay cho Geth evm t8n độc lập ở mục 5.

### 3.2. Workload rộng và workload có seed

Test này chạy 11 workload trên Sequential, OCC cửa sổ trượt với worker tự điều chỉnh, OCC cửa sổ trượt 4 worker, Grouped, Predicted-OCC và Geth tích hợp trong harness. Mỗi executor Pluto được đối chiếu với Geth và với Sequential; ma trận gồm cả workload chuyển coin giữa các ví, ví nóng, Counter tranh chấp, Access List đúng/sai, ERC-20, AMM, lời gọi lồng nhau, lỗi giao dịch và block hỗn hợp 10.000 giao dịch. Đây là test tính đúng đắn, không phải benchmark. Tên `Parallel` trong tên test là tên cũ được giữ để các lệnh hiện có tiếp tục dùng được.

~~~powershell
go test ./test/harness -run '^TestAllWorkloadsMatchSequentialParallelAndGeth$' -count=1 -v
~~~

Test sau tạo workload hỗn hợp 10.000 giao dịch theo seed cố định, giúp tái tạo đầu vào khi điều tra sai khác:

~~~powershell
go test ./test/harness -run '^TestDifferentialHarnessTenThousandSeededMixedTransactions$' -count=1 -v
~~~

Các test chuyên biệt kiểm tra xung đột OCC, fallback của Grouped, chính sách checkpoint của Predicted-OCC và tính nhất quán sau SELFDESTRUCT:

~~~powershell
go test ./internal/evm -run '^TestOCCWindowForcedCounterConflictMatchesSequential$' -count=1 -v
go test ./internal/evm -run '^TestGroupedMissingDeclaredKeyAbortsBatchAndMatchesSequential$' -count=1 -v
go test ./internal/evm -run '^TestPredictedOCCCheckpointsAllPoliciesMatchSequentialAndWaitingHoldsZeroBudget$' -count=1 -v
go test ./internal/evm -run '^TestCreateThenSelfDestructSameBlockAppHashParity$' -count=1 -v
~~~

Các test lập lịch trong internal/lab kiểm tra approve độc lập, giao dịch cùng pool nóng và xung đột gián tiếp khi hai pool chuyển đến cùng người nhận:

~~~powershell
go test ./internal/lab -run '^(TestGroupedApproveIndependentRunsInParallelAndTipSplits|TestPredictedOCCWaitsForHotPoolPredecessorWhileOtherPoolRuns|TestPredictedOCCSharedRecipientDifferentPoolsConflictCaughtByOCC)$' -count=1 -v
~~~

## 4. So sánh hiệu năng tuần tự với OCC trên tải độc lập

Báo cáo dưới đây đo các giao dịch chuyển coin có sender và recipient tách biệt, trên cùng workload và trạng thái đầu vào. Nó so Sequential với OCC cửa sổ trượt ở chế độ tự điều chỉnh và các mức 1, 2, 4, 8 worker. Sáu cỡ block (32, 64, 128, 256, 512, 1.024 giao dịch) giúp tìm khoảng hòa vốn trong dải đã đo. Mỗi dòng thời gian là trung vị của 5 mẫu, mỗi mẫu chạy 5 block; thời gian chỉ tính `ExecuteBlock`, không tính tạo DB, đồng thuận, RPC hoặc P2P. Mỗi kết quả vẫn được kiểm tra với Sequential và Geth tích hợp.

Cuối báo cáo, `OBSERVED BREAK-EVEN` in cỡ workload nhỏ nhất trong các mức đã lấy mẫu mà OCC có trung vị nhanh hơn Sequential và độ biến thiên của cả hai không vượt 10%. Đây là điểm giao nhau quan sát trên lưới cỡ block đã thử, không phải ngưỡng phổ quát hay giá trị nội suy. Báo cáo cũng in Go, hệ điều hành, CPU và `GOMAXPROCS`; giữ nguyên đầu ra đó khi đưa số liệu vào đồ án.

~~~powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
~~~

Lệnh này tạo bảng minh chứng Sequential/OCC cho tải độc lập và điểm hòa vốn quan sát được. Các lệnh tiếp theo tập trung vào tranh chấp hợp đồng và Access List:

~~~powershell
# Nhiều giao dịch tranh chấp cùng hợp đồng/Counter
go test ./test/harness -run '^TestContractContentionSequentialParallelReport$' -count=1 -v

# Access List khai báo đúng trên workload tranh chấp
go test ./test/harness -run '^TestAccessListContractContentionSequentialParallelReport$' -count=1 -v

# Access List khai báo sai
go test ./test/harness -run '^TestIncorrectAccessListContractContentionSequentialParallelReport$' -count=1 -v

# Các giao dịch truy cập những slot khác nhau trong cùng hợp đồng
go test ./test/harness -run '^TestSameContractDisjointAccessListSequentialParallelReport$' -count=1 -v
~~~

Đây là các báo cáo trong harness, không đo đồng thuận CometBFT hay RPC. Chúng tập trung vào Sequential/OCC; không đại diện cho phép so sánh đủ cả bốn chế độ.

Để lấy số liệu benchmark chi tiết, gồm thời gian và cấp phát bộ nhớ, chạy:

~~~powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchmem -count=1
~~~

Benchmark khảo sát 32, 64, 128, 256, 512 và 1.024 giao dịch với OCC cửa sổ trượt 1, 2, 4 và 8 worker, bên cạnh Sequential. Kết quả Go benchmark cung cấp `ns/op`, `B/op` và `allocs/op`; dùng báo cáo ở lệnh phía trên để có bảng trung vị và cách xác định điểm hòa vốn.

- ns/op là thời gian trung bình mỗi lần chạy; benchmark này đo ExecuteBlock, không bao gồm đồng thuận CometBFT, P2P hay RPC.
- B/op và allocs/op lần lượt là byte và số lần cấp phát trung bình.
- Kết quả phụ thuộc CPU, số lõi, phiên bản Go và trạng thái máy. Chạy nhiều lần trước khi kết luận; không suy ra OCC luôn nhanh hơn từ một lượt đo.
- Tải nhỏ hoặc có tranh chấp cao có thể khiến chi phí worker, overlay và xác thực lớn hơn lợi ích song song.

## 5. Đối chiếu độc lập bằng Geth evm t8n

pluto-lab verify dùng evm t8n của Geth làm bộ thực thi tham chiếu độc lập cho fixture. Lệnh Phase 1 kiểm tra mọi fixture, chế độ suite hỗ trợ và tắt checkpoint:

~~~powershell
go run ./cmd/pluto-lab verify --suite phase1 --case all --mode all --checkpoint off
~~~

Cần binary evm tương thích với phiên bản Geth Pluto sử dụng và có subcommand t8n. Có thể đặt binary trong PATH hoặc chỉ định biến EVM_BIN:

~~~powershell
$env:EVM_BIN = 'C:\tools\geth\evm.exe'
go run ./cmd/pluto-lab verify --suite phase1 --case all --mode all --checkpoint off
~~~

Nếu lệnh báo evm binary not found thì đối chiếu độc lập chưa chạy; không ghi trường hợp đó thành PASS. Test TestAllExecutionModesMatchSequentialAndGeth ở mục 3 dùng Geth tích hợp trong harness, không phải lệnh evm t8n bên ngoài.

## 6. Kiểm thử checkpoint và resume

Các test harness so sánh chính sách checkpoint với Sequential và Geth trên workload đại diện, đồng thời kiểm tra workload seeded:

~~~powershell
go test ./test/harness -run '^(TestCheckpointPoliciesMatchSequentialAndGethAcrossWorkloads|TestCheckpointPoliciesConcurrentSeededWorkload)$' -count=1 -v
~~~

Chạy nhóm test EVM liên quan checkpoint, resume, adaptive và Predicted-OCC:

~~~powershell
go test ./internal/evm -run 'Checkpoint|Resume|Adaptive|PredictedOCC' -count=1 -v
~~~

Chạy toàn bộ test package EVM:

~~~powershell
go test ./internal/evm -count=1
~~~

Checkpoint là tối ưu hóa cho một số đường OCC/Predicted-OCC. Khi không thể khôi phục an toàn, executor cần fallback thay vì chấp nhận trạng thái sai. Test qua không có nghĩa mọi fork EVM hay mọi bố cục nội bộ Geth đều hỗ trợ resume.

## 7. ABCI, trạng thái, lưu trữ và race detector

Chạy test các package ứng dụng ABCI, EVM và Pebble:

~~~powershell
go test ./internal/app ./internal/evm ./internal/store -count=1
~~~

Khoanh vùng các test callback ABCI và tính nhất quán trạng thái:

~~~powershell
go test ./internal/app -run 'ProcessProposal|PrepareProposal|FinalizeBlock|Commit|InitChain|Snapshot|PrevRandao' -count=1 -v
go test ./internal/evm -run 'SelfDestruct|AppHashParity|Resume|Checkpoint' -count=1 -v
~~~

Race detector cho package lõi và ma trận bốn chế độ:

~~~powershell
go test -race ./internal/app ./internal/evm ./internal/store
go test -race ./test/harness -run '^TestAllExecutionModesMatchSequentialAndGeth$' -count=1
~~~

Muốn chạy race detector toàn repository:

~~~powershell
go test -race ./... -count=1
~~~

Race detector chỉ phát hiện data race trên những đường test thực sự chạy; nó không chứng minh mọi nhánh không có race. Không gán nhãn T1–T4 nếu báo cáo chưa định nghĩa từng nhãn và ánh xạ chúng tới test cụ thể.

## 8. Kiểm thử mạng nhiều validator

Các test package internal/plutonetwork kiểm tra cấu hình và hành vi mạng validator:

~~~powershell
go test ./internal/plutonetwork -count=1 -v
go test ./internal/plutonetwork -run '^TestFourValidatorsCatchUpAndMixedExecutors$' -count=1 -v
~~~

Đây là kiểm thử tầng mạng/tích hợp. Không đồng nhất chúng với benchmark ExecuteBlock: mạng validator còn có chi phí tạo block, truyền thông điệp, bỏ phiếu và commit.

## 9. Thực nghiệm bằng pluto-lab

### 10.1. Kiểm chứng Phase 1

Đối chiếu fixture Phase 1 với Geth và các chế độ thực thi:

~~~powershell
go run ./cmd/pluto-lab verify --suite phase1 --case all --mode all --checkpoint off
~~~

Lệnh này cần evm t8n như mô tả ở mục 5.

### 10.2. Đo bốn chế độ trên fixture Phase 1

Chạy 10 lượt đo và ghi kết quả vào thư mục riêng:

~~~powershell
go run ./cmd/pluto-lab bench --suite phase1 --runs 10 --out .\results\phase1-four-modes
~~~

Suite tạo tham chiếu trước vùng đo, sau đó đo Sequential, OCC, Grouped và Predicted-OCC theo worker mà suite định nghĩa. Kiểm tra đầu ra CSV và bản tóm tắt để biết chính xác fixture/cấu hình đã chạy. Phép đo này cần binary Geth evm t8n.

### 10.3. Hiệu chuẩn và benchmark Phase 3

Phase 3 adaptive cần model được tạo trước. Chạy hiệu chuẩn rồi kiểm tra file:

~~~powershell
go run ./cmd/pluto-lab calibrate --suite phase3 --out .\results\phase3\model.json
Test-Path .\results\phase3\model.json
~~~

Chỉ chạy benchmark sau khi hiệu chuẩn thành công và Test-Path trả True:

~~~powershell
go run ./cmd/pluto-lab bench --suite phase3 --matrix --model .\results\phase3\model.json --out .\results\phase3-bench
~~~

Đối chiếu fixture Phase 3 với adaptive checkpoint:

~~~powershell
go run ./cmd/pluto-lab verify --suite phase3 --case all --mode occ --checkpoint adaptive --model .\results\phase3\model.json --workers 4
~~~

Các lệnh Phase 3 cần model phù hợp và Geth evm t8n nếu suite yêu cầu tham chiếu ngoài. Xem cú pháp của phiên bản CLI hiện tại:

~~~powershell
go run ./cmd/pluto-lab --help
go run ./cmd/pluto-lab bench --help
go run ./cmd/pluto-lab verify --help
~~~

## 10. Cách đọc kết quả và lập báo cáo

Khi báo cáo phép thử, ghi tối thiểu:

1. Lệnh đầy đủ, phiên bản/commit mã nguồn và ngày chạy.
2. CPU, số lõi, RAM, phiên bản Go và hệ điều hành.
3. Workload, số giao dịch, worker, executor và chính sách checkpoint.
4. Các trạng thái được đối chiếu: receipt, log, số dư, nonce, code/storage và state root nếu test có assertion đó.
5. Chỉ số hiệu năng: thời gian, throughput, bộ nhớ, cấp phát, retry/abort hoặc số liệu lập lịch mà bài test thực sự in ra.
6. Số lượt đo và độ phân tán; phân biệt ExecuteBlock với đo mạng có đồng thuận/RPC.

Lưu đầu ra test, CSV/summary và cấu hình làm minh chứng. Không so sánh số liệu từ workload, máy, worker hoặc vùng đo khác nhau. PASS chỉ xác nhận assertion của test đã chạy được thỏa mãn.

## 11. Bộ lệnh tối thiểu trước khi gửi thay đổi

~~~powershell
go test ./... -count=1
go test -race ./internal/app ./internal/evm ./internal/store
go test ./test/harness -run '^TestAllExecutionModesMatchSequentialAndGeth$' -count=1 -v
~~~

Nếu sửa executor hoặc checkpoint, bổ sung test chuyên biệt ở mục 3 và 7. Nếu sửa ABCI/Commit/snapshot, chạy test mục 8. Nếu sửa mạng validator, chạy mục 9.\n