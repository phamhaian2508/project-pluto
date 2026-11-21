package evm

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// LogicalAccount biểu diễn trạng thái logic của một tài khoản EVM dùng để đối chiếu với post-alloc của evm t8n.
type LogicalAccount struct {
	Nonce   uint64
	Balance *uint256.Int
	Code    []byte
	Storage map[common.Hash]common.Hash
}

// Clone tạo bản sao độc lập của LogicalAccount.
func (a *LogicalAccount) Clone() *LogicalAccount {
	if a == nil {
		return nil
	}
	bal := new(uint256.Int)
	if a.Balance != nil {
		bal.Set(a.Balance)
	}
	var code []byte
	if len(a.Code) > 0 {
		code = bytes.Clone(a.Code)
	}
	storage := make(map[common.Hash]common.Hash, len(a.Storage))
	for k, v := range a.Storage {
		if v != (common.Hash{}) {
			storage[k] = v
		}
	}
	return &LogicalAccount{
		Nonce:   a.Nonce,
		Balance: bal,
		Code:    code,
		Storage: storage,
	}
}

// LogicalState chứa toàn bộ trạng thái logic đọc từ PebbleStateDB + store.PebbleDB hoặc từ post-alloc Geth.
type LogicalState struct {
	Accounts   map[common.Address]*LogicalAccount
	OrphanKeys [][]byte
}

// NewLogicalState khởi tạo trạng thái logic rỗng.
func NewLogicalState() *LogicalState {
	return &LogicalState{
		Accounts: make(map[common.Address]*LogicalAccount),
	}
}

// Clone sao chép sâu toàn bộ LogicalState.
func (s *LogicalState) Clone() *LogicalState {
	out := NewLogicalState()
	if s == nil {
		return out
	}
	for addr, acc := range s.Accounts {
		out.Accounts[addr] = acc.Clone()
	}
	if len(s.OrphanKeys) > 0 {
		out.OrphanKeys = make([][]byte, len(s.OrphanKeys))
		for i, k := range s.OrphanKeys {
			out.OrphanKeys[i] = bytes.Clone(k)
		}
	}
	return out
}

// SeedLogicalState nạp trạng thái genesis (kể cả account rỗng có storage) vào PebbleStateDB.
func (s *PebbleStateDB) SeedLogicalState(ls *LogicalState) error {
	if ls == nil {
		return nil
	}
	addrs := make([]common.Address, 0, len(ls.Accounts))
	for addr := range ls.Accounts {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})
	for _, addr := range addrs {
		la := ls.Accounts[addr]
		if la == nil {
			continue
		}
		bal := new(big.Int)
		if la.Balance != nil {
			bal = la.Balance.ToBig()
		}
		acc := &Account{
			Nonce:   la.Nonce,
			Balance: bal,
		}
		if len(la.Code) > 0 {
			codeCopy := bytes.Clone(la.Code)
			acc.CodeHash = crypto.Keccak256(codeCopy)
			s.code[addr] = codeCopy
			s.dirtyCode[addr] = true
		}
		s.state[addr] = acc
		delete(s.missingAccounts, addr)
		delete(s.deletedAccounts, addr)
		s.dirtyAccounts[addr] = true

		if len(la.Storage) > 0 {
			s.ensureMutableStorage(addr)
			if s.storage[addr] == nil {
				s.storage[addr] = make(map[common.Hash]common.Hash, len(la.Storage))
			}
			if s.dirtyStorage[addr] == nil {
				s.dirtyStorage[addr] = make(map[common.Hash]bool, len(la.Storage))
			}
			for slot, val := range la.Storage {
				if val == (common.Hash{}) {
					continue
				}
				s.storage[addr][slot] = val
				s.dirtyStorage[addr][slot] = true
			}
		}
	}
	return s.Err()
}

