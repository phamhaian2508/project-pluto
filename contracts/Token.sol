// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// ERC-20 tối giản (chỉ có transfer/balanceOf), không cần tham số khi deploy
contract Token {
    mapping(address => uint256) public balanceOf; // slot 0
    uint256 public totalSupply;                   // slot 1

    event Transfer(address indexed from, address indexed to, uint256 value);

    constructor() {
        totalSupply = 1_000_000 ether;
        balanceOf[msg.sender] = totalSupply;
        emit Transfer(address(0), msg.sender, totalSupply);
    }

    function transfer(address to, uint256 v) external returns (bool) {
        require(balanceOf[msg.sender] >= v, "balance");
        balanceOf[msg.sender] -= v;
        balanceOf[to] += v;
        emit Transfer(msg.sender, to, v);
        return true;
    }
}
