# Báo cáo phần thực thi giao dịch song song của Pluto

**Ngày:** 17/10/2026

**Mục đích:** Tóm tắt phần đã làm, cách kiểm tra và phần còn lại bằng ngôn ngữ dễ hiểu.

## 1. Tóm tắt ngắn

Pluto hiện có executor lạc quan thử nghiệm: worker chạy giao dịch EVM trên overlay riêng, sau đó bộ phân giải kiểm tra và chấp nhận kết quả theo thứ tự block. Nếu một giao dịch đọc state đã bị giao dịch trước thay đổi, lần chạy thử bị bỏ và toàn bộ giao dịch được chạy lại trên state mới. Block chỉ gồm giao dịch chuyển EOA tĩnh đủ điều kiện vẫn dùng đường S trực tiếp ít cấp phát hơn. Chế độ song song cần bật thủ công; mặc định node vẫn dùng engine tuần tự.

Phần này được đối chiếu với engine tuần tự của Pluto và geth, có kiểm thử xung đột/ chạy lại, và đã chạy race test. Benchmark chuyển tiền độc lập còn dao động theo kích thước block và số worker; chưa có bằng chứng ổn định rằng 4 worker luôn nhanh hơn. Kết quả hiệu năng được ghi ở mục 6.

## 2. Hiểu đơn giản cơ chế mới

Một block có thứ tự giao dịch cố định. Pluto vẫn giữ thứ tự đó làm chuẩn để tính kết quả cuối cùng.

1. Mỗi worker chạy một giao dịch trên `TxState` riêng; dữ liệu chưa sửa được đọc từ state block bất biến.
2. Worker ghi lại tập khóa đã đọc và ghi, kèm phiên bản quan sát được.
3. Bộ phân giải xét kết quả theo `TxIndex`. Kết quả còn hợp lệ được áp dụng; kết quả đọc state cũ bị hủy và chạy lại trên state mới nhất.
4. Cả giao dịch hợp đồng cũng có thể được chạy thử đồng thời. Các giao dịch phụ thuộc nhau có thể phải chạy lại; loại P vẫn chưa được bật.
5. Chỉ luồng block theo thứ tự được sửa state chuẩn. Worker không ghi trực tiếp xuống PebbleDB.

Có thể hình dung worker là nhiều người tính trước các phép tính độc lập, còn một bộ kiểm tra cuối cùng duyệt và chốt kết quả theo thứ tự của block.

## 3. Những gì đã được thêm

| Phần | Đã làm |
|---|---|
| Thiết kế | Viết thiết kế P/O/S và sơ đồ luồng trong `design/m4-hybrid-parallel-execution.md`. |
| Phân loại giao dịch | **S** là chuyển EOA có tập truy cập hẹp được chứng minh; **O** là giao dịch thực thi bằng EVM trên overlay; **P** cần artifact thực thi được chứng nhận và hiện chưa bật. |
| Cách ly trạng thái | `TxState` cài `vm.StateDB`, đọc xuyên từ state gốc bất biến và giữ cache/journal/thay đổi riêng cho giao dịch. |
| Bộ nhớ phiên bản | MVMemory chia shard, giữ phiên bản và giá trị đã chấp nhận theo khóa, `TxIndex`, incarnation; có thể đọc phiên bản mới nhất của giao dịch đứng trước. |
| Worker và kiểm tra xung đột | Worker pool chạy thử theo lô. Mỗi lần chạy được đánh dấu `ESTIMATE`; bộ phân giải kiểm tra lần lượt và đánh dấu hủy/chạy lại khi tập đọc cũ. |
| Chốt kết quả | State delta được áp dụng theo thứ tự giao dịch; app tiếp tục ghi state và metadata bằng batch Commit hiện có. Worker không tự ghi DB. |
| Cấu hình node | Thêm tùy chọn `--executor=sequential|parallel` và `--workers=N`; mặc định vẫn là `sequential`. |
| Số liệu | Ghi số lượng giao dịch theo loại, worker, số lần chạy lại/hủy và thời gian các pha để hỗ trợ chẩn đoán, đo benchmark. |

## 4. Phần chưa hoàn thành

- Loại **P** chưa có bộ sinh/chứng nhận artifact và hiện vẫn tắt.
- Executor song song còn là prototype; workload EVM có phụ thuộc cao có thể bị chạy lại nhiều lần, nên chạy song song không đồng nghĩa mọi giao dịch đều tiết kiệm thời gian.
- Differential test hiện có Counter, ERC-20, AMM, gọi lồng nhau, revert/out-of-gas và block trộn 10.000 giao dịch; chưa chứng minh toàn bộ ngữ nghĩa EVM cho mọi hợp đồng.
- Benchmark hiện chưa chứng minh mức tăng thông lượng ổn định cho cấu hình từ 4 worker trở lên; xem số liệu và giới hạn mẫu ở mục 6.

