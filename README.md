# Pluto

Pluto là dự án blockchain viết bằng Go. CometBFT quản lý mạng validator và đồng thuận block; EVM của go-ethereum thực thi giao dịch và hợp đồng thông minh; Pebble lưu trạng thái đã commit; node cung cấp một phần giao diện Ethereum JSON-RPC.

Tài liệu này là **sổ tay lệnh tổng quát** để biên dịch, kiểm thử, chạy một node, chạy mạng bốn validator, trộn executor giữa các node, gửi giao dịch và chạy các công cụ so sánh. Các lệnh được viết cho PowerShell và chạy từ thư mục gốc của repository.

## 1. Chuẩn bị và kiểm tra nhanh

```powershell
# Xem phiên bản Go đang dùng
go version

# Biên dịch tất cả package Go
go build ./...

# Chạy tất cả test trong repository, không dùng kết quả test đã cache
go test ./... -count=1
```

Để xem các cờ của node:

```powershell
go run ./cmd/plutod --help
```

## 2. Chạy một node

Node một máy mặc định dùng EVM chain ID `31337`, Ethereum JSON-RPC tại `http://127.0.0.1:8545` và CometBFT RPC tại `http://127.0.0.1:26657`. Khi chạy tiến trình trực tiếp, nhấn `Ctrl+C` trong cửa sổ đó để dừng.

### Các chế độ thực thi

Mỗi lệnh dưới đây khởi chạy một node với cùng thư mục dữ liệu `.\.pluto-home`; mỗi lần chỉ chạy **một lệnh**:

```powershell
# Tuần tự: giao dịch được thực thi theo đúng thứ tự trong block
go run ./cmd/plutod --home .\.pluto-home --executor=sequential --workers=1

# OCC cửa sổ trượt; đây là tên chuẩn của chế độ
go run ./cmd/plutod --home .\.pluto-home --executor=occ --workers=auto

# Grouped: gom một số mẫu giao dịch đã nhận diện, hậu kiểm rồi mới áp dụng
go run ./cmd/plutod --home .\.pluto-home --executor=grouped --workers=auto

# Predicted-OCC: dự đoán điểm nóng để xếp lịch, sau đó vẫn xác thực OCC
go run ./cmd/plutod --home .\.pluto-home --executor=predicted-occ --workers=auto
```

### Checkpoint cho một node

Checkpoint là lựa chọn bổ sung cho một số executor OCC, không phải một executor riêng. `fixed`, `history` và `adaptive` chỉ dùng với `occ` hoặc `predicted-occ`; `sequential` và `grouped` phải để `off`.

```powershell
# OCC, không chụp checkpoint
go run ./cmd/plutod --home .\.pluto-home --executor=occ --workers=auto --checkpoint=off

# OCC, chụp theo chu kỳ 100 opcode
go run ./cmd/plutod --home .\.pluto-home --executor=occ --workers=auto --checkpoint=fixed --checkpoint-interval=100

# OCC, chọn checkpoint từ lịch sử xung đột
go run ./cmd/plutod --home .\.pluto-home --executor=occ --workers=auto --checkpoint=history

# Predicted-OCC, chính sách thích ứng; yêu cầu model.json đã hiệu chuẩn
go run ./cmd/plutod --home .\.pluto-home --executor=predicted-occ --workers=auto --checkpoint=adaptive --checkpoint-model .\results\phase3\model.json
```

Các cờ `--checkpoint-budget-bytes`, `--checkpoint-interval`, `--chain-id`, `--eth-rpc`, `--log-level` và `--timeout-commit` có thể bổ sung khi cần. Dùng `--checkpoint=adaptive` mà thiếu `--checkpoint-model` sẽ làm node báo lỗi cấu hình.

Các tên chính sách cũ vẫn được CLI chấp nhận qua `--checkpoint-policy`: `p1` là checkpoint trước lần đọc trạng thái; `p2` là checkpoint định kỳ và cần `--checkpoint-interval`; `p3` là checkpoint theo lịch sử xung đột. Tên hiện dùng tương ứng là `fixed` cho `p2`, `history` cho `p3`, và `adaptive` cho chính sách thích ứng.

## 3. Chạy mạng bốn validator

