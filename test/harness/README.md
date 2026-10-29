# Hướng dẫn kiểm thử và đo hiệu năng

## Harness là gì?

Harness là bộ khung dựng dữ liệu thử nghiệm và chạy cùng một workload qua nhiều bộ thực thi. Trong thư mục này, workload thường được chạy trên:

- **Pluto tuần tự**: xử lý giao dịch lần lượt theo thứ tự trong block.
- **Pluto song song**: thử xử lý đồng thời, kiểm tra xung đột rồi chạy lại khi cần.
- **Geth tham chiếu**: dùng EVM và state trong bộ nhớ của go-ethereum để đối chiếu kết quả có thể quan sát.

Các test đối chiếu kiểm tra trạng thái giao dịch, gas, log, dữ liệu trả về, số dư, nonce, code và các storage slot mà workload khai báo. Với Pluto, chúng còn so sánh state root giữa executor tuần tự và song song. State root riêng của Pluto không cùng định dạng với Ethereum Merkle Patricia Trie của geth nên không so sánh trực tiếp hai root đó.

Benchmark ở đây chủ yếu đo `ExecuteBlock` trên workload dựng sẵn. Đây không phải phép đo TPS của mạng nhiều validator: nó không tính thời gian khởi động node, RPC, truyền giao dịch qua mạng hoặc đồng thuận CometBFT.

> Các lệnh bên dưới dùng cú pháp PowerShell và chạy từ thư mục gốc của repository.

## Chạy toàn bộ test

```powershell
go test ./test/harness -count=1
```

Chạy tất cả test trong harness một lần, không dùng lại kết quả cache. Nếu muốn xem tên từng test, log và bảng báo cáo:

```powershell
go test ./test/harness -count=1 -v
```

Chạy test của toàn bộ project, không chỉ harness:

```powershell
go test ./... -count=1
```

Kiểm tra race condition trong toàn bộ project:

```powershell
go test -race ./... -count=1
```

Race detector tìm một số truy cập bộ nhớ đồng thời không được đồng bộ. Kết quả race sạch **không tự chứng minh** hai executor cho cùng kết quả; cần chạy thêm test đối chiếu.

## Test đối chiếu tính đúng đắn

### Ma trận workload

```powershell
go test ./test/harness -run '^TestAllWorkloadsMatchSequentialParallelAndGeth$' -count=1 -v
```

Chạy nhiều workload với Pluto tuần tự, Pluto song song tự điều chỉnh, Pluto song song 4 worker và geth. Các workload hiện gồm chuyển coin độc lập và qua ví nóng, Counter, nhiều ví gọi Counter, access list đúng/sai, ERC-20, AMM, gọi hợp đồng lồng nhau, revert, hết gas, nonce sai và block trộn. Test dừng và báo lỗi nếu phát hiện sai khác.

### 10.000 giao dịch có seed cố định

```powershell
go test ./test/harness -run '^TestDifferentialHarnessTenThousandSeededMixedTransactions$' -count=1 -v
```

Tạo một workload hỗn hợp 10.000 giao dịch có thể tái lập, rồi so Pluto tuần tự với Pluto song song 4 worker và geth. Seed cố định giúp dựng lại đúng workload khi tìm lỗi. Đây là một kịch bản lớn có thể lặp lại, không phải chứng minh mọi loại giao dịch EVM đều đúng.

### Các bài đối chiếu riêng

```powershell
go test ./test/harness -run '^(TestDifferentialHarnessCounterWorkloadMatchesGeth|TestDifferentialHarnessComparesPlutoExecutorsAndStateRoot|TestDifferentialHarnessParallelStaticTransfersMatchSequentialAndGeth|TestDifferentialHarnessParallelFullyReplaysAfterValidationFailure|TestDifferentialHarnessParallelStaticTransferFailuresMatch|TestDifferentialHarnessERC20MatchesGeth|TestDifferentialHarnessCounterContentionMatchesGeth|TestDifferentialHarnessNestedCallMatchesGeth|TestDifferentialHarnessConstantProductAMMMatchesGeth|TestDifferentialHarnessFailureModesMatchGeth)$' -count=1 -v
```

Nhóm này kiểm tra các workload nhỏ, riêng biệt để khi lỗi xuất hiện dễ khoanh vùng hơn: Counter, chuyển khoản, replay sau xung đột, ERC-20, AMM, lời gọi lồng nhau và các trường hợp lỗi.