Do đó, phần cơ chế M4 đã được mở rộng đáng kể và có kiểm thử; tiêu chí hiệu năng vẫn chưa được chứng minh. Nên báo cáo riêng độ đúng đắn, phạm vi hỗ trợ và hiệu năng, không gộp chúng thành một tuyên bố “nhanh hơn”.

## 5. Kiểm tra đã chạy

Các lệnh sau đã chạy thành công trong lượt làm việc này:

```powershell
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
go test ./test/harness -run '^$' -fuzz '^FuzzDifferentialHarnessSeededMixedBlocks$' -fuzztime=15s -parallel=1
git diff --check
```

Kết quả fuzz chạy 15 giây hoàn tất với 676 lần thực thi, không phát hiện sai lệch. Differential harness còn có workload hỗn hợp tất định 10.000 giao dịch để so sánh executor Pluto tuần tự/song song; đây là workload kiểm thử, không có nghĩa mọi giao dịch trong đó đều được thực thi đồng thời.

## 6. Kết quả benchmark hiện tại

Đo trên cùng máy (Windows amd64, Go 1.27.1, Intel Core i7-11800H), bằng benchmark 128, 512 và 1.024 giao dịch chuyển độc lập. Mỗi cấu hình chạy 30 lượt trong lần đo này; số liệu dưới đây là kết quả của lần chạy gần nhất, nên có thể dao động theo tải máy:

| Số giao dịch | Tuần tự | Song song, 4 worker | Thay đổi thời gian | Bộ nhớ tuần tự → song song |
|---:|---:|---:|---:|---:|
| 128 | 1,085 ms | 1,162 ms | Chậm hơn 7,1% | 598 KB → 964 KB |
| 512 | 5,029 ms | 4,264 ms | Nhanh hơn 15,2% | 2,42 MB → 4,00 MB |
| 1.024 | 10,107 ms | 8,982 ms | Nhanh hơn 11,1% | 4,84 MB → 8,00 MB |

Kết quả cho thấy chi phí worker và resolver chưa đáng giá ở block 128; block 512 và 1.024 có cải thiện trong phép đo này. Vì vậy mục tiêu hiệu năng mới đạt một phần, chưa thể khẳng định song song luôn nhanh hơn. Song song dùng nhiều bộ nhớ hơn khoảng 61–65%. Số worker tăng không bảo đảm tăng tốc: lần đo này, 4 worker cho kết quả tốt nhất ở block 512/1.024, còn block 128 vẫn chậm hơn tuần tự. Đây là benchmark vi mô trên máy phát triển, nên chưa đủ để kết luận hiệu năng production; cần đo lặp lại và profiling trên cấu hình node thực tế.

## 7. Cách chạy

Chạy node bằng engine tuần tự mặc định:

```powershell
go run ./cmd/plutod --home .\.pluto-home
```

Bật prototype song song với 4 worker:

```powershell
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=4
```

Chạy bài so sánh benchmark tuần tự và song song:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -benchmem -count=1
```

## 8. Tóm tắt tiến độ theo các mục M4

| Mục | Tiến độ hiện tại |
|---|---|
| M4.1 Thiết kế | Đã viết thiết kế và sơ đồ. |
| M4.2 TxState/overlay | Đã được dùng bởi executor EVM thử nghiệm; mỗi attempt chạy trên state riêng và có delta riêng. |
| M4.3 MVMemory | Có phiên bản và giá trị chấp nhận theo khóa, transaction index và incarnation; có test đọc phiên bản của giao dịch trước. |
| M4.4 Scheduler | Có worker pool, attempt ở trạng thái `ESTIMATE`, resolver theo thứ tự block và hủy/chạy lại khi dữ liệu đọc cũ. |
| M4.5 Read/write set và incarnation | Được ghi tự động khi EVM đọc/ghi balance, nonce, code, account và storage; retry tạo incarnation mới. |
| M4.6 Validate/retry | Đã kiểm tra phiên bản và chạy lại toàn bộ giao dịch phụ thuộc; harness yêu cầu workload Counter chứng minh đã có retry. |
| M4.7 Commit theo thứ tự | Delta được áp dụng theo `TxIndex`, state root đối chiếu với executor tuần tự; Pebble vẫn ghi bằng batch Commit hiện có. |
| M4.8 Metrics | Có số liệu phân loại, retry/abort, incarnation, worker và thời gian các pha. |
| M4.9 Tích hợp lựa chọn executor | Đã có cờ chọn tuần tự/song song và số worker; tuần tự vẫn là mặc định. |

## 9. Kết luận

Đã có executor thử nghiệm dùng overlay cho EVM, xác thực và chạy lại giao dịch phụ thuộc theo thứ tự, cùng đường S tối ưu cho block chuyển EOA đủ điều kiện. Engine tuần tự tiếp tục là mốc tham chiếu và lựa chọn mặc định. Kết quả test chứng minh các workload được liệt kê khớp; benchmark cần đo lặp lại thêm và hiện chưa chứng minh tăng thông lượng ổn định với từ 4 worker.