Mỗi validator có home và database Pebble riêng. `network.json` ghi các địa chỉ RPC; với cấu hình mặc định Ethereum RPC của validator1 thường là `18545` và tăng một cổng cho từng node. Hãy đọc `network.json` thay vì giả định cổng nếu đã đặt `PortOffset`.

### Tạo mạng lần đầu

```powershell
# Tạo genesis, khóa, cấu hình và metadata cho bốn validator dưới .pluto-network
.\scripts\setup-pluto-network.ps1 -Root ".\.pluto-network"
```

### Chạy cả bốn node cùng một executor

```powershell
# Chạy cả bốn validator tuần tự
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor sequential -Workers 1

# Chạy cả bốn validator bằng OCC cửa sổ trượt
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor occ -Workers auto

# Chạy cả bốn validator bằng grouped
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor grouped -Workers auto

# Chạy cả bốn validator bằng predicted-OCC
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor predicted-occ -Workers auto
```

Khi không truyền `-Node`, script khởi chạy cả bốn validator. Script dùng lại binary `<Root>\bin\plutod.exe` nếu file đó đã có; sau khi cập nhật mã nguồn hoặc nếu log báo executor không được hỗ trợ, hãy dừng mạng rồi thêm `-Rebuild` để biên dịch binary mới:

```powershell
.\scripts\stop-pluto-network.ps1 -Root ".\.pluto-network"
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor grouped -Workers auto -Rebuild
```

`-Rebuild` chỉ thay binary node; nó không tạo lại genesis hay xóa database của validator.

### Theo dõi, xem log và dừng validator

```powershell
# Mở dashboard và cập nhật liên tục mỗi giây
.\scripts\pluto-network-dashboard.ps1 -Root ".\.pluto-network"

# Chỉ lấy một lần trạng thái rồi thoát
.\scripts\pluto-network-dashboard.ps1 -Root ".\.pluto-network" -Once

# Cập nhật dashboard mỗi 3 giây
.\scripts\pluto-network-dashboard.ps1 -Root ".\.pluto-network" -RefreshSeconds 3

# Đọc log lỗi mới nhất của validator1
Get-Content .\.pluto-network\logs\validator1.err.log -Tail 100

# Theo dõi log lỗi liên tục
Get-Content .\.pluto-network\logs\validator1.err.log -Tail 100 -Wait

# Dừng tất cả validator do script quản lý; database vẫn được giữ lại
.\scripts\stop-pluto-network.ps1 -Root ".\.pluto-network"

# Chỉ dừng validator4
.\scripts\stop-pluto-network.ps1 -Root ".\.pluto-network" -Node validator4
```


Sau khi sửa mã Go, dừng các node rồi biên dịch lại binary và khởi động:

```powershell
# Phải dừng các node trước khi yêu cầu script dựng lại plutod.exe
.\scripts\stop-pluto-network.ps1 -Root ".\.pluto-network"
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Executor occ -Workers auto -Rebuild
```

### Chạy một validator trong mạng

Tham số `-Node` chỉ khởi động node được nêu; nó không tạo mạng mới và cũng không đổi genesis:

```powershell
# Chỉ khởi chạy validator1 bằng OCC
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network" -Node validator1 -Executor occ -Workers auto
```

Có thể dùng `-Node validator2`, `validator3` hoặc `validator4`. Dashboard vẫn là cách kiểm tra xem đủ bốn validator đã online và hội tụ cùng chiều cao/AppHash hay chưa.

## 4. Chạy cấu hình hỗn hợp giữa các validator

Để thử cấu hình hỗn hợp, tạo một root riêng. Không dùng chung root với mạng đang chạy:

```powershell
# Tạo bộ dữ liệu riêng cho mạng hỗn hợp
.\scripts\setup-pluto-network.ps1 -Root ".\.pluto-network-mixed"

# Tạo model thích ứng cho chính sách adaptive tại bước khởi chạy validator4
go run ./cmd/pluto-lab calibrate --suite phase3 --out .\.pluto-network-mixed\model.json

# validator1: tuần tự, không checkpoint
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network-mixed" -Node validator1 -Executor sequential -Workers 1 -Checkpoint off

# validator2: OCC với checkpoint cố định mỗi 100 opcode
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network-mixed" -Node validator2 -Executor occ -Workers auto -Checkpoint fixed -CheckpointInterval 100

# validator3: OCC với checkpoint theo lịch sử
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network-mixed" -Node validator3 -Executor occ -Workers auto -Checkpoint history

# validator4: OCC với checkpoint thích ứng; model phải tồn tại
.\scripts\start-pluto-network.ps1 -Root ".\.pluto-network-mixed" -Node validator4 -Executor occ -Workers auto -Checkpoint adaptive -CheckpointModel ".\.pluto-network-mixed\model.json"

# Xác nhận trạng thái và AppHash của cả bốn node
.\scripts\pluto-network-dashboard.ps1 -Root ".\.pluto-network-mixed"

# Dừng các node trong mạng hỗn hợp
.\scripts\stop-pluto-network.ps1 -Root ".\.pluto-network-mixed"
```


