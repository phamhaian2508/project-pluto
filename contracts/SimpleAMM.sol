// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Small constant-product pool used by the differential harness. The matching
// EVM creation/runtime fixture is assembled in test/harness so tests do not
// require solc to be installed.
contract SimpleAMM {
    uint256 public reserve0 = 1_000_000;
    uint256 public reserve1 = 1_000_000;

    function swap(uint256 amountIn) external returns (uint256 amountOut) {
        amountOut = (amountIn * reserve1) / (reserve0 + amountIn);
        reserve0 += amountIn;
        reserve1 -= amountOut;
    }
}
