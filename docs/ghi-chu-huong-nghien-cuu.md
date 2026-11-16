# Ghi chú hướng nghiên cứu

Các mục dưới đây là câu hỏi cần kiểm chứng và phát triển thành tài liệu riêng. Chúng không mô tả tính năng Pluto đã hoàn thành.

## Đồng thuận và lưu trữ

- So sánh CometBFT với các kiến trúc Ethereum, Bitcoin và Cosmos SDK: lớp đồng thuận, tạo block, thực thi giao dịch và mô hình lưu trữ khác nhau ở đâu?
- So sánh việc dựng lại trạng thái bằng cách phát lại toàn bộ block từ genesis với cách khôi phục từ snapshot/checkpoint rồi phát lại một số block gần nhất. Cần phân biệt cơ chế Pluto hiện có với hướng nghiên cứu.
- Đối chiếu mô hình lưu trữ Pluto/Pebble với client Ethereum, nêu rõ loại dữ liệu và cách lập chỉ mục; không chỉ so tên cơ sở dữ liệu.

## Thực thi song song

- Đọc tài liệu kỹ thuật và kết quả công bố về Sei v2, Monad và các hệ thống thực thi song song khác. Chỉ so sánh cùng loại workload, phần cứng và cách tính thông lượng.
- Ghi rõ Pluto hiện đo executor trong harness khác với thông lượng mạng nhiều validator.

## Triển khai và tương tác hợp đồng

- Làm rõ khác nhau giữa gửi giao dịch triển khai trực tiếp qua JSON-RPC/công cụ dòng lệnh và thao tác qua Remix/MetaMask. Cả hai đều gửi giao dịch đã ký lên cùng node; khác ở giao diện và công cụ.

## Cách dùng ghi chú

Trước khi đưa nội dung vào báo cáo, cần xác định câu hỏi, chọn nguồn chính thức hoặc bài nghiên cứu, ghi ngày truy cập và phân biệt số liệu nguồn với kết quả tự đo trên Pluto.
