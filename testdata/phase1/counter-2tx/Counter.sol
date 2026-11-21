// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

contract Counter {
    uint256 public count;
    event Incremented(uint256 value);

    function increment() external {
        count += 1;
        emit Incremented(count);
    }
}
