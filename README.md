# Pluto

Pluto là dự án blockchain viết bằng Go, kết hợp CometBFT để tạo block và đồng thuận, EVM của go-ethereum để chạy hợp đồng thông minh, Pebble để lưu dữ liệu, cùng một cổng Ethereum JSON-RPC có phạm vi phương thức được hỗ trợ.

## Bắt đầu

Yêu cầu: Go phiên bản được khai báo trong `go.mod`. Các lệnh dưới đây chạy trong PowerShell từ thư mục gốc dự án.

```powershell
go test ./... -count=1
go run ./cmd/plutod --home .\.pluto-home
```

Node mặc định chạy executor tuần tự và mở JSON-RPC Ethereum tại `http://127.0.0.1:8545`. Giữ cửa sổ terminal này mở. Dữ liệu node được lưu trong `.pluto-home`; không chạy hai tiến trình trên cùng thư mục dữ liệu.

Để chọn executor song song, dừng node bằng `Ctrl+C` rồi chạy:

```powershell
go run ./cmd/plutod --home .\.pluto-home --executor=parallel --workers=auto
```

Chỉ dùng khóa tài khoản thử nghiệm trên mạng cục bộ. Không đưa private key, tệp cấu hình node hoặc dữ liệu trong `.pluto-home` và `.pluto-network` lên Git.

## Tài liệu

- [Danh mục tài liệu](docs/README.md): hướng dẫn theo chủ đề và tình trạng tài liệu.
- [Sổ tay lệnh](docs/huong-dan-lenh-pluto.md): chạy một node, mạng bốn validator, kiểm thử, so sánh executor và thao tác hợp đồng tiết kiệm.
- [Kết nối Remix](docs/huong-dan-remix.md): kết nối MetaMask/Remix với node Pluto cục bộ.
- [Hướng dẫn harness](test/harness/README.md): kiểm tra tính đúng đắn, checkpoint, fuzz và đo hiệu năng.
- [Báo cáo cấu trúc](design/bao-cao-cau-truc-du-an-pluto.md): lần theo luồng xử lý và các gói chính.
- [Thiết kế thực thi song song](design/thiet-ke-thuc-thi-song-song-m4.md): mô hình P/O/S và các nguyên tắc xác thực kết quả.
- [Hướng dẫn mạng bốn validator](docs/huong-dan-lenh-pluto.md#3-mạng-bốn-validator): tạo, chạy, theo dõi và dừng mạng cục bộ.

## Phạm vi và cách đọc kết quả

Pluto là dự án đang phát triển, không phải node Ethereum tương thích đầy đủ hay mạng sản xuất. Các bài kiểm thử chỉ chứng minh những trường hợp được chạy; benchmark executor trong `test/harness` không đo TPS của mạng CometBFT. Đọc [sổ tay kiểm thử](test/harness/README.md) để phân biệt kiểm tra tính đúng đắn với đo hiệu năng.

Tài liệu nghiên cứu mật mã hậu lượng tử là đề xuất tham khảo, không phải tính năng đã tích hợp vào node. Tài liệu spike dưới `test/spike/` là thử nghiệm thiết kế và không thuộc đường thực thi production.
