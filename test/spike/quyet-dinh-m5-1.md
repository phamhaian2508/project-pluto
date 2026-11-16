# Quyết định kỹ thuật M5.1: checkpoint có thể tiếp tục EVM

**Trạng thái quyết định:** chọn hướng 1A làm cơ sở cho giai đoạn triển khai M5.
**Phạm vi tài liệu:** quyết định thiết kế dựa trên các spike cô lập; không tự khẳng định rằng thay đổi production đã hoàn tất.

## Bối cảnh

Mục tiêu M5 là tiếp tục giao dịch từ checkpoint nằm ngay trước lần đọc trạng thái đầu cơ bị phát hiện là đã cũ. Phát lại toàn bộ giao dịch hoặc call frame không đáp ứng mục tiêu tiếp tục giữa các opcode. Ba spike dùng cùng hợp đồng mẫu và chưa được nối với executor production của Pluto.

## So sánh các hướng

| Tiêu chí | 1A: fork interpreter geth | 1B: ranh giới call frame | 1C: interpreter tối giản |
|---|---|---|---|
| Điểm tiếp tục | Đã chứng minh giữa các opcode với mẫu chạy thẳng; có thể giữ toàn bộ trạng thái máy geth. | API công khai không khôi phục được trạng thái cha đang treo; phải hoàn tác rồi phát lại cả frame. | Đã chứng minh giữa các opcode trong mẫu chỉ có 7 opcode. |
| Độ đúng EVM | Spike khớp đầu ra và storage; gas còn đơn giản hóa. Bản production phải giữ nguyên bảng gas và logic opcode của geth. | Dùng geth nguyên bản; spike tái tạo đầu ra, storage, gas và trace bằng hoàn tác/phát lại frame. | Khớp đầu ra, storage và gas còn lại trong mẫu; không suy rộng sang opcode chưa hỗ trợ. |
| Lời gọi lồng nhau, revert, gas, log | Fork đầy đủ có thể giữ ngữ nghĩa, nhưng spike 1A chưa chứng minh đủ các trường hợp này. Cần chụp toàn bộ call stack và vị trí journal StateDB. | Quan sát và tái tạo được nested `CALL`, revert, hết gas, log và return data bằng phát lại frame; không thể tiếp tục chính xác giữa frame. | Không hỗ trợ `CALL`, revert, log hay call frame; gas chỉ được cài cho mẫu. |
| Phạm vi bảo trì | Spike có 265 dòng triển khai và 81 dòng test. Fork có thể chạm khoảng 8 tệp `core/vm` liên quan. | Spike có 61 dòng triển khai và 168 dòng test; không fork geth. | Spike có 299 dòng triển khai và 76 dòng test cho 7 opcode. Mở rộng thành EVM thứ hai có rủi ro cao. |
| Chi phí | Sao chép checkpoint tỷ lệ với stack, memory, return data và metadata overlay; có thể bỏ qua tiền tố đã chạy khi resume thành công. | Payload checkpoint nhỏ hơn nhưng vẫn phải thực thi lại toàn frame. | Chi phí mẫu nhỏ, nhưng không thể dùng làm ước tính sản phẩm nếu chưa cài toàn EVM. |
| Khả năng mở rộng | Có khả năng hỗ trợ EVM rộng, đổi lại phải đồng bộ và kiểm thử chặt mỗi lần nâng geth. | Dễ tương thích với geth nhưng không đạt tiếp tục giữa opcode. | Kém phù hợp: phải tự duy trì mọi opcode và quy tắc hard fork. |

## Quyết định và lý do

Chọn **1A** vì đây là hướng duy nhất trong ba lựa chọn có thể đáp ứng mục tiêu cốt lõi: tiếp tục từ checkpoint opcode trước lần đọc trạng thái xung đột mà không phát lại giao dịch hoặc frame cha.

Hướng 1B tương thích tốt với geth và có phạm vi bảo trì nhỏ, nhưng spike cho thấy API công khai không cung cấp PC, stack, memory, gas còn lại hoặc return data của frame cha để khôi phục. Hướng 1C chứng minh ý tưởng trên bảy opcode, nhưng phát triển nó thành EVM riêng sẽ lặp lại ngữ nghĩa đồng thuận và tăng rủi ro sai lệch.

## Điều kiện trước khi coi là hoàn tất

Fork 1A phải giữ nguyên ngữ nghĩa opcode và gas của geth, chỉ bổ sung cơ chế tạm dừng/chụp/khôi phục. Cần kiểm thử đối chiếu và fuzz với phiên bản geth được ghim, bao gồm nested call, revert, hết gas, log, return data, refund, access list và quy tắc hard fork. Tài liệu này ghi quyết định, không phải kết quả các điều kiện nghiệm thu hay bằng chứng tích hợp production.