## 5. Gửi giao dịch và kiểm tra số dư

### Chuyển PLU bằng công cụ Pluto

`plutotx` đọc số dư/nonce qua CometBFT RPC và gửi giao dịch đã ký. Với một node, RPC mặc định là CometBFT `26657` và Ethereum `8545`:

```powershell
# Đặt private key của tài khoản thử nghiệm cục bộ trong môi trường; không ghi khóa thật vào README hoặc Git
$env:PLUTO_TEST_KEY = "<private key cua tai khoan test cuc bo>"

# Xem số dư và nonce của địa chỉ qua ABCI Query
go run ./cmd/plutotx balance 0xDiaChiVi

# Chuyển 1 PLU; -plu nhận số PLU nguyên, không phải wei
go run ./cmd/plutotx send -key $env:PLUTO_TEST_KEY -to 0xDiaChiNhan -plu 1

# Triển khai contract bằng creation bytecode dạng file hoặc chuỗi hex
go run ./cmd/plutotx deploy -key $env:PLUTO_TEST_KEY -bytecode .\contracts\Counter.bin

# Gửi calldata đã mã hóa ABI tới contract; ví dụ selector increment() là d09de08a
go run ./cmd/plutotx call -key $env:PLUTO_TEST_KEY -to 0xDiaChiContract -data d09de08a

# Đọc storage slot 0 của contract qua ABCI Query (chỉ đọc, không tốn gas)
go run ./cmd/plutotx storage 0xDiaChiContract 0
```

`plutotx deploy` nhận bytecode tạo contract, không tự biên dịch Solidity hoặc nhận tham số constructor. Để triển khai hợp đồng có constructor như `PlutoSavingsVault`, dùng Foundry hoặc Remix.

Nếu dùng nhiều node hoặc thay cổng, lấy endpoint thực tế từ `network.json`:

```powershell
# Đọc endpoint CometBFT và Ethereum của validator1 từ metadata mạng
$network = Get-Content .\.pluto-network\network.json -Raw | ConvertFrom-Json
$env:PLUTO_COMET_RPC_URL = $network.nodes[0].comet_rpc
$env:PLUTO_ETH_RPC_URL = $network.nodes[0].ethereum_rpc

# Các lệnh plutotx sau đó dùng đúng hai endpoint vừa đọc
go run ./cmd/plutotx balance 0xDiaChiVi
go run ./cmd/plutotx send -key $env:PLUTO_TEST_KEY -to 0xDiaChiNhan -plu 1
```

`1 PLU = 10^18 wei`. PLU là coin gốc dùng trả phí mạng; PTKN là token ERC-20 trong hợp đồng, hai loại tài sản này khác nhau.

### Gửi tải giao dịch thử nghiệm lên mạng bốn validator

```powershell
# Gửi 10 giao dịch chuyển native coin từ các tài khoản test đã được cấp số dư trong genesis
go run ./cmd/plutoload -root .\.pluto-network -count 10

# Gửi 50 giao dịch và lưu báo cáo JSON để phân tích sau
go run ./cmd/plutoload -root .\.pluto-network -count 50 -out .\.pluto-network\load-report.json
```

`-count` nhận từ 1 đến 64. Công cụ dùng tài khoản thử nghiệm trong `accounts.json`; tệp này chứa private key, chỉ dùng cục bộ và không gửi lên Git.

### Dùng Foundry và Cast

Yêu cầu cài `forge` và `cast`, đồng thời `PLUTO_TEST_KEY` phải là khóa của tài khoản thử nghiệm có tiền trên mạng đang chọn:

