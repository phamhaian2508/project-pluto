# Demo M5 checkpoint

Đứng tại thư mục gốc của dự án và chạy:

```powershell
go run ./cmd/checkpointdemo
```

Lệnh tự tạo 64 giao dịch gọi cùng một contract `Counter`. Các giao dịch chạy
song song và cùng cập nhật một ô storage nên phát sinh xung đột. Demo chạy cùng
workload bằng P0, P1, P2 và P3 rồi in bảng so sánh.

Phần `TRACE THUC TE` được lấy trực tiếp tại resolver của production
`ParallelExecutor`. Mỗi dòng cho biết transaction bị validation thất bại, các
vị trí checkpoint thật đã tạo, checkpoint thật được khôi phục và số opcode chạy
tiếp. P0 sẽ hiện `CHAY LAI TU DAU`, còn P1 sẽ hiện `khoi phuc checkpoint`.

- `P0`: không checkpoint, giao dịch xung đột phải chạy lại từ đầu.
- `P1/P2/P3`: có checkpoint và có thể resume.
- `CHAY LAI OPCODE`: càng nhỏ thì lượng công việc phải chạy lại càng ít.
- `Counter=64` ở mọi policy: tối ưu không làm thay đổi kết quả cuối.

Demo gọi trực tiếp `ParallelExecutor` và EVM production mà node Pluto sử dụng.
Nó không cần mở cổng mạng hoặc chạy `plutod`, vì mục tiêu là cô lập và thể hiện
rõ cơ chế thực thi M5.
