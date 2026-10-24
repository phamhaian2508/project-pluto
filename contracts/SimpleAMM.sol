// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Bể thanh khoản tích số không đổi cỡ nhỏ, dùng trong bộ kiểm thử đối chiếu.
// Mã khởi tạo và mã chạy EVM tương ứng được tạo trong test/harness để bài kiểm
// thử không cần cài solc.
contract SimpleAMM {
    uint256 public reserve0 = 1_000_000;
    uint256 public reserve1 = 1_000_000;

    function swap(uint256 amountIn) external returns (uint256 amountOut) {
        amountOut = (amountIn * reserve1) / (reserve0 + amountIn);
        reserve0 += amountIn;
        reserve1 -= amountOut;
    }
}
