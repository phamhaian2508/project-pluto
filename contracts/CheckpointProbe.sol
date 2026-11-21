// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

/// @title HelperTarget
/// @notice hợp đồng phụ nhận external CALL cho ca checkpoint-errors (suffix CALL và prefix CALL)
contract HelperTarget {
    uint256 public helperCount;

    event HelperPinged(address indexed caller, uint256 newHelperCount, bytes32 tag);

    function ping(bytes32 tag) external returns (uint256 newVal) {
        unchecked {
            newVal = helperCount + 1;
        }
        helperCount = newVal;
        emit HelperPinged(msg.sender, newVal, tag);
    }
}

/// @title CheckpointProbe
/// @notice hợp đồng kiểm thử & benchmark Phase 2 cho checkpointing/resume trong OCC
contract CheckpointProbe {
    // slot 0 dùng chung cho các ca tranh chấp early/late/revert/external-call
    uint256 public count;

    event CountUpdated(uint256 indexed newCount, bytes32 digest);
    event SlotUpdated(uint256 indexed slotKey, uint256 value, bytes32 digest);
    event WarmColdChecked(
        uint256 indexed slotKey,
        uint256 read1,
        uint256 read2,
        uint256 read3,
        uint256 finalVal,
        bytes32 digest
    );
    event ExternalCallDone(
        address indexed target,
        uint256 helperVal,
        uint256 newCount,
        bytes32 digest
    );

    error ForcedRevert(uint256 slotKey, uint256 observedCount, bytes32 digest);

    /// @notice ca checkpoint-early: SLOAD(count) trước -> băm rounds vòng -> SSTORE(count+1) -> emit
    function readThenHashIncrement(
        uint256 rounds,
        bytes32 seed
    ) external returns (uint256 newCount, bytes32 digest) {
        uint256 current = count;
        digest = _hashRounds(rounds, seed);
        unchecked {
            newCount = current + 1;
        }
        count = newCount;
        emit CountUpdated(newCount, digest);
    }

    /// @notice ca checkpoint-late: băm rounds vòng trước -> SLOAD(count) -> SSTORE(count+1) -> emit
    function hashThenIncrement(
        uint256 rounds,
        bytes32 seed
    ) external returns (uint256 newCount, bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        uint256 current = count;
        unchecked {
            newCount = current + 1;
        }
        count = newCount;
        emit CountUpdated(newCount, digest);
    }

    /// @notice ca checkpoint-independent: băm rounds vòng trước -> đọc & tăng slot riêng theo slotKey
    function hashThenIncrementOwnSlot(
        uint256 slotKey,
        uint256 rounds,
        bytes32 seed
    ) external returns (uint256 newVal, bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        assembly {
            let cur := sload(slotKey)
            newVal := add(cur, 1)
            sstore(slotKey, newVal)
        }
        emit SlotUpdated(slotKey, newVal, digest);
    }

    /// @notice ca checkpoint-sstore-gas: băm thuần rồi SSTORE trực tiếp, tuyệt đối 0 SLOAD trước SSTORE
    function hashThenDirectSstore(
        uint256 slotKey,
        uint256 rounds,
        bytes32 seed,
        uint256 value
    ) external returns (bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        assembly {
            sstore(slotKey, value)
        }
        emit SlotUpdated(slotKey, value, digest);
    }

    /// @notice ca checkpoint-errors (revert): băm thuần -> đọc count (tạo xung đột với A) -> ghi slotKey + emit -> revert
    function hashThenWriteLogRevert(
        uint256 slotKey,
        uint256 rounds,
        bytes32 seed,
        uint256 value
    ) external {
        bytes32 digest = _hashRounds(rounds, seed);
        uint256 observed = count;
        assembly {
            sstore(slotKey, value)
        }
        emit SlotUpdated(slotKey, value, digest);
        revert ForcedRevert(slotKey, observed, digest);
    }

    /// @notice ca checkpoint-errors (memory expansion): mở rộng memory cả trước và sau SLOAD/SSTORE để kiểm memory.lastGasCost
    function hashThenExpandMemoryAndStore(
        uint256 slotKey,
        uint256 rounds,
        bytes32 seed,
        uint256 wordsBefore,
        uint256 wordsAfter
    ) external returns (uint256 newVal, bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        uint256 beforeChecksum;
        assembly {
            if gt(wordsBefore, 0) {
                let ptr := mload(0x40)
                let endOff := add(ptr, shl(5, sub(wordsBefore, 1)))
                mstore(endOff, xor(digest, wordsBefore))
                beforeChecksum := mload(endOff)
                // đẩy free memory pointer qua vùng vừa cấp phát để emit/encode không ghi đè
                mstore(0x40, add(endOff, 0x20))
            }
        }
        uint256 cur;
        assembly {
            cur := sload(slotKey)
        }
        uint256 afterChecksum;
        assembly {
            if gt(wordsAfter, 0) {
                let ptr := mload(0x40)
                let endOff := add(ptr, shl(5, sub(wordsAfter, 1)))
                mstore(endOff, xor(add(cur, beforeChecksum), wordsAfter))
                afterChecksum := mload(endOff)
                mstore(0x40, add(endOff, 0x20))
            }
            newVal := add(cur, 1)
            sstore(slotKey, newVal)
        }
        assembly {
            mstore(0x00, digest)
            mstore(0x20, xor(beforeChecksum, afterChecksum))
            digest := keccak256(0x00, 0x40)
        }
        emit SlotUpdated(slotKey, newVal, digest);
    }

    /// @notice ca checkpoint-errors (warm/cold & refund EIP-2200/3529): đọc/ghi nhiều lần cùng slot, hỗ trợ đặt lại giá trị gốc
    function hashThenWarmColdRefund(
        uint256 slotKey,
        uint256 rounds,
        bytes32 seed,
        uint256 v1,
        uint256 v2
    ) external returns (uint256 finalVal, bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        uint256 read1;
        uint256 read2;
        uint256 read3;
        assembly {
            read1 := sload(slotKey)
            read2 := sload(slotKey)
            sstore(slotKey, v1)
            read3 := sload(slotKey)
            sstore(slotKey, v2)
            finalVal := sload(slotKey)
        }
        emit WarmColdChecked(slotKey, read1, read2, read3, finalVal, digest);
    }

    /// @notice ca checkpoint-errors (suffix CALL): băm thuần -> chạm count (kích hoạt resume) -> CALL sang HelperTarget
    function hashThenExternalCall(
        address target,
        uint256 rounds,
        bytes32 seed
    ) external returns (uint256 helperVal, uint256 newCount, bytes32 digest) {
        digest = _hashRounds(rounds, seed);
        uint256 current = count;
        unchecked {
            newCount = current + 1;
        }
        count = newCount;
        helperVal = HelperTarget(target).ping(digest);
        emit ExternalCallDone(target, helperVal, newCount, digest);
    }

    /// @notice ca checkpoint-errors (prefix CALL): CALL sang HelperTarget trước -> băm ở frame gốc -> đọc & tăng count
    function externalCallThenRead(
        address target,
        uint256 rounds,
        bytes32 seed
    ) external returns (uint256 helperVal, uint256 newCount, bytes32 digest) {
        helperVal = HelperTarget(target).ping(seed);
        digest = _hashRounds(rounds, bytes32(uint256(seed) ^ helperVal));
        uint256 current = count;
        unchecked {
            newCount = current + 1;
        }
        count = newCount;
        emit ExternalCallDone(target, helperVal, newCount, digest);
    }

    function readSlot(uint256 slotKey) external view returns (uint256 val) {
        assembly {
            val := sload(slotKey)
        }
    }

    function _hashRounds(uint256 rounds, bytes32 seed) internal pure returns (bytes32 digest) {
        digest = seed;
        assembly {
            // dùng scratch space [0x00..0x3f] để giữ memory ổn định cho các ca đo chuẩn
            for { let i := 0 } lt(i, rounds) { i := add(i, 1) } {
                mstore(0x00, digest)
                mstore(0x20, i)
                digest := keccak256(0x00, 0x40)
            }
        }
    }
}
