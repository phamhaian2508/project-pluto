# Sổ tay lệnh Pluto: node, checkpoint, kiểm thử và gửi tiết kiệm

## 1. Chạy một node

Mở terminal thứ nhất và giữ node chạy.

### 1.1. Tuần tự

~~~powershell
go run ./cmd/plutod --home .\.pluto-home --executor=sequential --workers=1
~~~

SequentialExecutor xử lý lần lượt theo thứ tự giao dịch trong block; đây là mốc chuẩn. Tuần tự cũng là mặc định:

~~~powershell
go run ./cmd/plutod --home .\.pluto-home
~~~

### 1.2. Song song, worker tự điều chỉnh

~~~powershell
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto
~~~

### 1.3. Song song, số worker cố định

~~~powershell
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=4
~~~


### 1.4. Chạy từng chính sách checkpoint

Checkpoint dùng trong thực thi lạc quan song song, đặc biệt khi attempt xung đột và cần chạy lại. P0 không tạo checkpoint; P1 chụp trước lần đọc state; P2 chụp theo chu kỳ opcode; P3 chụp trước lần đọc khóa đã được nhận diện là hay xung đột.

~~~powershell
# P0: mốc không checkpoint
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto --checkpoint-policy=p0

# P1: checkpoint trước lần đọc state
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto --checkpoint-policy=p1

# P2: checkpoint mỗi 100 opcode
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto --checkpoint-policy=p2 --checkpoint-interval=100

# P2: checkpoint mỗi 1.000 opcode
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto --checkpoint-policy=p2 --checkpoint-interval=1000

# P3: checkpoint trước khi đọc khóa nóng đã nhận diện
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto --checkpoint-policy=p3
~~~

Có thể thay workers=auto bằng workers=4. checkpoint-interval tính theo opcode, không phải mili giây; chỉ có ý nghĩa với P2. P1–P3 yêu cầu executor=parallel. Khi đổi chế độ, dừng node bằng Ctrl+C rồi khởi chạy lại. Không chạy hai tiến trình trên cùng home/database. Log lúc khởi động có executor, checkpoint_policy và checkpoint_interval để kiểm tra cờ.

---

## 2. Dùng CLI với node một

Mở terminal thứ hai:

~~~powershell
$env:PLUTO_ETH_RPC_URL = "http://127.0.0.1:8545"
$env:PLUTO_COMET_RPC_URL = "http://127.0.0.1:26657"
~~~

Xem balance và nonce:

~~~powershell
go run ./cmd/plutotx balance 0xDIA_CHI_VI
~~~

Gửi 1 PLU:

~~~powershell
$env:PLUTO_TEST_KEY = "KHOA_RIENG_TAI_KHOAN_TEST"
go run ./cmd/plutotx send -key $env:PLUTO_TEST_KEY -to 0xDIA_CHI_NHAN -plu 1
~~~

Lệnh send lấy nonce và gas price qua RPC. Đợi transaction vào block rồi truy vấn balance lại.

---

## 3. Mạng bốn validator

### 3.1. Tạo cấu hình mạng lần đầu

~~~powershell
.\scripts\setup-pluto-network.ps1
~~~

~~~powershell
.\scripts\setup-pluto-network.ps1 -Root .pluto-network-lab -PortOffset 20
~~~

Khi dùng root mới, thêm -Root .pluto-network-lab cho các lệnh start/stop/dashboard/test. Chọn port offset chưa được tiến trình khác sử dụng.

### 3.2. Chạy bốn node tuần tự

~~~powershell
.\scripts\start-pluto-network.ps1 -Executor sequential
~~~

### 3.3. Chạy bốn node song song

Worker tự điều chỉnh:

~~~powershell
.\scripts\start-pluto-network.ps1 -Executor parallel -Workers auto
~~~

Hoặc cố định 4 worker:

~~~powershell
.\scripts\start-pluto-network.ps1 -Executor parallel -Workers 4
~~~

Tất cả validator phải dùng cùng binary và chế độ executor. Script chạy node nền, log ở .pluto-network\logs. Ethereum RPC validator 1 là http://127.0.0.1:18545; CometBFT RPC là http://127.0.0.1:27757. Validator còn lại dùng các cổng kế tiếp.

### 3.4. Dashboard, dừng và khởi động lại validator

~~~powershell
.\scripts\pluto-network-dashboard.ps1
~~~

Dashboard theo dõi chiều cao block, AppHash ở cùng chiều cao và số liệu mạng. Dừng/chạy lại riêng validator 2:

~~~powershell
.\scripts\stop-pluto-network.ps1 -Node validator2
.\scripts\start-pluto-network.ps1 -Node validator2
~~~

Dừng cả mạng, giữ nguyên dữ liệu:

