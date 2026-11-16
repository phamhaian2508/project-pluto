# Kết nối Remix với Pluto

Pluto cung cấp một điểm cuối Ethereum JSON-RPC cho môi trường phát triển cục bộ. Địa chỉ HTTP mặc định là `http://127.0.0.1:8545`. Giao dịch đã ký được gửi vào mempool CometBFT của node.

## 1. Khởi động node

Mở PowerShell tại thư mục gốc repository:

```powershell
go run ./cmd/plutod --home .\.pluto-home
```

Giữ cửa sổ này mở trong suốt quá trình dùng Remix. Mặc định node dùng executor tuần tự.

## 2. Thêm mạng vào MetaMask

Tạo mạng tùy chỉnh với các giá trị sau:

| Trường | Giá trị |
|---|---|
| Tên mạng | Pluto Local |
| URL RPC | `http://127.0.0.1:8545` |
| ID chuỗi | `31337` |
| Ký hiệu tiền tệ | `PLU` |

Kết nối một tài khoản thử nghiệm đã được cấp PLU trên đúng dữ liệu node này. Dữ liệu node đơn và mạng bốn validator là hai bộ dữ liệu riêng, dù có thể cùng EVM chain ID.

## 3. Triển khai và gọi Counter

1. Mở [Remix](https://remix.ethereum.org) và nạp `contracts/Counter.sol` từ dự án, hoặc dán mã nguồn vào một tệp mới.
2. Chọn compiler Solidity tương thích với pragma của hợp đồng rồi biên dịch.
3. Trong **Deploy & Run Transactions**, chọn **Browser Extension** (hoặc **Injected Provider - MetaMask**, tùy phiên bản Remix) và xác nhận MetaMask đang ở mạng Pluto Local.
4. Chọn artifact `Counter`, bấm **Deploy** và xác nhận giao dịch trong MetaMask.
5. Trong **Deployed Contracts**, mở hợp đồng vừa triển khai, gọi `increment()`, sau đó đọc `count`. Giá trị phải tăng thêm một nếu giao dịch thành công.

Muốn tương tác với hợp đồng đã triển khai, chọn đúng artifact ABI rồi dùng **At Address** với địa chỉ hợp đồng. Việc compile tạo ABI/bytecode trong Remix; nó không triển khai lại hợp đồng.

## 4. Giới hạn cần biết

JSON-RPC của Pluto chỉ hỗ trợ một tập phương thức. Không giả định mọi phương thức Ethereum hay truy vấn trạng thái lịch sử đều đã có. MetaMask là ví để ký và gửi giao dịch; Remix là giao diện biên dịch, triển khai và gọi hàm. Phí gas và số dư coin gốc được biểu diễn theo cấu hình Pluto hiện tại.

Chỉ dùng khóa thử nghiệm trên mạng phát triển. Không nhập khóa có tài sản thật vào node cục bộ.
