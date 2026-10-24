package evm

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// NewOverlay tạo StateDB riêng cho giao dịch, đọc xuyên qua ảnh chụp cha bất
// biến và chỉ lưu các tài khoản/ô lưu trữ mà chính giao dịch đó truy cập.
func (s *PebbleStateDB) NewOverlay() *PebbleStateDB {
	overlay := NewPebbleStateDB(s.db)
	overlay.overlayBase = s
	return overlay
}

// Fork tạo một bản chụp cô lập tại ranh giới giao dịch. Các lần đọc chưa lưu
// đệm vẫn dùng trạng thái đã chốt bất biến của Pebble; mọi map và nhật ký mà EVM
// có thể thay đổi đều thuộc riêng về bản sao này.
func (s *PebbleStateDB) Fork() *PebbleStateDB {
	fork := NewPebbleStateDB(s.db)
	fork.overlayBase = s.overlayBase
	for addr, account := range s.state {
		clone := *account
		if account.Balance != nil {
			clone.Balance = new(big.Int).Set(account.Balance)
		}
		clone.CodeHash = append([]byte(nil), account.CodeHash...)
		fork.state[addr] = &clone
	}
	for addr, code := range s.code {
		fork.code[addr] = append([]byte(nil), code...)
	}
	for addr, dirty := range s.dirtyCode {
		fork.dirtyCode[addr] = dirty
	}
	for addr, slots := range s.storage {
		fork.storage[addr] = cloneHashMap(slots)
	}
	for addr, slots := range s.dirtyStorage {
		fork.dirtyStorage[addr] = make(map[common.Hash]bool, len(slots))
		for key, dirty := range slots {
			fork.dirtyStorage[addr][key] = dirty
		}
	}
	for addr, dirty := range s.dirtyAccounts {
		fork.dirtyAccounts[addr] = dirty
	}
	fork.logs = cloneLogs(s.logs)
	return fork
}

func cloneHashMap(input map[common.Hash]common.Hash) map[common.Hash]common.Hash {
	output := make(map[common.Hash]common.Hash, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneLogs(input []*types.Log) []*types.Log {
	output := make([]*types.Log, len(input))
	for i, entry := range input {
		if entry == nil {
			continue
		}
		clone := *entry
		clone.Topics = append([]common.Hash(nil), entry.Topics...)
		clone.Data = append([]byte(nil), entry.Data...)
		output[i] = &clone
	}
	return output
}

// ApplyPendingWrites nhập phần thay đổi đầu cơ đã được chấp nhận vào bộ đệm
// StateDB chuẩn của khối. Hàm không ghi vào Pebble; App.Commit vẫn là nơi duy
// nhất lưu trạng thái bền vững.
func (s *PebbleStateDB) ApplyPendingWrites(writes map[string][]byte) error {
	for key, value := range writes {
		switch {
		case len(key) == len("acc-")+common.AddressLength && key[:len("acc-")] == "acc-":
			addr := common.BytesToAddress([]byte(key[len("acc-"):]))
			var account Account
			if err := rlp.DecodeBytes(value, &account); err != nil {
				return fmt.Errorf("decode account %s: %w", addr, err)
			}
			if account.Balance == nil {
				account.Balance = new(big.Int)
			}
			account.CodeHash = append([]byte(nil), account.CodeHash...)
			s.state[addr] = &account
			s.dirtyAccounts[addr] = true
		case len(key) == len("code-")+common.AddressLength && key[:len("code-")] == "code-":
			addr := common.BytesToAddress([]byte(key[len("code-"):]))
			code := append([]byte(nil), value...)
			s.code[addr] = code
			s.dirtyCode[addr] = true
			account := s.getAccount(addr)
			if len(code) == 0 {
				account.CodeHash = nil
			} else {
				account.CodeHash = crypto.Keccak256(code)
			}
			s.dirtyAccounts[addr] = true
		case len(key) == len("storage-")+common.AddressLength+common.HashLength && key[:len("storage-")] == "storage-":
			addr := common.BytesToAddress([]byte(key[len("storage-") : len("storage-")+common.AddressLength]))
			slot := common.BytesToHash([]byte(key[len("storage-")+common.AddressLength:]))
			if s.storage[addr] == nil {
				s.storage[addr] = make(map[common.Hash]common.Hash)
			}
			s.storage[addr][slot] = common.BytesToHash(value)
			if s.dirtyStorage[addr] == nil {
				s.dirtyStorage[addr] = make(map[common.Hash]bool)
			}
			s.dirtyStorage[addr][slot] = true
			s.dirtyAccounts[addr] = true
		default:
			return fmt.Errorf("unknown speculative state key %q", key)
		}
	}
	return nil
}