~~~powershell
.\scripts\stop-pluto-network.ps1
~~~

Bốn validator trên cùng máy vẫn cùng phụ thuộc một máy và một miền quản trị; đây là mạng thử nghiệm cục bộ.

### 3.5. Gửi giao dịch tới validator 1

~~~powershell
$env:PLUTO_ETH_RPC_URL = "http://127.0.0.1:18545"
$env:PLUTO_COMET_RPC_URL = "http://127.0.0.1:27757"
~~~

Lấy tài khoản test do setup tạo mà không in khóa ra màn hình:

~~~powershell
$testAccounts = Get-Content .\.pluto-network\accounts.json -Raw | ConvertFrom-Json
$env:PLUTO_TEST_KEY = $testAccounts[0].private_key
~~~

Kiểm tra balance hoặc chuyển coin như mục 2. File accounts.json chứa private key test, không commit/chia sẻ.

Smoke test và tải giao dịch:

~~~powershell
go run ./cmd/plutosmoke -root .pluto-network -key $env:PLUTO_TEST_KEY
go run ./cmd/plutoload -root .pluto-network -count 50 -out .\.pluto-network\load-report.json
~~~

plutosmoke kiểm tra giao dịch/receipt/trạng thái các validator. plutoload gửi workload chuyển tiền và lưu số liệu JSON.

### 3.6. So sánh tuần tự với song song ở cấp mạng

~~~powershell
.\scripts\compare-executors.ps1 -Transactions 50 -Samples 5
~~~

Lệnh này dựng hai mạng bốn validator mới, một tuần tự và một song song; tự tìm hai dải cổng đang trống; chạy một lượt làm nóng rồi đo các mẫu; lưu báo cáo JSON trong thư mục `.pluto-network\comparisons\<thời-điểm>`. Lệnh không dùng lại dữ liệu của mạng đang chạy bình thường. Có thể mất vài phút vì mỗi lượt khởi động tám tiến trình validator.

Nếu hiện cảnh báo `validatorN RPC is not ready yet`, lượt đo chưa đáng tin. Script sẽ thử gửi workload; nếu RPC vẫn chưa sẵn sàng, lệnh sẽ kết thúc lỗi và dừng các tiến trình do nó quản lý. Xem `logs\validatorN.out.log` và `logs\validatorN.err.log` trong thư mục so sánh tương ứng để tìm nguyên nhân. Chỉ dùng bảng kết quả khi cả hai workload hoàn tất và script in bảng tổng hợp cuối.

---

## 4. Kiểm thử và so sánh

Chạy tại thư mục gốc. count=1 yêu cầu Go chạy lại, v không tắt output chi tiết.

### 4.1. Kiểm thử toàn dự án và bộ harness

~~~powershell
# Toàn bộ package Go
go test ./... -count=1

# Các package nền tảng, ABCI, EVM, RPC và network setup
go test ./internal/config ./internal/plutonetwork ./internal/app ./internal/ethrpc ./internal/evm ./cmd/plutod -count=1

# Chạy toàn bộ bộ kiểm thử harness
go test ./test/harness -count=1
go test ./test/harness -count=1 -v

# Race detector: tìm một số lỗi truy cập bộ nhớ đồng thời
go test -race ./... -count=1
~~~

End-to-end smoke test với Foundry cho node một đang chạy; bài này kiểm tra chuyển giá trị và deploy/call Counter, không thay thế test Savings Vault:

~~~powershell
.\scripts\foundry-e2e.ps1 -RpcUrl $rpc -PrivateKey $env:PLUTO_TEST_KEY
~~~

### 4.2. Ma trận đúng đắn Pluto tuần tự/song song/geth

~~~powershell
go test ./test/harness -run '^TestAllWorkloadsMatchSequentialParallelAndGeth$' -count=1 -v
~~~

So các workload đại diện qua Pluto sequential, Pluto parallel và geth tham chiếu. Có các nhóm chuyển khoản, Counter/contention, access list, ERC-20, AMM, nested call và lỗi thực thi.

### 4.3. Đối chiếu 10.000 giao dịch

~~~powershell
go test ./test/harness -run '^TestDifferentialHarnessTenThousandSeededMixedTransactions$' -count=1 -v
~~~


### 4.4. Báo cáo riêng cho contract contention/access list

~~~powershell
# Nhiều giao dịch cùng gọi một contract
go test ./test/harness -run '^TestContractContentionSequentialParallelReport$' -count=1 -v

# Access list đúng và sai, mỗi test chạy riêng trong cùng lệnh
go test ./test/harness -run '^TestAccessListContractContentionSequentialParallelReport$' -count=1 -v

go test ./test/harness -run '^TestIncorrectAccessListContractContentionSequentialParallelReport$' -count=1 -v