// ExportLogicalState xuất toàn bộ trạng thái logic (accounts, nonce, balance, code, non-zero storage slots)
// từ PebbleDB kết hợp bộ đệm hiện tại của PebbleStateDB (và overlayBase nếu có), đồng thời phát hiện khóa mồ côi.
func (s *PebbleStateDB) ExportLogicalState() (*LogicalState, error) {
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("export logical state aborted due to db read error: %w", err)
	}

	out := NewLogicalState()
	expectedCodeHash := make(map[common.Address][]byte)
	rawCode := make(map[common.Address][]byte)
	rawCodeKeys := make(map[common.Address][]byte)
	rawStorage := make(map[common.Address]map[common.Hash]common.Hash)
	rawStorageKeys := make(map[common.Address]map[common.Hash][]byte)

	// 1. Quét toàn bộ khóa acc-/code-/storage- đã lưu trong PebbleDB
	if s.db != nil {
		iter, err := s.db.Iterator(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("export logical state iterator: %w", err)
		}
		defer iter.Close()
		for ; iter.Valid(); iter.Next() {
			k := iter.Key()
			v := iter.Value()
			switch {
			case len(k) == len("acc-")+common.AddressLength && string(k[:len("acc-")]) == "acc-":
				addr := common.BytesToAddress(k[len("acc-"):])
				var acc Account
				if err := rlp.DecodeBytes(v, &acc); err != nil {
					return nil, fmt.Errorf("decode account %s during export: %w", addr.Hex(), err)
				}
				bal := new(uint256.Int)
				if acc.Balance != nil {
					u, overflow := uint256.FromBig(acc.Balance)
					if overflow {
						return nil, fmt.Errorf("account %s balance overflows uint256", addr.Hex())
					}
					bal = u
				}
				out.Accounts[addr] = &LogicalAccount{
					Nonce:   acc.Nonce,
					Balance: bal,
					Storage: make(map[common.Hash]common.Hash),
				}
				if len(acc.CodeHash) > 0 {
					expectedCodeHash[addr] = bytes.Clone(acc.CodeHash)
				} else {
					delete(expectedCodeHash, addr)
				}
			case len(k) == len("code-")+common.AddressLength && string(k[:len("code-")]) == "code-":
				addr := common.BytesToAddress(k[len("code-"):])
				if len(v) > 0 {
					rawCode[addr] = bytes.Clone(v)
					rawCodeKeys[addr] = bytes.Clone(k)
				}
			case len(k) == len("storage-")+common.AddressLength+common.HashLength && string(k[:len("storage-")]) == "storage-":
				addr := common.BytesToAddress(k[len("storage-") : len("storage-")+common.AddressLength])
				slot := common.BytesToHash(k[len("storage-")+common.AddressLength:])
				if rawStorage[addr] == nil {
					rawStorage[addr] = make(map[common.Hash]common.Hash)
					rawStorageKeys[addr] = make(map[common.Hash][]byte)
				}
				rawStorage[addr][slot] = common.BytesToHash(v)
				rawStorageKeys[addr][slot] = bytes.Clone(k)
			}
		}
		if err := iter.Error(); err != nil {
			return nil, fmt.Errorf("export logical state iterator error: %w", err)
		}
	}

	// 2. Gom chuỗi lớp phủ từ overlayBase -> s (từ gốc lên đỉnh)
	var layers []*PebbleStateDB
	for cur := s; cur != nil; cur = cur.overlayBase {
		layers = append(layers, cur)
	}
	for i := len(layers) - 1; i >= 0; i-- {
		layer := layers[i]
		for addr, deleted := range layer.deletedAccounts {
			if deleted {
				delete(out.Accounts, addr)
				delete(expectedCodeHash, addr)
				delete(rawCode, addr)
				delete(rawCodeKeys, addr)
				delete(rawStorage, addr)
				delete(rawStorageKeys, addr)
			}
		}
		for addr, cleared := range layer.storageCleared {
			if cleared {
				delete(expectedCodeHash, addr)
				delete(rawCode, addr)
				delete(rawCodeKeys, addr)
				delete(rawStorage, addr)
				delete(rawStorageKeys, addr)
			}
		}
		for addr, acc := range layer.state {
			if acc == nil || layer.deletedAccounts[addr] {
				continue
			}
			bal := new(uint256.Int)
			if acc.Balance != nil {
				u, overflow := uint256.FromBig(acc.Balance)
				if overflow {
					return nil, fmt.Errorf("buffered account %s balance overflows uint256", addr.Hex())
				}
				bal = u
			}
			la := out.Accounts[addr]
			if la == nil {
				la = &LogicalAccount{Storage: make(map[common.Hash]common.Hash)}
				out.Accounts[addr] = la
			}
			la.Nonce = acc.Nonce
			la.Balance = bal
			if len(acc.CodeHash) > 0 {
				expectedCodeHash[addr] = bytes.Clone(acc.CodeHash)
			} else {
				delete(expectedCodeHash, addr)
			}
		}
		for addr := range layer.dirtyCode {
			if layer.deletedAccounts[addr] {
				continue
			}
			code := layer.code[addr]
			if len(code) == 0 {
				delete(rawCode, addr)
				delete(rawCodeKeys, addr)
			} else {
				rawCode[addr] = bytes.Clone(code)
				rawCodeKeys[addr] = codeKey(addr)
			}
		}
		for addr, slots := range layer.dirtyStorage {
			if layer.deletedAccounts[addr] {
				continue
			}
			if rawStorage[addr] == nil {
				rawStorage[addr] = make(map[common.Hash]common.Hash)
				rawStorageKeys[addr] = make(map[common.Hash][]byte)
			}
			for slot := range slots {
				val := layer.storage[addr][slot]
				if val == (common.Hash{}) {
					delete(rawStorage[addr], slot)
					delete(rawStorageKeys[addr], slot)
				} else {
					rawStorage[addr][slot] = val
					rawStorageKeys[addr][slot] = storageKey(addr, slot)
				}
			}
		}
	}

	// 3. Gắn code & storage vào account tương ứng; đối chiếu acc.CodeHash == keccak256(code) để bắt lỗi lệch metadata
	for addr, code := range rawCode {
		acc, ok := out.Accounts[addr]
		if !ok {
			out.OrphanKeys = append(out.OrphanKeys, rawCodeKeys[addr])
			continue
		}
		acc.Code = code
	}
	for addr, acc := range out.Accounts {
		wantHash := expectedCodeHash[addr]
		if len(acc.Code) == 0 {
			if len(wantHash) > 0 {
				return nil, fmt.Errorf("account %s has CodeHash %x but missing bytecode", addr.Hex(), wantHash)
			}
			continue
		}
		gotHash := crypto.Keccak256(acc.Code)
		if !bytes.Equal(wantHash, gotHash) {
			return nil, fmt.Errorf("account %s CodeHash mismatch: acc=%x keccak256(code)=%x", addr.Hex(), wantHash, gotHash)
		}
	}
	for addr, slots := range rawStorage {
		acc, ok := out.Accounts[addr]
		if !ok {
			for slot := range slots {
				out.OrphanKeys = append(out.OrphanKeys, rawStorageKeys[addr][slot])
			}
			continue
		}
		for slot, val := range slots {
			if val == (common.Hash{}) {
				// slot giá trị 0 vẫn còn trong Pebble bị coi là khóa thừa/zero-key
				out.OrphanKeys = append(out.OrphanKeys, rawStorageKeys[addr][slot])
				continue
			}
			acc.Storage[slot] = val
		}
	}
	sort.Slice(out.OrphanKeys, func(i, j int) bool {
		return bytes.Compare(out.OrphanKeys[i], out.OrphanKeys[j]) < 0
	})
	return out, nil
}
