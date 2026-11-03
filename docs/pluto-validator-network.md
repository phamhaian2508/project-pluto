# Mạng Pluto nhiều validator

Tài liệu này hướng dẫn dựng mạng CometBFT 4 validator riêng cho mạng Pluto, chạy song song, kiểm tra `AppHash`, quan sát TPS/retry, gửi giao dịch, deploy hợp đồng và khởi động lại validator.

## 1. Chức năng mạng Pluto nhiều validator

- **Dựng mạng nhiều validator:** tạo bốn node có khóa, database và cổng riêng; cùng dùng một genesis, một `app_state` và cùng danh sách validator; kết nối bằng `persistent_peers`.
- **Cấu hình node:** `plutod` đọc `config/<node>/config.toml`; các cờ `--log-level` và `--timeout-commit` có thể ghi đè cấu hình khi chạy.
- **Kiểm tra đồng thuận:** đối chiếu `AppHash` ở cùng chiều cao giữa các validator; dashboard báo lỗi và dừng các tiến trình mạng do script quản lý nếu hash khác nhau.
- **Kiểm tra giao dịch và hợp đồng:** chuyển PLU, deploy/gọi hợp đồng, tạo tải giao dịch và khởi động lại một validator.
- **Theo dõi hiệu năng:** hiển thị TPS đã commit và số lần executor chạy lại theo block.

Trong kế hoạch đồ án, nhóm yêu cầu này được đánh mã M6.1–M6.6; “M6” là mã theo dõi tiến độ, không phải tên chức năng của Pluto.
Mạng bốn node trên cùng một máy phù hợp để kiểm tra vận hành và kiểm tra trạng thái đồng nhất. Nó chưa phải bốn bên độc lập về quản trị hoặc hạ tầng.

## 2. Yêu cầu

- Windows PowerShell và Go 1.24.1.
- Foundry (`forge`, `cast`) nếu muốn chạy script kiểm tra giao dịch/hợp đồng.
- Các cổng P2P `27656–27659`, CometBFT RPC `27757–27760`, Ethereum RPC `18545–18548` phải còn trống. Nếu các cổng này bận, hãy chọn `-PortOffset` khác.

## 3. Tạo mạng lần đầu

Từ thư mục gốc repo:

```powershell
.\scripts\setup-pluto-network.ps1
```

Script tạo `.pluto-network/validator1` đến `validator4`, `network.json`, cùng genesis/config/khóa riêng cho từng node. Genesis giống nhau ở cả bốn home, cấp số dư thử nghiệm cho địa chỉ `0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266` và 64 tài khoản ngẫu nhiên chỉ dùng để tạo tải chuyển tiền độc lập. Khóa riêng của 64 tài khoản nằm trong `<Root>\accounts.json`; file này chứa bí mật thử nghiệm, không chia sẻ hoặc commit.

Setup **không xóa hoặc sửa** `.pluto-home`. Lệnh setup từ chối ghi vào thư mục đích đã có nội dung. Muốn tạo mạng thứ hai để so sánh, dùng một thư mục mới, ví dụ:

```powershell
.\scripts\setup-pluto-network.ps1 -Root .pluto-network-sequential -PortOffset 20
```

Khóa validator và database của mạng thử nghiệm nằm trong thư mục bị Git bỏ qua `.pluto-network/`. Không commit khóa trong các thư mục validator.

Nếu `.pluto-network` đã có mạng từ lần chạy trước, đừng chạy setup ghi đè. Tạo một home mới dưới thư mục đó và chọn offset cổng riêng:

```powershell
.\scripts\setup-pluto-network.ps1 -Root .pluto-network\secondary -PortOffset 20
```

Trong các lệnh tiếp theo, thêm `-Root .pluto-network\secondary` để dùng đúng mạng mới.

## 4. Khởi động và dừng bốn node

Khởi động executor song song với worker tự điều chỉnh:

```powershell
.\scripts\start-pluto-network.ps1
```

Node 1 mở Ethereum RPC `http://127.0.0.1:18545`; các node còn lại lần lượt dùng cổng `18546`, `18547`, `18548`. CometBFT RPC dùng cổng `27757–27760`. Log nằm trong `.pluto-network/logs/`; mỗi node có DB riêng trong home riêng.

Lệnh khởi động đợi bốn node cùng online và có `AppHash` khớp tại một chiều cao chung trước khi báo thành công. Nếu mạng không hội tụ, script dừng các tiến trình mạng do script quản lý và giữ nguyên dữ liệu.

Khởi động với log chi tiết hơn hoặc thời gian commit khác bằng cờ của script; nếu bỏ cờ, cấu hình trong `config.toml` được dùng:

```powershell
.\scripts\start-pluto-network.ps1 -LogLevel debug -TimeoutCommit 2s
```

Hoặc chạy trực tiếp một node đã cấu hình sẵn, khi node đó chưa được script khởi động:

```powershell
go run ./cmd/plutod --home .\.pluto-network\validator1 --executor=parallel --workers=auto --log-level=debug --timeout-commit=2s --eth-rpc=127.0.0.1:18545
```