# Cùng contract nhưng ghi các storage slot tách biệt
go test ./test/harness -run '^TestSameContractDisjointAccessListSequentialParallelReport$' -count=1 -v
~~~

Các báo cáo tách workload để quan sát contention, retry và access pattern. Access list Ethereum thông thường không tự chứng minh rằng hợp đồng chỉ truy cập những slot đã kê.

### 4.5. Bảng benchmark tuần tự/song song

~~~powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
~~~

Lưu đầu ra để xem/chèn vào báo cáo:

~~~powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v 2>&1 | Tee-Object -FilePath .\comparison-report.txt
~~~

Đây là đo ExecuteBlock trên workload dựng sẵn, không phải TPS mạng. Benchmark Go chi tiết:

~~~powershell
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchtime=30x -benchmem -count=3

# Workload benchmark bổ sung
go test ./test/harness -run '^$' -bench '^BenchmarkDifferentialWorkloads$' -benchtime=10x -benchmem
~~~

ns/op là thời gian mỗi lượt; B/op là byte cấp phát; allocs/op là số lần cấp phát. B/op không phải RAM đỉnh của tiến trình.

### 4.6. Kiểm thử checkpoint P0–P3

So policy với sequential/geth trên workload đại diện:

~~~powershell
go test ./test/harness -run '^(TestCheckpointPoliciesMatchSequentialAndGethAcrossWorkloads|TestCheckpointPoliciesConcurrentSeededWorkload|TestCheckpointPoliciesAcrossRepresentativeWorkloadsReport|TestCheckpointPolicyCSVExport)$' -count=1 -v
~~~

Test resume/replay trong EVM production path:

~~~powershell
go test ./internal/evm -run '^(TestProductionCheckpointPoliciesResumeAndReplayLessThanP0|TestProductionP3LearnsHotStorageKeyAndResumes|TestProductionResumeAcrossNestedCallMatchesSequential|TestProductionResumePreservesRevertOutOfGasAndLogs)$' -count=1 -v
~~~

Test đơn vị chọn policy, khôi phục, fallback và cấu hình:

~~~powershell
go test ./internal/evm -run 'Checkpoint|Resume|Recovery' -count=1 -v
go test ./internal/app ./cmd/plutod -run 'Checkpoint' -count=1 -v
~~~

Báo cáo các policy qua workload:

~~~powershell
go test ./test/harness -run '^TestCheckpointPoliciesAcrossRepresentativeWorkloadsReport$' -count=1 -v
~~~

Benchmark policy:

~~~powershell
go test ./test/harness -run '^$' -bench '^BenchmarkCheckpointPoliciesCSV$' -benchtime=10x -benchmem
~~~

Xuất CSV metric:

~~~powershell
$env:PLUTO_CHECKPOINT_CSV = '.\checkpoint-metrics.csv'
go test ./test/harness -run '^TestCheckpointPolicyCSVExport$' -count=1 -v
Remove-Item Env:PLUTO_CHECKPOINT_CSV
~~~

CSV có replay opcode/gas, thời gian replay, checkpoint tạo/tái sử dụng, bộ nhớ, abort/retry/resume/fallback. Ít opcode replay không đồng nghĩa chắc chắn nhanh hơn; hãy so thời gian và bộ nhớ.

### 4.7. Fuzz và kiểm tra harness bắt sai lệch

~~~powershell
go test ./test/harness -run '^$' -fuzz '^FuzzDifferentialHarnessSeededMixedBlocks$' -fuzztime=30s -parallel=1
go test ./test/harness -run '^TestHarnessDetectsInjectedMutations$' -count=1 -v
~~~

### 4.8. Lưu toàn bộ đầu ra harness

~~~powershell
go test ./test/harness -count=1 -v 2>&1 | Tee-Object -FilePath .\harness-full-report.txt
~~~

Lệnh này có thể chạy lâu; chọn từng bài test riêng nếu chỉ cần một bảng.

---

## 5. Triển khai và sử dụng hợp đồng gửi tiết kiệm

Các địa chỉ hợp đồng đã triển khai:

| Hợp đồng | Địa chỉ |
|---|---|
| PlutoToken (PTKN) | `0xC56de46BAeBb56D7b0B77Ccb0990798d1a597C8a` |
| PlutoSavingsVault | `0x5386A71e11DC32d78A83dE14aC85B4352a038191` |

Hai địa chỉ chỉ dùng được trên mạng Pluto nơi chúng đã được triển khai. Node đơn và mạng bốn validator có dữ liệu riêng dù cùng chain ID `31337`. MetaMask ký giao dịch; Remix cung cấp giao diện gọi hàm.

