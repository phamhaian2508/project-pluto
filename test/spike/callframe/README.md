# Thử nghiệm M5.1 1B: checkpoint tại ranh giới khung gọi

Spike này dùng geth nguyên bản và được cô lập khỏi executor production của Pluto. Nó dùng lại `gethfork.SampleContract` từ spike 1A.

## Kết quả

- **Hoạt động tại ranh giới frame hoàn chỉnh bên ngoài:** chụp `StateDB` trước top-level call, hoàn tác về snapshot rồi phát lại toàn bộ frame cha. Test xác nhận đầu ra, gas, trace lời gọi lồng nhau và storage giống nhau.
- **Có thể quan sát:** hook tracing công khai báo lúc vào/ra `CALL` lồng nhau, `returnData`, gas sử dụng, revert và hết gas. Log nằm trong `StateDB` và được hoàn tác cùng snapshot.
- **Không thể tiếp tục tại ranh giới `CALL` lồng nhau:** `OnExit` không cung cấp PC, stack, memory, gas còn lại hay vùng `returnData` của frame cha đang treo. API công khai geth cũng không cho khôi phục các trường này. Vì vậy, khi hàm con trả về, không thể tiếp tục frame cha ở opcode kế tiếp; phải phát lại toàn bộ frame cha.

> Cách này không sửa interpreter geth và có phạm vi bảo trì nhỏ, nhưng checkpoint thô hơn spike 1A. Nó không triển khai tích hợp production hay phần M5.2.