Script `start-pluto-network.ps1` chạy tiến trình nền ẩn và ghi PID để dừng đúng các node do script khởi động. Dừng cả mạng:

```powershell
.\scripts\stop-pluto-network.ps1
```

Lệnh dừng chỉ tác động đến PID có trong `.pluto-network/processes.json` khi đường dẫn binary và home còn khớp. Dữ liệu node không bị xóa.

## 5. Theo dõi đồng thuận, AppHash, TPS và retry

Mở dashboard ở một PowerShell riêng:

```powershell
.\scripts\pluto-network-dashboard.ps1
```

Dashboard đọc:

- Chiều cao block từ `/abci_info`, sau đó đọc `app_hash` của `/commit?height=H` tại cùng một chiều cao `H` trên cả bốn CometBFT RPC. Cách lấy header cùng chiều cao tránh so hai block khác nhau khi node đang bắt kịp.
- Số giao dịch đã commit trong khoảng cập nhật để tính TPS quan sát được trên mạng.
- `retryCount` từ `pluto_getBlockMetrics` của node 1 rồi cộng theo các block mới.

TPS này bao gồm nhịp tạo block/đồng thuận, không phải chỉ thời gian EVM. Các số đo executor cho từng block được lưu cùng metadata block. Chúng không thay thế benchmark có kiểm soát.

Dashboard lấy chiều cao thấp nhất hiện có làm `H`, rồi so header commit tại chính chiều cao đó. Nếu một node chưa có block `H`, dashboard đợi. Nếu đủ bốn node có block `H` nhưng hash khác, dashboard báo lỗi và gọi `stop-pluto-network.ps1` để dừng các node đã được quản lý. Đây là monitor bên ngoài tiến trình đồng thuận; không thêm lời gọi mạng vào lúc executor đang thực thi block.

RPC riêng để đọc metrics một block:

```powershell
$body = @{ jsonrpc = "2.0"; id = 1; method = "pluto_getBlockMetrics"; params = @("0x1") } | ConvertTo-Json -Compress
Invoke-RestMethod -Uri "http://127.0.0.1:18545" -Method Post -ContentType "application/json" -Body $body
```

Kết quả có `executor`, `workers`, `transactionCount`, `retryCount` và `executionNanos`.

## 6. Chạy giao dịch và hợp đồng

Script cần một khóa test cục bộ đã được cấp coin ở genesis. Lấy khóa từ danh sách tài khoản do bước setup tạo ra; không dùng khóa ví thật:

```powershell
$testAccounts = Get-Content .\.pluto-network\accounts.json -Raw | ConvertFrom-Json
$env:PLUTO_TEST_KEY = $testAccounts[0].private_key
.\scripts\test-pluto-network.ps1 -LoadTransactions 50
```

Script kiểm tra chain ID `31337`, chạy smoke test chuyển coin và deploy/gọi Counter, build và deploy `PlutoToken`, xác nhận mã token đọc được ở cả bốn RPC, rồi gửi và xác nhận các giao dịch tải qua validator 1. Mở dashboard trong một cửa sổ khác để xem block/hash/TPS/retry khi kiểm tra.

Không cần Foundry để chạy smoke transfer và tải chuyển tiền từ 64 tài khoản ngẫu nhiên đã được cấp tiền trong genesis:

```powershell
go run ./cmd/plutosmoke -root .pluto-network -key $env:PLUTO_TEST_KEY
go run ./cmd/plutoload -root .pluto-network -count 50 -out .pluto-network\load-report.json
```

`plutosmoke` gửi một giao dịch thật từ tài khoản Anvil thử nghiệm rồi kiểm tra receipt, số dư nhận và `AppHash` trên cả bốn node. `plutoload` gửi tối đa 64 chuyển khoản độc lập (mỗi giao dịch từ một tài khoản khác nhau), đợi receipt thành công và ghi thời gian, TPS, retry cùng metrics executor ra JSON.

Để so sánh tuần tự và song song qua mạng bốn validator, chạy:

```powershell
.\scripts\compare-executors.ps1 -Transactions 50 -Samples 5
```

