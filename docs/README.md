# Danh mục tài liệu Pluto

Trang này là điểm vào cho tài liệu dự án. Mã nguồn và cấu hình đang chạy mới là căn cứ cuối cùng khi nội dung mô tả có thể đã cũ.

## Hướng dẫn sử dụng

| Tài liệu | Nội dung |
|---|---|
| [Sổ tay lệnh Pluto](huong-dan-lenh-pluto.md) | Chạy node đơn, mạng bốn validator, gửi coin, kiểm thử, so sánh executor, checkpoint và thao tác hợp đồng tiết kiệm trong Remix. |
| [Kết nối Remix](huong-dan-remix.md) | Kết nối MetaMask/Remix với JSON-RPC của node cục bộ và triển khai hợp đồng mẫu. |
| [Hướng dẫn harness](../test/harness/README.md) | Chạy test đối chiếu, workload, benchmark, fuzz và kiểm thử checkpoint. |

## Thiết kế và cấu trúc

| Tài liệu | Nội dung |
|---|---|
| [Báo cáo cấu trúc dự án](../design/bao-cao-cau-truc-du-an-pluto.md) | Thành phần, đường đi của giao dịch và vị trí mã nguồn chính. |
| [Thiết kế thực thi song song P/O/S](../design/thiet-ke-thuc-thi-song-song-m4.md) | Phân loại giao dịch, trạng thái riêng, xác thực và thứ tự chấp nhận kết quả. |

## Nghiên cứu và thử nghiệm

| Tài liệu | Trạng thái |
|---|---|
| [Ghi chú hướng nghiên cứu](ghi-chu-huong-nghien-cuu.md) | Các câu hỏi cần nghiên cứu thêm; chưa phải kết quả đã triển khai. |
| [Nghiên cứu mật mã hậu lượng tử](nghien-cuu-mat-ma-hau-luong-tu.md) | Tài liệu tham khảo lý thuyết và đề xuất; Pluto hiện chưa tích hợp PQC/QRNG. |
| [Quyết định spike M5.1](../test/spike/quyet-dinh-m5-1.md) | So sánh các hướng kỹ thuật; cần đọc như quyết định thiết kế, không phải bằng chứng rằng mọi phương án đã triển khai. |
| [Spike call-frame](../test/spike/callframe/README.md), [mini-interpreter](../test/spike/minivm/README.md), [fork interpreter geth](../test/spike/gethfork/README.md) | Các thử nghiệm cô lập, không được node production sử dụng. |

## Quy ước

- Tên package, hàm, cờ dòng lệnh, giao thức và mã lỗi giữ nguyên tiếng Anh để có thể đối chiếu trực tiếp với mã nguồn.
- Phần hướng dẫn và giải thích viết bằng tiếng Việt; thuật ngữ chuyên môn được giữ khi là tên chuẩn, kèm diễn giải khi cần.
- Lệnh PowerShell phải chạy từ thư mục gốc dự án, trừ khi tài liệu ghi rõ khác.
- Kết quả benchmark phụ thuộc máy, workload và cấu hình; không coi số liệu một lượt chạy là cam kết hiệu năng chung.
