# Thử nghiệm M5.1 1C: bộ thông dịch tối giản

Spike này tách khỏi production Pluto và dùng cùng hợp đồng mẫu với spike 1A/1B. Nó chỉ cài đúng **7 opcode**: `PUSH0`, `PUSH1`, `ADD`, `SLOAD`, `SSTORE`, `MSTORE` và `RETURN`.

Checkpoint giữ PC, stack, memory, gas còn lại, các lần ghi storage và tập storage đã làm nóng. Test tạm dừng ngay sau `SSTORE`, khôi phục một máy mới rồi đối chiếu đầu ra, storage và gas còn lại chính xác với lần chạy liên tục của mini-interpreter và geth v1.16.8.

## Độ phức tạp và rủi ro bảo trì

- Phần cài đặt có 299 dòng (273 dòng không trắng), kèm 76 dòng test. Vòng lặp thực thi có đúng bảy nhánh opcode; checkpoint giữ sáu nhóm trạng thái: PC, stack, memory, gas, các lần ghi và storage đã làm nóng.
- Để tính gas đúng trong mẫu phải xử lý EIP-2929 (storage nóng/lạnh), chi phí `SSTORE` của EIP-2200 và chi phí mở rộng memory. Trạng thái cần overlay ghi riêng và tập slot nóng trong mỗi checkpoint.
- Spike không cài refund, lịch sử committed state, lựa chọn hard fork, jump, call, log, revert, create, precompile, đầu vào access list hay opcode ngoài danh sách bảy opcode.
- Mở rộng hướng này đồng nghĩa tự duy trì ngữ nghĩa EVM quan trọng cho đồng thuận khi geth và quy tắc hard fork thay đổi. Đây là phương án rủi ro cao nhất dù bản chứng minh khái niệm nhỏ.

> Spike không tích hợp vào executor production và không thuộc luồng xử lý giao dịch của node.