```powershell
# Biên dịch các hợp đồng Solidity theo foundry.toml
forge build

# Xem EVM chain ID của validator1 trên mạng bốn validator
cast chain-id --rpc-url http://127.0.0.1:18545

# Xem số dư PLU (coin gốc) của một địa chỉ
cast balance 0xDiaChiVi --rpc-url http://127.0.0.1:18545

# Chuyển 1 PLU tới địa chỉ nhận qua JSON-RPC của validator1
cast send 0xDiaChiNhan --value 1ether --private-key $env:PLUTO_TEST_KEY --rpc-url http://127.0.0.1:18545

# Chạy smoke test: chuyển coin, triển khai Counter, gọi increment và kiểm tra kết quả
.\scripts\foundry-e2e.ps1 -RpcUrl "http://127.0.0.1:18545" -PrivateKey $env:PLUTO_TEST_KEY

# Chạy kiểm tra tích hợp mạng: smoke test, triển khai PlutoToken và gửi tải giao dịch
.\scripts\test-pluto-network.ps1 -Root ".\.pluto-network" -LoadTransactions 10
```

RPC `18545` là ví dụ cho validator1 với port offset mặc định. Nếu cấu hình mạng khác, dùng `$network.nodes[0].ethereum_rpc` như phần trên. Chỉ dùng khóa thử nghiệm; không nhập private key ví cá nhân vào lệnh hoặc lưu trong tệp dự án.

### Biên dịch, triển khai và dùng hợp đồng gửi tiết kiệm

Các lệnh dưới đây dùng Foundry/Cast với node một máy. Khi dùng mạng bốn validator, thay RPC bằng endpoint trong `network.json` như phần trước. `PlutoToken` là token ERC-20 PTKN; PLU mới là coin gốc dùng trả phí giao dịch.

```powershell
# Khai báo RPC và khóa của ví thử nghiệm đã có PLU trả gas
$env:PLUTO_ETH_RPC_URL = "http://127.0.0.1:8545"
$env:PLUTO_TEST_KEY = "<private key cua tai khoan test cuc bo>"

# Biên dịch Solidity theo foundry.toml
forge build

# Triển khai token PTKN; người triển khai nhận tổng cung ban đầu
forge create contracts/PlutoToken.sol:PlutoToken --rpc-url $env:PLUTO_ETH_RPC_URL --private-key $env:PLUTO_TEST_KEY --broadcast

# Lưu địa chỉ token vừa triển khai để làm tham số cho vault
$env:PTKN_ADDRESS = "0xDiaChiPlutoToken"

# Triển khai vault: tham số thứ nhất là địa chỉ PTKN, 500 tương đương lãi suất 5%/năm
forge create contracts/PlutoSavingsVault.sol:PlutoSavingsVault --rpc-url $env:PLUTO_ETH_RPC_URL --private-key $env:PLUTO_TEST_KEY --broadcast --constructor-args $env:PTKN_ADDRESS 500

# Lưu địa chỉ vault lấy từ kết quả triển khai
$env:VAULT_ADDRESS = "0xDiaChiPlutoSavingsVault"
```

Nếu đã triển khai token và vault trên Remix, bỏ qua hai lệnh `forge create` và điền địa chỉ hợp đồng thực tế vào hai biến môi trường. Luôn dùng đúng mạng/RPC đã triển khai hợp đồng.

```powershell
# Nạp 1 PTKN vào quỹ lãi: trước hết cấp allowance cho vault
cast send $env:PTKN_ADDRESS "approve(address,uint256)" $env:VAULT_ADDRESS 1000000000000000000 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL

# Vault kéo 1 PTKN từ ví người gọi vào rewardReserve
cast send $env:VAULT_ADDRESS "fundRewards(uint256)" 1000000000000000000 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL

# Đọc quỹ lãi hiện có; kết quả tính theo đơn vị nhỏ nhất của PTKN (18 số thập phân)
cast call $env:VAULT_ADDRESS "rewardReserve()(uint256)" --rpc-url $env:PLUTO_ETH_RPC_URL

# Cấp allowance cho khoản gửi 100 PTKN
cast send $env:PTKN_ADDRESS "approve(address,uint256)" $env:VAULT_ADDRESS 100000000000000000000 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL

# Gửi 100 PTKN trong một ngày (86400 giây)
cast send $env:VAULT_ADDRESS "deposit(uint256,uint64)" 100000000000000000000 86400 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL

# Xem ID vị thế kế tiếp; để biết ID vừa tạo, xem event Deposited trong receipt giao dịch
cast call $env:VAULT_ADDRESS "nextPositionId()(uint256)" --rpc-url $env:PLUTO_ETH_RPC_URL

# Đọc dữ liệu vị thế; thay 1 bằng positionId lấy từ event Deposited
cast call $env:VAULT_ADDRESS "positions(uint256)(address,uint64,uint64,uint256,uint256,bool)" 1 --rpc-url $env:PLUTO_ETH_RPC_URL
```

