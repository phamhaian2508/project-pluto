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
	overlay.kv = s.kv
	overlay.overlayBase = s
	return overlay
}

// Fork tạo một bản chụp cô lập tại ranh giới giao dịch. Các lần đọc chưa lưu
// đệm vẫn dùng trạng thái đã chốt bất biến của Pebble; mọi map và nhật ký mà EVM
// có thể thay đổi đều thuộc riêng về bản sao này.
func (s *PebbleStateDB) Fork() *PebbleStateDB {
	fork := NewPebbleStateDB(s.db)
	fork.kv = s.kv
	fork.overlayBase = s.overlayBase
	for addr, account := range s.state {
		fork.state[addr] = cloneAccount(account)
	}
	for addr, missing := range s.missingAccounts {
		fork.missingAccounts[addr] = missing
	}
	for addr, deleted := range s.deletedAccounts {
		fork.deletedAccounts[addr] = deleted
	}
	for addr, cleared := range s.storageCleared {
		fork.storageCleared[addr] = cleared
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

// ForkImmutable tạo ảnh chụp chỉ-đọc gọn nhẹ dùng làm overlayBase bất biến cho nhiều worker song song.
// Map storage từng địa chỉ dùng copy-on-write: chỉ clone khi bản cha bị ghi đè sau snapshot.
func (s *PebbleStateDB) ForkImmutable() *PebbleStateDB {
	snap := NewPebbleStateDB(s.db)
	snap.kv = s.kv
	snap.overlayBase = s.overlayBase
	for addr, account := range s.state {
		snap.state[addr] = cloneAccount(account)
	}
	for addr, missing := range s.missingAccounts {
		snap.missingAccounts[addr] = missing
	}
	for addr, deleted := range s.deletedAccounts {
		snap.deletedAccounts[addr] = deleted
	}
	for addr, cleared := range s.storageCleared {
		snap.storageCleared[addr] = cleared
	}
	// slice bytecode không bao giờ bị sửa tại chỗ (SetCode luôn thay slice mới)
	for addr, code := range s.code {
		snap.code[addr] = code
	}
	for addr, slots := range s.storage {
		s.frozenStorage[addr] = true
		snap.storage[addr] = slots
		snap.frozenStorage[addr] = true
	}
	return snap
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
	deletedInBatch := make(map[common.Address]bool)
	// áp acc- trước để xác định account bị xóa hay tạo mới trước khi áp code/storage
	for key, value := range writes {
		if len(key) == len("acc-")+common.AddressLength && key[:len("acc-")] == "acc-" {
			addr := common.BytesToAddress([]byte(key[len("acc-"):]))
			if value == nil {
				deletedInBatch[addr] = true
				delete(s.state, addr)
				delete(s.code, addr)
				delete(s.dirtyCode, addr)
				delete(s.storage, addr)
				delete(s.dirtyStorage, addr)
				delete(s.frozenStorage, addr)
				s.storageCleared[addr] = true
				// chỉ đánh dấu deleted/dirty khi account từng tồn tại ở base để khớp AppHash với sequential
				if s.existedInBase(addr) {
					s.deletedAccounts[addr] = true
					delete(s.missingAccounts, addr)
					s.dirtyAccounts[addr] = true
				} else {
					delete(s.deletedAccounts, addr)
					delete(s.dirtyAccounts, addr)
					s.missingAccounts[addr] = true
				}
				continue
			}
			var account Account
			if err := rlp.DecodeBytes(value, &account); err != nil {
				return fmt.Errorf("decode account %s: %w", addr, err)
			}
			if account.Balance == nil {
				account.Balance = new(big.Int)
			}
			account.CodeHash = append([]byte(nil), account.CodeHash...)
			s.state[addr] = &account
			delete(s.deletedAccounts, addr)
			delete(s.missingAccounts, addr)
			s.dirtyAccounts[addr] = true
		}
	}

	for key, value := range writes {
		switch {
		case len(key) == len("acc-")+common.AddressLength && key[:len("acc-")] == "acc-":
			continue
		case len(key) == len("code-")+common.AddressLength && key[:len("code-")] == "code-":
			addr := common.BytesToAddress([]byte(key[len("code-"):]))
			if s.deletedAccounts[addr] || deletedInBatch[addr] {
				continue
			}
			code := append([]byte(nil), value...)
			if len(code) == 0 {
				delete(s.code, addr)
			} else {
				s.code[addr] = code
			}
			s.dirtyCode[addr] = true
			account := s.getOrCreateAccount(addr)
			if len(code) == 0 {
				account.CodeHash = nil
			} else {
				account.CodeHash = crypto.Keccak256(code)
			}
			s.dirtyAccounts[addr] = true
		case len(key) == len("storage-")+common.AddressLength+common.HashLength && key[:len("storage-")] == "storage-":
			addr := common.BytesToAddress([]byte(key[len("storage-") : len("storage-")+common.AddressLength]))
			if s.deletedAccounts[addr] || deletedInBatch[addr] {
				continue
			}
			slot := common.BytesToHash([]byte(key[len("storage-")+common.AddressLength:]))
			s.ensureMutableStorage(addr)
			if s.storage[addr] == nil {
				s.storage[addr] = make(map[common.Hash]common.Hash)
			}
			if value == nil {
				s.storage[addr][slot] = common.Hash{}
			} else {
				s.storage[addr][slot] = common.BytesToHash(value)
			}
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