Lệnh này dựng hai genesis mới, dùng cổng tách biệt, một mạng tuần tự và một mạng song song; mỗi chế độ chạy một lượt làm nóng rồi 5 mẫu đo. Nó in median thời gian/TPS và lưu báo cáo thô trong `.pluto-network\comparisons\<thời-gian>\`. Các node dừng lại sau phép đo nhưng dữ liệu và báo cáo được giữ nguyên. Chỉ số mạng bao gồm đồng thuận và thời gian tạo block, nên hãy đọc cùng benchmark executor bên dưới.

Để chạy thêm báo cáo benchmark executor tuần tự/song song có sẵn trong harness:

```powershell
.\scripts\test-pluto-network.ps1 -LoadTransactions 50 -RunExecutorComparison
```

Hoặc chạy trực tiếp:

```powershell
go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
```

Benchmark này so sánh executor trên workload harness; nó **không** đo TPS mạng CometBFT bốn node. Dashboard đo giao dịch commit thực tế trên mạng Pluto. Khi báo cáo đồ án, nêu rõ hai phép đo này khác phạm vi.

## 7. Tắt và bật lại một validator

Trong khi dashboard đang chạy:

```powershell
.\scripts\stop-pluto-network.ps1 -Node validator2
.\scripts\start-pluto-network.ps1 -Node validator2
```

Script giữ nguyên `validator2/config`, khóa và DB nên node chạy lại với danh tính cũ và khôi phục dữ liệu. Quan sát dashboard/log cho tới khi node bắt kịp chiều cao và có cùng `AppHash` với các node còn lại.

Đây là thao tác dừng tiến trình thử nghiệm. Với node đang giữ dữ liệu có giá trị, cần dùng quy trình shutdown có kiểm soát trước khi dừng máy.

## 8. Chạy kiểm tra tự động

Kiểm tra các gói liên quan mạng thử nghiệm:

```powershell
go test ./internal/config ./internal/plutonetwork ./internal/app ./internal/ethrpc ./cmd/plutonetwork ./cmd/plutosmoke ./cmd/plutoload ./cmd/plutod -count=1
```

Kiểm tra toàn bộ repo:

```powershell
go test ./... -count=1
go test -race ./... -count=1
go build ./...
go vet ./...
```

Các test `internal/plutonetwork` xác nhận bốn validator có khóa/node ID riêng, cùng genesis/validator set/app state, khóa test không bị đưa vào metadata công khai, cấp cổng riêng (có thể tạo thêm mạng bằng port offset), và setup không ghi đè thư mục không rỗng. Test khởi tạo app từ cùng genesis trên bốn DB độc lập để so AppHash ban đầu; test so sánh AppHash bao gồm hash khớp, node lệch chiều cao và hash khác ở cùng chiều cao.

## 9. Giới hạn cần trình bày trung thực

- Bốn validator trên một máy kiểm tra được cấu hình và tính nhất quán, nhưng một sự cố máy hoặc một người quản trị vẫn có thể ảnh hưởng cả mạng.
- `AppHash` là hash trạng thái ứng dụng riêng của Pluto; không mô tả nó là Ethereum Merkle Patricia Trie root.
- Dashboard chỉ tự dừng node nếu nó đang mở và các PID vẫn khớp manifest do script quản lý.
- Các node phải chạy cùng phiên bản mã/config/genesis và cùng chế độ thực thi để phép so sánh có ý nghĩa.

## 10. Phí giao dịch PLU

Pluto dùng thị trường phí kiểu EIP-1559. Mức khởi đầu là base fee 1 gwei; RPC `eth_gasPrice` gợi ý base fee của block kế tiếp cộng tip mặc định 1 gwei, còn `eth_maxPriorityFeePerGas` trả về tip. Base fee của mỗi block tiếp theo được tính tất định từ base fee và lượng gas của block trước, với mục tiêu sử dụng một nửa giới hạn gas của block.

Ví có thể đặt `maxFeePerGas` cao hơn mức thực trả. Khi giao dịch vào block, số PLU bị trừ là `gasUsed × effectiveGasPrice`; phần gas không dùng được hoàn lại. Base fee không được chuyển cho validator và bị loại khỏi lưu thông; phần tip được ghi có vào địa chỉ proposer CometBFT của block. Đây là phí mạng thử nghiệm, không phải khoản thu nhập có giá trị bên ngoài mạng.

Hiện proposer vẫn giữ thứ tự giao dịch từ mempool, chưa xếp giao dịch theo tip; tip được trả cho proposer nhưng chưa tạo ưu tiên đưa vào block.

Các block đã tạo trước khi bật thị trường phí giữ nguyên dữ liệu và không bị tính phí hồi tố. Mọi validator phải chạy cùng binary đã cập nhật. Với mạng nhiều validator đang chạy, dừng toàn bộ node rồi build và khởi động lại cùng lúc; script giữ nguyên database:

```powershell
.\scripts\stop-pluto-network.ps1
.\scripts\start-pluto-network.ps1 -Rebuild
```

Sau khi mạng hội tụ, MetaMask sẽ lấy mức phí qua Ethereum RPC (validator 1: `http://127.0.0.1:18545`). Receipt có trường `effectiveGasPrice`; so sánh số dư PLU trước/sau giao dịch để xem phí đã trừ. Không khởi động đồng thời node cũ và mới vì chúng sẽ không đồng thuận về trạng thái phí.

CLI `plutotx` cũng lấy giá gas từ Ethereum RPC và phát giao dịch qua CometBFT RPC. Để dùng CLI với mạng bốn validator, trỏ cả hai endpoint vào validator 1:

```powershell
$env:PLUTO_ETH_RPC_URL = "http://127.0.0.1:18545"
$env:PLUTO_COMET_RPC_URL = "http://127.0.0.1:27757"
go run ./cmd/plutotx balance 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
go run ./cmd/plutotx send -key $env:PLUTO_TEST_KEY -to 0xDIA_CHI_NHAN -plu 1
```