Trước khi nạp tiền, kết nối đúng mạng rồi gọi `asset()` trên vault. Giá trị trả về phải đúng bằng địa chỉ PlutoToken ở bảng trên. Cùng chain ID không có nghĩa hai mạng dùng chung dữ liệu hoặc cùng có hợp đồng.

### 5.1. Gắn hợp đồng trong Remix

1. Khởi động đúng node/mạng. Trong MetaMask, chọn đúng RPC và tài khoản có PTKN để gửi cùng PLU trả gas.
2. Trong Remix, chọn **Browser Extension** để kết nối MetaMask.
3. Compile `PlutoToken.sol` bằng Solidity `0.8.20`. Chọn artifact `PlutoToken`, nhập địa chỉ PTKN ở trên vào **At Address** rồi bấm nút đó.
4. Import `PlutoSavingsVault.sol` và `contracts/utils/TwoStepOwnable.sol`, compile với EVM version **Shanghai**. Chọn artifact `PlutoSavingsVault`, nhập địa chỉ vault ở trên vào **At Address** rồi bấm nút đó.
5. Không bấm **Deploy** khi muốn dùng các hợp đồng hiện có. Không chọn `ISavingsAsset` vì đây chỉ là interface.

### 5.2. Nạp quỹ trả lãi

Quỹ lãi phải được nạp trước bằng PTKN. Ví dụ nạp 1 PTKN:

1. Ở giao diện token PTKN, gọi `approve(spender, amount)`:
   - spender: `0x5386A71e11DC32d78A83dE14aC85B4352a038191` (PlutoSavingsVault)
   - amount: 1000000000000000000
2. Xác nhận giao dịch trên MetaMask.
3. Chuyển sang giao diện vault, gọi `fundRewards(amount)` với:
   - amount: 1000000000000000000
4. Xác nhận giao dịch.
`approve` và `fundRewards` là hai giao dịch riêng. Sau đó gọi `rewardReserve()` trên vault để kiểm tra quỹ đã được nạp.

### 5.3. Gửi tiết kiệm

Ví dụ gửi 100 PTKN trong một ngày:

1. Trên token PTKN, gọi `approve(spender, amount)`:
   - spender: địa chỉ vault
   - amount: 100000000000000000000
2. Xác nhận trên MetaMask.
3. Trên vault, gọi `deposit(amount, termSeconds)`:
   - amount: 100000000000000000000
   - termSeconds: 86400 — một ngày
4. Xác nhận trên MetaMask.

Hợp đồng hiện giới hạn kỳ hạn từ 1 đến 365 ngày, nên không gửi kỳ hạn 20 giây được. Lãi suất 500 basis points tương đương 5%/năm; tiền lãi được tính và giữ cố định khi gửi. Ví cần có PLU để trả gas cho từng giao dịch.

Sau khi gửi, ghi lại `positionId` trong event `Deposited`. Có thể gọi `positions(positionId)` trên vault để xem thông tin khoản gửi. Nếu đây là vị thế đầu tiên thì thường ID là 1, nhưng hãy kiểm tra event/kết quả thực tế.

### 5.4. Rút tiền

Khi đã qua thời điểm unlockAt, kết nối đúng tài khoản đã gửi rồi gọi trên vault:
```text
withdraw(positionId)
```
Ví dụ vị thế ID 1: gọi `withdraw(1)`.
Nếu rút trước hạn, gọi:

```text
withdrawEarly(positionId)
```

Mọi giao dịch cần PLU trả gas. Nếu Remix báo lỗi ước lượng gas, đừng ép gửi ngay; kiểm tra allowance, số dư PTKN, `rewardReserve`, kỳ hạn, RPC và tài khoản đang kết nối.

---

## 6. Đọc kết quả

| Trường | Ý nghĩa |
|---|---|
| PASS | Các test được chọn thành công trên code/môi trường hiện tại. |
| ns/op | Thời gian mỗi lượt benchmark executor; thấp hơn thường nhanh hơn. |
| B/op, allocs/op | Cấp phát bình quân; không phải RAM đỉnh. |
| abort/retry/resume | Attempt bị hủy, chạy lại hoặc khôi phục checkpoint. |
| TPS trong load test | Thông lượng quan sát của mạng cục bộ, gồm ảnh hưởng đồng thuận/block timing. |
| AppHash giống ở cùng chiều cao | Validator báo cùng hash trạng thái ứng dụng của block đó. |
| PTKN balance | Số dư token ERC-20 trong contract token. |
| PLU balance | Coin gốc dùng chuyển khoản và trả gas. |

Không kết luận song song luôn nhanh hơn, không gọi benchmark ExecuteBlock là TPS mạng, và không suy rộng một workload thành mọi hợp đồng EVM.
