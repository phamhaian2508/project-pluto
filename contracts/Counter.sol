// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Hợp đồng đếm đơn giản, dùng để thử deploy/gọi hàm và test xung đột (mọi tx cùng ghi 1 ô nhớ)
contract Counter {
    uint256 public count; // slot 0

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
