// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Hợp đồng đếm đơn giản, dùng để thử triển khai/gọi hàm và kiểm thử xung đột
// (mọi giao dịch cùng ghi vào một ô lưu trữ).
contract Counter {
    uint256 public count; // Ô lưu trữ 0.

    event Incremented(uint256 newCount);

    function increment() external {
        count += 1;
        emit Incremented(count);
    }

    function add(uint256 x) external {
        count += x;
        emit Incremented(count);
    }

    function fail() external pure {
        revert("nope");
    }
}
