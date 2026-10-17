# Báo cáo phần thực thi giao dịch song song của Pluto

**Ngày:** 15/10/2026

**Mục đích:** Tóm tắt phần đã làm, cách kiểm tra và phần còn lại bằng ngôn ngữ dễ hiểu.

## 1. Tóm tắt ngắn

Pluto hiện có một bản thử nghiệm đầu tiên để xử lý song song **một nhóm giao dịch chuyển GO rất đơn giản**. Các giao dịch hợp đồng thông minh vẫn chạy theo cách tuần tự cũ. Chế độ song song phải được bật thủ công; mặc định node vẫn dùng engine tuần tự.

Phần mới đã được so sánh với engine tuần tự của Pluto và geth trong các bài kiểm tra, đồng thời đã chạy test, race test, vet và build. Tuy nhiên, benchmark hiện tại cho thấy workload chuyển tiền độc lập được thử **chậm hơn khoảng 15% khi dùng 4 worker**. Vì vậy, đây là nền móng để tiếp tục phát triển, chưa phải bằng chứng rằng Pluto đã tăng tốc giao dịch.

## 2. Hiểu đơn giản cơ chế mới

Một block có thứ tự giao dịch cố định. Pluto vẫn giữ thứ tự đó làm chuẩn để tính kết quả cuối cùng.

1. Pluto xem từng giao dịch có đủ đơn giản và dự đoán được để chạy song song hay không.
2. Các giao dịch chuyển tiền độc lập, không đụng cùng tài khoản, có thể được worker tính cùng lúc.
3. Những giao dịch có khả năng ảnh hưởng lẫn nhau được xếp vào lượt phù hợp, rồi kiểm tra theo đúng thứ tự ban đầu.
4. Giao dịch gọi hoặc tạo hợp đồng thông minh chưa được chạy song song; chúng quay về engine EVM tuần tự hiện có.
5. Kết quả sau cùng phải khớp với engine tuần tự. Worker không tự ghi xuống PebbleDB.

Có thể hình dung worker là nhiều người tính trước các phép tính độc lập, còn một bộ kiểm tra cuối cùng duyệt và chốt kết quả theo thứ tự của block.

## 3. Những gì đã được thêm

| Phần | Đã làm |
|---|---|
| Thiết kế | Viết thiết kế P/O/S và sơ đồ luồng trong `design/m4-hybrid-parallel-execution.md`. |
| Phân loại giao dịch | **S** là giao dịch có thể chứng minh trước những tài khoản sẽ đọc/ghi; **O** là giao dịch phức tạp, chạy bằng EVM tuần tự; **P** là loại cần một bản mô tả thực thi có kiểm chứng và hiện chưa bật. |
| Phạm vi chạy song song | Hỗ trợ hẹp giao dịch chuyển GO thông thường giữa tài khoản EOA: không có calldata, đủ điều kiện gas, phí bằng 0 trong cấu hình hiện tại, đúng chain ID, và không gửi đến precompile hoặc hợp đồng. |
| Cách ly trạng thái | Thêm state fork/overlay để một lần thực thi có vùng trạng thái riêng, tránh ghi trực tiếp vào trạng thái dùng chung. |
| Bộ nhớ phiên bản | Thêm MVMemory có khóa chia shard để theo dõi phiên bản một số dữ liệu tài khoản và bảo vệ truy cập đồng thời. |
| Worker và kiểm tra xung đột | Thêm worker pool, xếp các giao dịch S không xung đột vào cùng đợt, kiểm tra phiên bản và có cơ chế chạy lại khi kết quả không còn phù hợp. |
| Chốt kết quả | Nhận kết quả theo thứ tự giao dịch trong block. Việc ghi bền vững vẫn đi qua luồng Commit hiện có, không để worker tự ghi DB. |
| Cấu hình node | Thêm tùy chọn `--executor=sequential|parallel` và `--workers=N`; mặc định vẫn là `sequential`. |
| Số liệu | Ghi số lượng giao dịch theo loại, worker, số lần chạy lại/hủy và thời gian các pha để hỗ trợ chẩn đoán, đo benchmark. |

## 4. Phần chưa hoàn thành

