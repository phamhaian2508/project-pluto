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

`compareAllExecutors` nhận mọi implementation của `evm.Executor`. Hàm này chạy executor song song của Pluto (chế độ tùy chọn) để đối chiếu với geth và executor tuần tự trên các giao dịch chuyển tiền độc lập, giao dịch hợp đồng được chuyển về xử lý tuần tự và block trộn 10.000 giao dịch có thể tái lập. Bộ phân tích P/O/S hiện chỉ chạy song song các giao dịch chuyển EOA tĩnh trong phạm vi đã chứng minh; tạo hợp đồng và gọi hợp đồng vẫn chạy tuần tự theo loại O.

So sánh engine tuần tự với engine song song dùng 1, 2, 4 và 8 worker trên block có 128, 512 và 1.024 giao dịch chuyển tiền không xung đột:

```powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -benchmem -count=3
```

Thời gian tạo Pebble và nạp trạng thái ban đầu không tính vào thời gian benchmark; mỗi lượt vẫn bắt đầu từ trạng thái mới. Đây là benchmark chẩn đoán trên máy cục bộ, không phải bằng chứng rằng toàn bộ EVM đã tăng tốc. Khi chia sẻ kết quả, hãy ghi lại máy, phiên bản Go, số worker, seed và workload.

`mutation_test.go` chứa các executor kiểm thử được cố ý viết sai. Các test xác nhận bộ so sánh phát hiện được giao dịch không hợp lệ bị chấp nhận và giá trị storage bị commit sai. Những executor lỗi này chỉ dùng trong test, không sửa executor production.
