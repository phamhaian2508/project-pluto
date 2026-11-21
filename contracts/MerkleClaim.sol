// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

/// @title MerkleClaim
/// @notice hợp đồng benchmark Phase 3 so sánh đọc bitmap sớm (claimEarly) vs muộn (claimLate) trong 1 call frame
contract MerkleClaim {
    uint256 public constant TREE_DEPTH = 14;
    uint256 public constant LEAF_COUNT = 1 << TREE_DEPTH; // 16384 lá
    uint256 public constant WORD_COUNT = LEAF_COUNT >> 8; // 64 word x 256 bit

    // nhúng trực tiếp vào runtime bytecode (PUSH32) để không phát sinh SLOAD khi verify
    bytes32 public immutable MERKLE_ROOT;

    // 64 slot storage liên tiếp [0..63], mỗi slot chứa bitmap 256-bit
    uint256[64] private claimedBitMap;

    event Claimed(uint256 indexed index, address indexed account);

    error InvalidIndex(uint256 index);
    error InvalidProofLength(uint256 length);
    error AlreadyClaimed(uint256 index);
    error InvalidProof();

    constructor(bytes32 merkleRoot_) {
        MERKLE_ROOT = merkleRoot_;
    }

    function claimEarly(uint256 index, bytes32[] calldata proof) external {
        if (index >= LEAF_COUNT) revert InvalidIndex(index);
        if (proof.length != TREE_DEPTH) revert InvalidProofLength(proof.length);

        uint256 wordIndex = index >> 8;
        uint256 bitMask = uint256(1) << (index & 255);

        // đọc & check bitmap trước vòng băm proof -> xung đột lộ ngay đầu tx
        uint256 word = claimedBitMap[wordIndex];
        if ((word & bitMask) != 0) revert AlreadyClaimed(index);

        _verifyProof(index, msg.sender, proof);

        claimedBitMap[wordIndex] = word | bitMask;
        emit Claimed(index, msg.sender);
    }

    function claimLate(uint256 index, bytes32[] calldata proof) external {
        if (index >= LEAF_COUNT) revert InvalidIndex(index);
        if (proof.length != TREE_DEPTH) revert InvalidProofLength(proof.length);

        // verify proof thuần tính toán trước, đọc bitmap sau -> checkpoint trước SLOAD giữ được công băm
        _verifyProof(index, msg.sender, proof);

        uint256 wordIndex = index >> 8;
        uint256 bitMask = uint256(1) << (index & 255);

        uint256 word = claimedBitMap[wordIndex];
        if ((word & bitMask) != 0) revert AlreadyClaimed(index);

        claimedBitMap[wordIndex] = word | bitMask;
        emit Claimed(index, msg.sender);
    }

    function isClaimed(uint256 index) external view returns (bool) {
        if (index >= LEAF_COUNT) revert InvalidIndex(index);
        uint256 wordIndex = index >> 8;
        uint256 bitMask = uint256(1) << (index & 255);
        return (claimedBitMap[wordIndex] & bitMask) != 0;
    }

    function claimedWord(uint256 wordIndex) external view returns (uint256) {
        if (wordIndex >= WORD_COUNT) revert InvalidIndex(wordIndex);
        return claimedBitMap[wordIndex];
    }

    function _verifyProof(
        uint256 index,
        address owner,
        bytes32[] calldata proof
    ) internal view {
        bytes32 node = keccak256(abi.encode(index, owner));
        uint256 idx = index;
        unchecked {
            for (uint256 i = 0; i < TREE_DEPTH; ++i) {
                bytes32 sibling = proof[i];
                if ((idx & 1) == 0) {
                    node = keccak256(abi.encode(node, sibling));
                } else {
                    node = keccak256(abi.encode(sibling, node));
                }
                idx >>= 1;
            }
        }
        if (node != MERKLE_ROOT) revert InvalidProof();
    }
}