### Test chính sách checkpoint

```powershell
go test ./test/harness -run '^(TestCheckpointPoliciesMatchSequentialAndGethAcrossWorkloads|TestCheckpointPoliciesConcurrentSeededWorkload|TestCheckpointPolicyCSVExport)$' -count=1 -v
```

So sánh các chính sách checkpoint P0, P1, P2 theo chu kỳ và P3 thích nghi với executor tuần tự và geth trên workload đã chọn. Nhóm này kiểm tra tính đúng đắn và số liệu checkpoint; nó không mặc định checkpoint sẽ nhanh hơn.

### Test harness có bắt được lỗi cố ý không?

```powershell
go test ./test/harness -run '^TestHarnessDetectsInjectedMutations$' -count=1 -v
```

Đưa một số sai lệch giả lập vào kết quả để xác nhận harness phát hiện được. Đây là test cho khả năng phát hiện lỗi của bộ đối chiếu, không phải test hiệu năng.

## Báo cáo hiệu năng có bảng dễ đọc

### Chuyển coin độc lập

```powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
```

In bảng so sánh Pluto tuần tự, song song tự điều chỉnh và song song 1/2/4/8 worker ở quy mô 128, 512 và 1.024 giao dịch mỗi block. Báo cáo có thời gian trung vị, throughput ước tính trong executor, chênh lệch so với tuần tự, bộ nhớ cấp phát, số lần cấp phát, worker thực dùng, abort/retry và kết quả đối chiếu.

Test đo 5 mẫu, mỗi mẫu chạy 5 block; mỗi mẫu đều được kiểm tra tính đúng đắn. Nếu báo cáo cảnh báo số đo dao động trên 10%, hãy chạy lại khi máy ít tải. Không diễn giải throughput trong bảng thành TPS của mạng thật.

Lưu toàn bộ đầu ra vào tệp trong PowerShell:

```powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v 2>&1 | Tee-Object -FilePath .\comparison-report.txt
```

### Nhiều giao dịch cùng gọi một hợp đồng

```powershell
go test ./test/harness -run '^TestContractContentionSequentialParallelReport$' -count=1 -v
```

So sánh tuần tự và các cấu hình song song khi nhiều giao dịch tranh chấp cùng hợp đồng/ô trạng thái Counter. Trường hợp này giúp thấy chi phí xung đột, abort và chạy lại; nó có thể chậm hơn tuần tự.

### Cùng hợp đồng, access list đúng hoặc sai

```powershell
go test ./test/harness -run '^(TestAccessListContractContentionSequentialParallelReport|TestIncorrectAccessListContractContentionSequentialParallelReport)$' -count=1 -v
```

Chạy riêng trường hợp access list đúng và khai báo sai. Báo cáo đối chiếu kết quả với tuần tự/geth và hiển thị lựa chọn lập lịch cùng chi phí gas. Access list là dữ liệu do giao dịch cung cấp; danh sách sai không được xem là bằng chứng rằng EVM đã truy cập đúng các slot đó.

### Cùng hợp đồng nhưng ghi các ô storage khác nhau

```powershell
go test ./test/harness -run '^TestSameContractDisjointAccessListSequentialParallelReport$' -count=1 -v
```

Đây là trường hợp có khả năng tận dụng song song hơn: các giao dịch gọi cùng một hợp đồng nhưng làm việc trên các storage slot tách biệt. Test so kết quả cuối với tuần tự/geth và in số đo cho các mức giao dịch, worker. Kết quả nhanh hơn chỉ áp dụng cho workload và máy đã đo.

## Benchmark chi tiết của Go

Benchmark độc lập cho workload chuyển coin, so tuần tự với 1/2/4/8 worker:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -benchmem -count=3
```

`-run '^$'` bỏ qua các test thường; `-bench` chọn benchmark; `-benchtime=30x` chạy mỗi biến thể 30 lần; `-benchmem` in bộ nhớ cấp phát và số lần cấp phát; `-count=3` lặp lại benchmark ba lượt. Các dòng `sequential` là mốc tuần tự; so chúng với `parallel-workers-1`, `-2`, `-4` và `-8` trong cùng quy mô `tx-128`, `tx-512` hoặc `tx-1024`.

Benchmark workload đa dạng, phần lớn chỉ đo executor tuần tự:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkDifferentialWorkloads$' -benchtime=10x -benchmem
```