Khi rút, chọn **một** trong hai cách sau; `withdraw` chỉ dùng sau `unlockAt`, còn `withdrawEarly` dùng trước hạn. Không gọi cả hai cho cùng một `positionId`:

```powershell
# Rút đúng hạn: nhận lại gốc và lãi
cast send $env:VAULT_ADDRESS "withdraw(uint256)" 1 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL
```

```powershell
# Rút trước hạn: nhận lại gốc và từ bỏ toàn bộ lãi của vị thế
cast send $env:VAULT_ADDRESS "withdrawEarly(uint256)" 1 --private-key $env:PLUTO_TEST_KEY --rpc-url $env:PLUTO_ETH_RPC_URL
```

`approve` và `fundRewards` là hai giao dịch riêng; cũng vậy, phải cấp allowance trước `deposit`. Vault hiện chỉ nhận kỳ hạn từ 1 đến 365 ngày. Ví cần có PLU để trả gas cho từng giao dịch. Các lệnh `cast send` ở trên tạo giao dịch có thật trên mạng được chỉ định; chỉ dùng với token, vault và khóa thử nghiệm bạn kiểm soát.

## 6. So sánh executor và tải mạng

### So sánh hai mạng bốn validator sạch

```powershell
# Tạo hai mạng mới trên các cổng còn trống, đo tuần tự và OCC, rồi ghi báo cáo
.\scripts\compare-executors.ps1 -Transactions 50 -Samples 5
```

Script này tạo root riêng dưới `.pluto-network\comparisons`, chạy sequential và OCC trên hai mạng bốn validator khởi tạo độc lập, thực hiện lượt khởi động, đo nhiều mẫu rồi dừng các node do script quản lý. `-Transactions` nhận 1–64; `-Samples` nhận 3–20. Báo cáo JSON được giữ trong thư mục lần chạy. Đây là phép đo mạng cục bộ; kết quả phụ thuộc máy và không đại diện TPS của mạng sản xuất.

### So sánh executor trực tiếp trong harness

```powershell
# Báo cáo workload giao dịch độc lập: tuần tự so với OCC
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v

# Báo cáo tranh chấp cùng contract và các dạng access list
go test ./test/harness -run '^TestContractContentionSequentialParallelReport$' -count=1 -v
go test ./test/harness -run '^TestAccessListContractContentionSequentialParallelReport$' -count=1 -v
go test ./test/harness -run '^TestIncorrectAccessListContractContentionSequentialParallelReport$' -count=1 -v
go test ./test/harness -run '^TestSameContractDisjointAccessListSequentialParallelReport$' -count=1 -v

# Benchmark Go cho giao dịch độc lập
go test ./test/harness -run '^$' -bench '^BenchmarkSequentialVsParallelIndependentTransfers$' -benchmem -count=1
```

Các tên test Go ở trên vẫn có chữ `Parallel` để giữ nguyên lệnh kiểm thử đã công bố; chúng đo `SequentialExecutor` với OCC/`ParallelExecutor`, không phải một executor thứ hai tên parallel. Các báo cáo này không đo hiệu năng riêng cho `grouped` hoặc `predicted-occ`. Xem [README kiểm thử](test/harness/README.md) để biết phạm vi từng bài.

## 7. Công cụ hiệu chuẩn và kiểm chứng `pluto-lab`

Benchmark `phase3 --matrix` bắt buộc đọc model thích ứng từ đường dẫn truyền trong `--model`. Vì vậy, phải chạy `calibrate` thành công trước; lệnh `bench` không tự tạo file này.

```powershell
# In hướng dẫn các lệnh calibrate, verify, bench và gen-swap-pools
go run ./cmd/pluto-lab --help

# Bước 1: tạo thư mục results\phase3 và ghi model.json vào đó
go run ./cmd/pluto-lab calibrate --suite phase3 --out .\results\phase3\model.json
```

