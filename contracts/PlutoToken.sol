// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "./utils/TwoStepOwnable.sol";

/// @title PlutoToken
/// @notice Token ERC-20 thử nghiệm nguồn cung cố định dùng trên EVM của Pluto.
/// @dev Đây là token hợp đồng, không phải đồng gốc dùng trả phí mạng.
contract PlutoToken is TwoStepOwnable {
    string public constant name = "Pluto Demo Token";
    string public constant symbol = "PTKN";
    uint8 public constant decimals = 18;

    uint256 public constant MAX_SUPPLY = 1_000_000 * 10 ** 18;

    uint256 public totalSupply;
    bool public paused;

    mapping(address account => uint256 amount) public balanceOf;
    mapping(address account => mapping(address spender => uint256 amount)) public allowance;

    error Paused();
    error NotPaused();
    error InsufficientBalance();
    error InsufficientAllowance();

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    event PausedStateChanged(bool paused);

    constructor() {
        totalSupply = MAX_SUPPLY;
        balanceOf[msg.sender] = MAX_SUPPLY;
        emit Transfer(address(0), msg.sender, MAX_SUPPLY);
    }

    /// @notice Gửi token từ ví gọi hàm đến địa chỉ nhận.
    function transfer(address to, uint256 amount) external returns (bool) {
        _transfer(msg.sender, to, amount);
        return true;
    }

    /// @notice Cấp quyền cho spender dùng tối đa amount token.
    function approve(address spender, uint256 amount) external returns (bool) {
        if (spender == address(0)) revert ZeroAddress();
        allowance[msg.sender][spender] = amount;
        emit Approval(msg.sender, spender, amount);
        return true;
    }

    /// @notice Chuyển token theo quyền đã được cấp bằng approve.
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        _spendAllowance(from, msg.sender, amount);
        _transfer(from, to, amount);
        return true;
    }

    /// @notice Hủy token của chính người gọi và giảm tổng cung.
    function burn(uint256 amount) external {
        _burn(msg.sender, amount);
    }

    /// @notice Hủy token của tài khoản khác trong phạm vi allowance.
    function burnFrom(address account, uint256 amount) external {
        _spendAllowance(account, msg.sender, amount);
        _burn(account, amount);
    }

    /// @notice Tạm dừng chuyển, cấp quyền dùng và hủy token khi có sự cố.
    function pause() external onlyOwner {
        if (paused) revert Paused();
        paused = true;
        emit PausedStateChanged(true);
    }

    /// @notice Mở lại các thao tác token sau khi sự cố được xử lý.
    function unpause() external onlyOwner {
        if (!paused) revert NotPaused();
        paused = false;
        emit PausedStateChanged(false);
    }

    function _transfer(address from, address to, uint256 amount) private {
        _requireNotPaused();
        if (from == address(0) || to == address(0)) revert ZeroAddress();

        uint256 balance = balanceOf[from];
        if (balance < amount) revert InsufficientBalance();
        unchecked {
            balanceOf[from] = balance - amount;
        }
        balanceOf[to] += amount;
        emit Transfer(from, to, amount);
    }

    function _burn(address account, uint256 amount) private {
        _requireNotPaused();
        if (account == address(0)) revert ZeroAddress();

        uint256 balance = balanceOf[account];
        if (balance < amount) revert InsufficientBalance();
        unchecked {
            balanceOf[account] = balance - amount;
            totalSupply -= amount;
        }
        emit Transfer(account, address(0), amount);
    }

    function _spendAllowance(address account, address spender, uint256 amount) private {
        uint256 approved = allowance[account][spender];
        if (approved == type(uint256).max) return;
        if (approved < amount) revert InsufficientAllowance();

        unchecked {
            allowance[account][spender] = approved - amount;
        }
        emit Approval(account, spender, allowance[account][spender]);
    }

    function _requireNotPaused() private view {
        if (paused) revert Paused();
    }
}