Benchmark các chính sách checkpoint:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkCheckpointPoliciesCSV$' -benchtime=10x -benchmem
```

Các trường thường gặp trong kết quả Go:

- `ns/op`: thời gian cho một lần chạy; số nhỏ hơn thường nhanh hơn.
- `B/op`: byte được cấp phát trong một lần chạy; đây không phải RAM đỉnh của tiến trình.
- `allocs/op`: số lần cấp phát trong một lần chạy.
- Các số liệu như `aborts/op`, `retries/op`, `checkpoint-B/op`: metric bổ sung do benchmark báo cáo.

Benchmark Go in nhiều dòng kỹ thuật; dùng các bài `...Report` phía trên nếu cần bảng tổng hợp dễ đọc. Khi so tốc độ, so đúng cùng số giao dịch và cùng workload; đừng kết luận từ một lần chạy ngắn hoặc một máy đang tải cao.

## Fuzz test

Chạy fuzz trong 15 giây, một worker fuzz để giảm tải và giúp kết quả dễ tái lập hơn:

```powershell
go test ./test/harness -run '^$' -fuzz '^FuzzDifferentialHarnessSeededMixedBlocks$' -fuzztime=15s -parallel=1
```

Fuzzer thay seed để sinh nhiều block trộn; mỗi trường hợp được đối chiếu với các executor tham chiếu. Có thể tăng thời gian, ví dụ `-fuzztime=2m`, để tìm thêm trường hợp. Nếu Go tìm thấy lỗi, nó lưu đầu vào tái hiện trong thư mục testdata của package.

## Xuất số liệu checkpoint ra CSV

Test CSV xác nhận các chính sách checkpoint có số liệu hợp lệ và mặc định in CSV vào log test:

```powershell
go test ./test/harness -run '^TestCheckpointPolicyCSVExport$' -count=1 -v
```

Để lưu riêng CSV vào tệp:

```powershell
$env:PLUTO_CHECKPOINT_CSV = '.\checkpoint-metrics.csv'
go test ./test/harness -run '^TestCheckpointPolicyCSVExport$' -count=1 -v
Remove-Item Env:PLUTO_CHECKPOINT_CSV
```

Tệp CSV ghi metric của P0/P1/P2/P3 trên cùng workload. P0 là mốc không checkpoint. So sánh cả thời gian chạy lại, số checkpoint tạo/dùng lại và bộ nhớ checkpoint; ít opcode chạy lại hơn chưa chắc tổng thời gian thấp hơn.

## Chọn lệnh theo mục tiêu

| Muốn biết | Chạy lệnh |
| --- | --- |
| Toàn bộ harness còn đúng không | `go test ./test/harness -count=1` |
| Pluto tuần tự/song song/geth có khớp trên nhiều loại tải không | `TestAllWorkloadsMatchSequentialParallelAndGeth` hoặc test 10.000 giao dịch |
| Có bảng hiệu năng tuần tự và song song dễ đọc | `TestSequentialParallelPerformanceReport` |
| Tranh chấp cùng Counter tốn chi phí thế nào | `TestContractContentionSequentialParallelReport` |
| Access list đúng/sai ảnh hưởng ra sao | Hai test `AccessList...Report` |
| Gọi cùng hợp đồng nhưng ghi slot riêng có lợi không | `TestSameContractDisjointAccessListSequentialParallelReport` |
| Benchmark Go chi tiết, có bộ nhớ | `BenchmarkSequentialVsParallelIndependentTransfers` |
| Các policy checkpoint dùng tài nguyên thế nào | `TestCheckpointPolicyCSVExport` hoặc `BenchmarkCheckpointPoliciesCSV` |
| Tìm workload sai khác theo seed ngẫu nhiên | `FuzzDifferentialHarnessSeededMixedBlocks` |
| Harness có phát hiện lỗi chèn thử không | `TestHarnessDetectsInjectedMutations` |

Các tên test có thể chạy riêng bằng mẫu:

```powershell
go test ./test/harness -run '^TEN_TEST$' -count=1 -v
```

Thay `TEN_TEST` bằng tên test đầy đủ trong bảng hoặc danh sách ở trên.
