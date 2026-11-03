// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "./utils/TwoStepOwnable.sol";

/// @notice Giao diện tối thiểu để vault nhận và trả một token ERC-20.
interface ISavingsAsset {
    function balanceOf(address account) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

/// @title PlutoSavingsVault
/// @notice Vault gửi token có kỳ hạn; tiền lãi phải được nạp vào vault trước.
/// @dev Chỉ hỗ trợ ERC-20 chuẩn, không hỗ trợ token thu phí hoặc token rebasing.
contract PlutoSavingsVault is TwoStepOwnable {
    uint256 public constant BASIS_POINTS = 10_000;
    uint256 public constant YEAR = 365 days;
    uint256 public constant MIN_TERM = 1 days;
    uint256 public constant MAX_TERM = 365 days;
    uint256 public constant MAX_APR_BPS = 10_000;

    ISavingsAsset public immutable asset;
    uint16 public annualRateBps;
    bool public depositsPaused;
    bool private entered;

    uint256 public nextPositionId = 1;
    uint256 public totalPrincipal;
    uint256 public totalReservedInterest;
    uint256 public rewardReserve;

    struct Position {
        address depositor;
        uint64 openedAt;
        uint64 unlockAt;
        uint256 principal;
        uint256 interest;
        bool closed;
    }

    mapping(uint256 positionId => Position position) public positions;

    error InvalidAsset();
    error ReentrantCall();
    error DepositsArePaused();
    error InvalidAmount();
    error InvalidTerm();
    error InvalidRate();
    error InsufficientRewardReserve();
    error PositionNotFound();
    error NotDepositor();
    error PositionAlreadyClosed();
    error PositionStillLocked();
    error UnsupportedTransferFee();
    error TokenCallFailed();
    error InsolventVault();
    error InsufficientFreeRewards();

    event Deposited(
        uint256 indexed positionId,
        address indexed depositor,
        uint256 principal,
        uint256 interest,
        uint64 unlockAt,
        uint16 annualRateBps
    );
    event Withdrawn(uint256 indexed positionId, address indexed depositor, uint256 principal, uint256 interest);
    event EarlyWithdrawn(uint256 indexed positionId, address indexed depositor, uint256 principal, uint256 forfeitedInterest);
    event RewardsFunded(address indexed funder, uint256 amount);
    event FreeRewardsWithdrawn(address indexed recipient, uint256 amount);
    event AnnualRateChanged(uint16 previousRateBps, uint16 newRateBps);
    event DepositsPausedStateChanged(bool paused);

    constructor(ISavingsAsset asset_, uint16 initialAnnualRateBps) {
        if (address(asset_) == address(0)) revert ZeroAddress();
        if (address(asset_).code.length == 0) revert InvalidAsset();
        if (initialAnnualRateBps > MAX_APR_BPS) revert InvalidRate();
        asset = asset_;
        annualRateBps = initialAnnualRateBps;
    }

    modifier nonReentrant() {
        if (entered) revert ReentrantCall();
        entered = true;
        _;
        entered = false;
    }

    /// @notice Xem lãi dự kiến theo lãi suất hiện tại; kết quả bị làm tròn xuống.
    function previewInterest(uint256 amount, uint256 termSeconds) public view returns (uint256) {
        if (termSeconds < MIN_TERM || termSeconds > MAX_TERM) revert InvalidTerm();
        return (amount * uint256(annualRateBps) * termSeconds) / (BASIS_POINTS * YEAR);
    }

    /// @notice Nạp token và mở một khoản gửi mới.
    /// @dev Người gửi cần approve vault trước; lãi suất của vị thế được cố định lúc gửi.
    function deposit(uint256 amount, uint64 termSeconds) external nonReentrant returns (uint256 positionId) {
        if (depositsPaused) revert DepositsArePaused();
        if (amount == 0) revert InvalidAmount();
        if (termSeconds < MIN_TERM || termSeconds > MAX_TERM) revert InvalidTerm();

        uint256 interest = (amount * uint256(annualRateBps) * uint256(termSeconds)) / (BASIS_POINTS * YEAR);
        if (totalReservedInterest + interest > rewardReserve) revert InsufficientRewardReserve();

        uint256 nowTimestamp = block.timestamp;
        uint256 unlockTimestamp = nowTimestamp + uint256(termSeconds);
        if (unlockTimestamp > type(uint64).max) revert InvalidTerm();

        positionId = nextPositionId++;
        positions[positionId] = Position({
            depositor: msg.sender,
            openedAt: uint64(nowTimestamp),
            unlockAt: uint64(unlockTimestamp),
            principal: amount,
            interest: interest,
            closed: false
        });
        totalPrincipal += amount;
        totalReservedInterest += interest;

        uint256 balanceBefore = asset.balanceOf(address(this));
        _safeTransferFrom(msg.sender, address(this), amount);
        if (asset.balanceOf(address(this)) - balanceBefore != amount) revert UnsupportedTransferFee();
        _requireSolvent();

        emit Deposited(positionId, msg.sender, amount, interest, uint64(unlockTimestamp), annualRateBps);
    }

    /// @notice Nạp trước token dùng để trả lãi cho các khoản gửi.
    function fundRewards(uint256 amount) external nonReentrant {
        if (amount == 0) revert InvalidAmount();
        uint256 balanceBefore = asset.balanceOf(address(this));
        _safeTransferFrom(msg.sender, address(this), amount);
        if (asset.balanceOf(address(this)) - balanceBefore != amount) revert UnsupportedTransferFee();
        rewardReserve += amount;
        _requireSolvent();
        emit RewardsFunded(msg.sender, amount);
    }

    /// @notice Rút gốc và lãi khi khoản gửi đã đến hạn.
    function withdraw(uint256 positionId) external nonReentrant {
        Position storage position = positions[positionId];
        _checkPosition(position);
        if (block.timestamp < position.unlockAt) revert PositionStillLocked();

        uint256 principal = position.principal;
        uint256 interest = position.interest;
        position.closed = true;
        totalPrincipal -= principal;
        totalReservedInterest -= interest;
        rewardReserve -= interest;
        _requireSolvent();

        _safeTransferExact(msg.sender, principal + interest);
        emit Withdrawn(positionId, msg.sender, principal, interest);
    }

    /// @notice Rút gốc trước hạn; người gửi từ bỏ toàn bộ lãi của vị thế đó.
    function withdrawEarly(uint256 positionId) external nonReentrant {
        Position storage position = positions[positionId];
        _checkPosition(position);

        uint256 principal = position.principal;
        uint256 forfeitedInterest = position.interest;
        position.closed = true;
        totalPrincipal -= principal;
        totalReservedInterest -= forfeitedInterest;
        _requireSolvent();

        _safeTransferExact(msg.sender, principal);
        emit EarlyWithdrawn(positionId, msg.sender, principal, forfeitedInterest);
    }

    /// @notice Đổi lãi suất áp dụng cho khoản gửi mới; vị thế cũ không bị đổi lãi.
    function setAnnualRateBps(uint16 newRateBps) external onlyOwner {
        if (newRateBps > MAX_APR_BPS) revert InvalidRate();
        uint16 previousRateBps = annualRateBps;
        annualRateBps = newRateBps;
        emit AnnualRateChanged(previousRateBps, newRateBps);
    }

    /// @notice Tạm dừng/mở lại việc tạo khoản gửi mới; người dùng vẫn rút được.
    function setDepositsPaused(bool paused_) external onlyOwner {
        depositsPaused = paused_;
        emit DepositsPausedStateChanged(paused_);
    }

    /// @notice Số lãi chưa được hứa trả cho vị thế nào.
    function freeRewardReserve() public view returns (uint256) {
        return rewardReserve - totalReservedInterest;
    }

    /// @notice Chủ vault chỉ rút phần quỹ lãi còn dư sau khi trừ các nghĩa vụ đã hứa.
    function withdrawFreeRewards(address recipient, uint256 amount) external onlyOwner nonReentrant {
        if (recipient == address(0)) revert ZeroAddress();
        if (amount > freeRewardReserve()) revert InsufficientFreeRewards();

        rewardReserve -= amount;
        _requireSolvent();
        _safeTransferExact(recipient, amount);
        emit FreeRewardsWithdrawn(recipient, amount);
    }

    function _checkPosition(Position storage position) private view {
        if (position.depositor == address(0)) revert PositionNotFound();
        if (position.depositor != msg.sender) revert NotDepositor();
        if (position.closed) revert PositionAlreadyClosed();
    }

    function _requireSolvent() private view {
        if (asset.balanceOf(address(this)) < totalPrincipal + rewardReserve) revert InsolventVault();
    }

    function _safeTransfer(address to, uint256 amount) private {
        (bool success, bytes memory returned) = address(asset).call(
            abi.encodeWithSelector(ISavingsAsset.transfer.selector, to, amount)
        );
        if (!success || !_returnedTrueOrEmpty(returned)) revert TokenCallFailed();
    }

    function _safeTransferExact(address to, uint256 amount) private {
        if (to == address(0) || to == address(this)) revert ZeroAddress();
        uint256 vaultBalanceBefore = asset.balanceOf(address(this));
        uint256 recipientBalanceBefore = asset.balanceOf(to);
        _safeTransfer(to, amount);
        uint256 vaultBalanceAfter = asset.balanceOf(address(this));
        uint256 recipientBalanceAfter = asset.balanceOf(to);
        if (
            vaultBalanceBefore < vaultBalanceAfter ||
            vaultBalanceBefore - vaultBalanceAfter != amount ||
            recipientBalanceAfter < recipientBalanceBefore ||
            recipientBalanceAfter - recipientBalanceBefore != amount
        ) revert UnsupportedTransferFee();
    }

    function _safeTransferFrom(address from, address to, uint256 amount) private {
        (bool success, bytes memory returned) = address(asset).call(
            abi.encodeWithSelector(ISavingsAsset.transferFrom.selector, from, to, amount)
        );
        if (!success || !_returnedTrueOrEmpty(returned)) revert TokenCallFailed();
    }

    function _returnedTrueOrEmpty(bytes memory returned) private pure returns (bool) {
        if (returned.length == 0) return true;
        if (returned.length < 32) return false;
        return abi.decode(returned, (bool));
    }
}
