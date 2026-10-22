# M5.1 spike 1A: fork vòng lặp interpreter của geth

Spike này tách khỏi production và chỉ trả lời câu hỏi khả thi: nếu sở hữu vòng
lặp interpreter thì Pluto có thể dừng tại ranh giới opcode, sao chép trạng thái
và chạy tiếp hay không? Test chứng minh có thể trên contract thẳng nhỏ: checkpoint
lưu `pc`, stack, memory, return data, gas, call depth và storage overlay; execution
khôi phục cho output/storage giống chạy liên tục và geth v1.16.8.

## Phạm vi code phải duy trì

- Spike nhỏ chỉ fork hình dạng fetch/charge/execute của `EVM.Run` và 7 opcode:
  `PUSH0`, `PUSH1`, `ADD`, `SLOAD`, `SSTORE`, `MSTORE`, `RETURN`.
- Geth v1.16.8 `core/vm` hiện có 31 file Go, khoảng 10.012 dòng. Tám file gắn
  trực tiếp với một fork resumable (`interpreter.go`, `contract.go`, `memory.go`,
  `stack.go`, `evm.go`, `instructions.go`, `gas_table.go`, `jump_table.go`) có khoảng
  3.980 dòng. Đây là bề mặt đồng bộ tối thiểu mỗi khi nâng geth.

## Phần geth cần sửa hoặc fork

- Biến cục bộ của `EVM.Run`: `pc`, stack, memory, gas/cost, result và call context.
- Trạng thái private của EVM: `returnData`, `depth`, `readOnly` và frame cha/con.
- Constructor/restore cho stack, memory, contract gas và jump analysis.
- Tín hiệu pause riêng, không bị hiểu thành revert, STOP hoặc out-of-gas.
- Liên kết checkpoint interpreter với snapshot của StateDB/overlay và read-version.
- Giữ tracer, dynamic gas và quy tắc hard-fork khớp với upstream.

API công khai của geth chỉ cho tracer đọc stack/memory; nó không cho gán lại
`pc`, stack, memory, `returnData` hoặc call-frame để resume. Vì vậy hướng này
thực sự yêu cầu duy trì fork, không thể chỉ bọc API hiện có.

## Chưa hỗ trợ trong spike

- Opcode khác, `JUMP/JUMPI`, dynamic gas và memory-expansion gas chính xác.
- `CALL/DELEGATECALL/STATICCALL`, `CREATE`, precompile và call frame lồng nhau.
- Revert, refund, log, access list, transient storage, self-destruct và blob/EOF.
- Tracer, interrupt, read-only mode, fork rules và nhiều checkpoint lồng nhau.

Do các giới hạn trên, spike chỉ chứng minh khả năng kỹ thuật trên mẫu nhỏ;
nó không phải interpreter thay thế và không được dùng bởi node.

Phần loop có cấu trúc dựa trên go-ethereum `core/vm/interpreter.go`, phát
hành theo GNU LGPL v3; spike giữ ghi nhận nguồn này để phục vụ đánh giá.