Chờ lệnh hiệu chuẩn in `calibrate complete` rồi kiểm tra file đã có:

```powershell
Test-Path .\results\phase3\model.json
```

Kết quả phải là `True`. Sau đó mới chạy benchmark:

```powershell
# Bước 2: dùng model vừa tạo để chạy ma trận benchmark phase3
go run ./cmd/pluto-lab bench --suite phase3 --matrix --model .\results\phase3\model.json --out .\results\phase3-bench
```

Nếu `calibrate` báo lỗi, xử lý lỗi đó trước; nếu `Test-Path` trả `False`, hãy kiểm tra bạn đang ở thư mục gốc repository và đường dẫn `--out` có đúng không. Không cần tự tạo `model.json` rỗng.

Các lệnh kiểm chứng fixture:

```powershell
# Đối chiếu fixture phase1 với tuần tự và mọi chế độ executor được công cụ hỗ trợ
go run ./cmd/pluto-lab verify --suite phase1 --case all --mode all --checkpoint off

# Kiểm chứng fixture phase3 với checkpoint thích ứng; cần model đã hiệu chuẩn và binary evm của Geth
go run ./cmd/pluto-lab verify --suite phase3 --case all --mode occ --checkpoint adaptive --model .\results\phase3\model.json --workers 4
```

Các lệnh `verify` nhận suite, case, executor, checkpoint và tùy chọn đường dẫn binary `evm` của Geth. Xem cú pháp chi tiết bằng `go run ./cmd/pluto-lab --help`; một số kiểm chứng cần `evm` có trên `PATH` hoặc truyền `--evm-bin`.

## 8. Test hệ thống

```powershell
# Chạy toàn bộ test Go của repository
go test ./... -count=1

# Chạy toàn bộ harness so sánh giao dịch/EVM/checkpoint
go test ./test/harness -count=1 -v

# Chạy các test mạng bốn validator
go test ./internal/plutonetwork -count=1 -v

# Chạy riêng test kịch bản validator hỗn hợp executor/checkpoint
go test ./internal/plutonetwork -run '^TestFourValidatorsCatchUpAndMixedExecutors$' -count=1 -v
```

Các bài đối chiếu riêng cho OCC, grouped và predicted-OCC với sequential, cùng toàn bộ test harness, được ghi trong [test/harness/README.md](test/harness/README.md). `PASS` chỉ xác nhận những assertion thuộc test đã chạy; test harness không đo thời gian đồng thuận CometBFT.

## 9. Dữ liệu, an toàn và giới hạn

- `.pluto-home` là home của node đơn trong các ví dụ; `.pluto-network` chứa bốn home validator, `network.json`, log và `accounts.json` của mạng thử nghiệm.
- `setup-pluto-network.ps1` từ chối ghi đè thư mục mạng đã có dữ liệu. `stop-pluto-network.ps1` dừng tiến trình nhưng giữ nguyên database.
- Không chạy hai node trên cùng một thư mục home. Không chạy các lệnh `setup` vào root đang dùng.
- Không đưa private key, `accounts.json`, khóa validator, dữ liệu database hoặc báo cáo cục bộ có thông tin nhạy cảm lên Git.
- Pluto hiện là dự án nghiên cứu/đồ án; hỗ trợ JSON-RPC và tương thích Ethereum có phạm vi giới hạn. Không dùng các khóa thử nghiệm hay mạng cục bộ này cho tài sản thật.

## 10. Tài liệu mã nguồn

- [README test harness](test/harness/README.md): lệnh kiểm thử, so sánh executor và checkpoint.
- [Thiết kế các chế độ thực thi](docs/bao-cao-thiet-ke-cac-che-do-thuc-thi-giao-dich-pluto.md): tuần tự, grouped, OCC, predicted-OCC và checkpoint.
- [Kiến trúc node và mạng validator](docs/bao-cao-kien-truc-tong-the-node-pluto-va-mang-da-validator.md): các thành phần, luồng block và mạng nhiều node.
- [Báo cáo nội dung tích hợp từ main](docs/bao-cao-noi-dung-tich-hop-tu-main.md): executor, checkpoint, lab và các kiểm thử mới.