- **Chưa xử lý song song tổng quát EVM.** Counter, ERC-20, AMM, tạo hợp đồng, gọi hợp đồng và lời gọi lồng nhau hiện vẫn đi tuần tự.
- Loại **P** chưa có bộ phân tích tạo artifact/certificate và bộ kiểm tra đủ để áp dụng an toàn; hiện không có giao dịch nào được xử lý theo P.
- Phạm vi **S** còn nhỏ, chủ yếu cho chuyển tiền EOA đơn giản. Chưa thể suy rộng kết quả này thành tương thích đầy đủ với mọi loại giao dịch Ethereum.
- Vì lịch hiện ưu tiên gom giao dịch không xung đột, test hiện chưa chứng minh hiệu năng tốt trên workload có nhiều ví nóng hoặc nhiều xung đột.
- Tiêu chí “nhanh hơn tuần tự” chưa đạt trong benchmark hiện có.

Do đó, chưa nên ghi M4 là hoàn thành toàn bộ. Có thể mô tả là đã có thiết kế và prototype song song hẹp, đã tích hợp tùy chọn và kiểm thử ban đầu.

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

Kết quả fuzz chạy 15 giây hoàn tất với 654 lần thực thi. Differential harness còn có workload hỗn hợp tất định 10.000 giao dịch để so sánh executor Pluto và geth; đây là workload kiểm thử, không có nghĩa mọi giao dịch trong đó đều được thực thi song song.

## 6. Kết quả benchmark hiện tại

Đo trên workload gồm 128 giao dịch chuyển tiền độc lập, lặp 30 lần, với 4 worker:

| Cách chạy | Thời gian mỗi lượt | Bộ nhớ cấp phát | Số lần cấp phát |
|---|---:|---:|---:|
| Tuần tự | 7.646.983 ns/op | 1.278.245 B/op | 15.657 allocs/op |
| Song song, 4 worker | 8.787.327 ns/op | 1.504.712 B/op | 16.378 allocs/op |

Trong phép đo này, bản song song chậm hơn khoảng 15% và dùng nhiều bộ nhớ hơn. Đây là kết quả cần báo cáo trung thực; hiện chưa có cơ sở nói prototype đã tăng tốc. Cần profiling và thử nhiều kích thước block, số worker, workload ít xung đột/nhiều xung đột trước khi kết luận thêm.

## 7. Cách chạy

Chạy node bằng engine tuần tự mặc định:

```powershell
go run ./cmd/plutod --home .\.pluto-home

Bật prototype song song với 4 worker:

```powershell
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=4


Chạy bài so sánh benchmark tuần tự và song song:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -count=1
```

## 8. Tóm tắt tiến độ theo các mục M4

| Mục | Tiến độ hiện tại |
|---|---|
| M4.1 Thiết kế | Đã viết thiết kế và sơ đồ. |
| M4.2 TxState/overlay | Đã có state fork/overlay cơ bản; cần mở rộng và rà soát cho các luồng EVM tổng quát. |
| M4.3 MVMemory | Đã có bản đầu, dùng cho tập khóa hẹp hiện hỗ trợ. |
| M4.4 Scheduler | Đã có worker pool và xếp lượt cho giao dịch S đơn giản. |
| M4.5 Read/write set và incarnation | Đã có ở phạm vi prototype; chưa bao phủ toàn bộ hành vi EVM. |
| M4.6 Validate/retry | Đã có kiểm tra phiên bản và đường chạy lại; cần thêm workload xung đột để kiểm chứng rộng. |
| M4.7 Commit theo thứ tự | Kết quả được chốt theo thứ tự block và đi qua Commit hiện có; worker không ghi DB. |
| M4.8 Metrics | Đã thêm số liệu theo block để quan sát các pha. |
| M4.9 Tích hợp lựa chọn executor | Đã có cờ chọn tuần tự/song song và số worker; tuần tự vẫn là mặc định. |

## 9. Kết luận

Đã có một prototype thực thi song song có kiểm soát cho chuyển tiền đơn giản, cơ chế quay về EVM tuần tự cho trường hợp chưa hỗ trợ, tùy chọn bật/tắt và bộ kiểm thử đối chiếu. Các bài kiểm tra kỹ thuật đang xanh, nhưng benchmark ban đầu chưa cho thấy tăng tốc. Bước tiếp theo nên là đo và tối ưu scheduler/chi phí worker, mở rộng workload xung đột, rồi mới mở rộng phân tích giao dịch hợp đồng theo P/S. Engine tuần tự tiếp tục là mốc tham chiếu và lựa chọn mặc định.
