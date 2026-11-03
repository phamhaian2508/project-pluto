// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @notice Quyền quản trị với quy trình chuyển quyền hai bước.
/// @dev Chủ sở hữu mới phải tự gọi acceptOwnership để hoàn tất việc nhận quyền.
abstract contract TwoStepOwnable {
    address public owner;
    address public pendingOwner;

    error NotOwner();
    error NotPendingOwner();
    error ZeroAddress();

    event OwnershipTransferStarted(address indexed owner, address indexed pendingOwner);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);
    event OwnershipTransferCancelled(address indexed owner, address indexed cancelledPendingOwner);

    constructor() {
        owner = msg.sender;
        emit OwnershipTransferred(address(0), msg.sender);
    }

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    /// @notice Đề nghị chuyển quyền; người nhận phải chấp thuận ở giao dịch riêng.
    function transferOwnership(address newOwner) external onlyOwner {
        if (newOwner == address(0)) revert ZeroAddress();
        pendingOwner = newOwner;
        emit OwnershipTransferStarted(owner, newOwner);
    }

    /// @notice Người được đề nghị gọi hàm này để nhận quyền quản trị.
    function acceptOwnership() external {
        if (msg.sender != pendingOwner) revert NotPendingOwner();

        address previousOwner = owner;
        owner = msg.sender;
        pendingOwner = address(0);
        emit OwnershipTransferred(previousOwner, msg.sender);
    }

    /// @notice Chủ hiện tại hủy đề nghị chuyển quyền đang chờ.
    function cancelOwnershipTransfer() external onlyOwner {
        address cancelledOwner = pendingOwner;
        pendingOwner = address(0);
        emit OwnershipTransferCancelled(owner, cancelledOwner);
    }
}
