package evm

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie/utils"
	"github.com/holiman/uint256"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// Account biểu diễn tài khoản EVM, được mã hóa bằng RLP khi lưu vào PebbleDB.
type Account struct {
	Nonce       uint64
	Balance     *big.Int
	StorageRoot common.Hash // Giá trị giữ chỗ; hiện chưa dùng cây Merkle.
	CodeHash    []byte      // Mã băm bytecode; nil nếu là tài khoản EOA.
}

// Ảnh chụp / nhật ký

// journalEntry lưu thay đổi để có thể hoàn tác.
type journalEntry struct {
	typ int // 0=số dư, 1=nonce, 2=vùng lưu trữ, 3=mã, 4=tự hủy, 5=tạm thời.

	addr common.Address

	prevBalance *big.Int

	prevNonce uint64

	key     common.Hash
	prevVal common.Hash

	prevCode     []byte
	prevCodeHash []byte

	prevSelfDestructed bool

	prevTransientVal common.Hash
}

// snapshot lưu trạng thái tại một thời điểm.
type snapshot struct {
	id         int
	journalLen int    // Vị trí trong nhật ký.
	refund     uint64 // Lượng gas được hoàn lại.
	logsLen    int    // Số lượng nhật ký sự kiện.
}

// PebbleStateDB cài đặt vm.StateDB cho geth v1.16.
type PebbleStateDB struct {
	db *store.PebbleDB
	// overlayBase là ảnh chụp giao dịch/khối bất biến, dùng cho các TxState chạy
	// lạc quan. Việc đọc dữ liệu này không làm thay đổi bộ đệm của trạng thái cha.
	overlayBase *PebbleStateDB

	// Bộ đệm tài khoản.
	state map[common.Address]*Account

	// Bộ đệm mã hợp đồng.
	code      map[common.Address][]byte
	dirtyCode map[common.Address]bool // Đánh dấu mã cần chốt.

	// Bộ đệm vùng lưu trữ.
	storage      map[common.Address]map[common.Hash]common.Hash
	dirtyStorage map[common.Address]map[common.Hash]bool // Đánh dấu ô lưu trữ cần chốt.

	// Tài khoản tự hủy.
	selfDestructed map[common.Address]bool

	// Vùng lưu trữ tạm (EIP-1153), xóa sau mỗi giao dịch.
	transientStorage map[common.Address]map[common.Hash]common.Hash

	// Phần gas được hoàn lại.
	refund uint64

	// Nhật ký sự kiện.
	logs []*types.Log

	// Ảnh chụp / nhật ký.
	journal   []journalEntry
	snapshots []snapshot
	nextRevID int

	// Danh sách truy cập (EIP-2930).
	accessList *accessList

	// Đánh dấu các tài khoản đã đổi (để tính AppHash).
	dirtyAccounts map[common.Address]bool

	// Giá trị ô lưu trữ lúc bắt đầu giao dịch hiện tại (cho GetCommittedState, EIP-2200).
	originStorage map[common.Address]map[common.Hash]common.Hash
}

// accessList lưu các địa chỉ/ô đã được làm nóng theo EIP-2929.
type accessList struct {
	addresses map[common.Address]bool
	slots     map[common.Address]map[common.Hash]bool
}

func newAccessList() *accessList {
	return &accessList{
		addresses: make(map[common.Address]bool),
		slots:     make(map[common.Address]map[common.Hash]bool),
	}
}

func NewPebbleStateDB(db *store.PebbleDB) *PebbleStateDB {
	return &PebbleStateDB{
		db:               db,
		state:            make(map[common.Address]*Account),
		code:             make(map[common.Address][]byte),
		dirtyCode:        make(map[common.Address]bool),
		storage:          make(map[common.Address]map[common.Hash]common.Hash),
		dirtyStorage:     make(map[common.Address]map[common.Hash]bool),
		selfDestructed:   make(map[common.Address]bool),
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
		logs:             make([]*types.Log, 0),
		journal:          make([]journalEntry, 0, 64),
		snapshots:        make([]snapshot, 0, 4),
		accessList:       newAccessList(),
		dirtyAccounts:    make(map[common.Address]bool),
		originStorage:    make(map[common.Address]map[common.Hash]common.Hash),
	}
}

// ============================================================
// Hàm hỗ trợ nội bộ.
// ============================================================

func (s *PebbleStateDB) getAccount(addr common.Address) *Account {
	if acc, ok := s.state[addr]; ok {
		return acc
	}
	if s.overlayBase != nil {
		acc := s.overlayBase.accountSnapshot(addr)
		s.state[addr] = acc
		return acc
	}

	// Khóa cơ sở dữ liệu: "acc-" + địa chỉ.
	key := append([]byte("acc-"), addr.Bytes()...)
	data, _ := s.db.Get(key)

	if len(data) == 0 {
		// Tạo tài khoản mới nếu chưa tồn tại.
		acc := &Account{
			Nonce:       0,
			Balance:     big.NewInt(0),
			StorageRoot: common.Hash{},
			CodeHash:    nil,
		}
		s.state[addr] = acc
		return acc
	}

	// Giải mã RLP.
	var acc Account
	if err := rlp.DecodeBytes(data, &acc); err != nil {
	panic(err) // Dừng chương trình nếu dữ liệu bị hỏng.
	}
	s.state[addr] = &acc
	return &acc
}

// accountSnapshot đọc tài khoản mà không điền vào các bộ đệm có thể thay đổi
// của StateDB nguồn. Có thể dùng an toàn cho nhiều lượt đọc lạc quan đồng thời
// khi trạng thái chuẩn chưa được phân giải.
func (s *PebbleStateDB) accountSnapshot(addr common.Address) *Account {
	if acc, ok := s.state[addr]; ok {
		clone := *acc
		if acc.Balance != nil {
			clone.Balance = new(big.Int).Set(acc.Balance)
		}
		clone.CodeHash = append([]byte(nil), acc.CodeHash...)
		return &clone
	}
	data, _ := s.db.Get(append([]byte("acc-"), addr.Bytes()...))
	if len(data) == 0 {
		return &Account{Balance: new(big.Int)}
	}
	var account Account
	if err := rlp.DecodeBytes(data, &account); err != nil {
		panic(err)
	}
	if account.Balance == nil {
		account.Balance = new(big.Int)
	}
	account.CodeHash = append([]byte(nil), account.CodeHash...)
	return &account
}

// storageKey tạo khóa vùng lưu trữ trong cơ sở dữ liệu: "storage-" + địa chỉ + khóa.
func storageKey(addr common.Address, key common.Hash) []byte {
	k := make([]byte, 0, 8+20+32)
	k = append(k, []byte("storage-")...)
	k = append(k, addr.Bytes()...)
	k = append(k, key.Bytes()...)
	return k
}

// codeKey tạo khóa mã hợp đồng trong cơ sở dữ liệu: "code-" + địa chỉ.
func codeKey(addr common.Address) []byte {
	k := make([]byte, 0, 5+20)
	k = append(k, []byte("code-")...)
	k = append(k, addr.Bytes()...)
	return k
}

// markDirty đánh dấu tài khoản đã thay đổi để tính AppHash.
func (s *PebbleStateDB) markDirty(addr common.Address) {
	s.dirtyAccounts[addr] = true
}

// ============================================================
// Cài đặt giao diện vm.StateDB (go-ethereum v1.16).
// ============================================================

func (s *PebbleStateDB) CreateAccount(addr common.Address) {
	acc := s.getAccount(addr)
	// Đặt lại trạng thái của địa chỉ mới nhưng giữ số dư (EIP-161).
	acc.Nonce = 0
	acc.CodeHash = nil
	acc.StorageRoot = common.Hash{}
	s.dirtyAccounts[addr] = true
	s.markDirty(addr)
}

func (s *PebbleStateDB) CreateContract(addr common.Address) {
	acc := s.getAccount(addr)
	acc.Nonce = 1 // Nonce của hợp đồng bắt đầu từ 1 (EIP-161).
	s.markDirty(addr)
}

// --- Số dư ---

func (s *PebbleStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acc := s.getAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:         0,
		addr:        addr,
		prevBalance: new(big.Int).Set(acc.Balance),
	})

	acc.Balance.Sub(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acc := s.getAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:         0,
		addr:        addr,
		prevBalance: new(big.Int).Set(acc.Balance),
	})

	acc.Balance.Add(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) GetBalance(addr common.Address) *uint256.Int {
	acc := s.getAccount(addr)
	bal := new(uint256.Int)
	bal.SetFromBig(acc.Balance)
	return bal
}

// --- Số thứ tự giao dịch (nonce) ---

func (s *PebbleStateDB) GetNonce(addr common.Address) uint64 {
	acc := s.getAccount(addr)
	return acc.Nonce
}

func (s *PebbleStateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	acc := s.getAccount(addr)

	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:       1,
		addr:      addr,
		prevNonce: acc.Nonce,
	})

	acc.Nonce = nonce
	s.markDirty(addr)
}

// --- Mã (bytecode hợp đồng thông minh) ---

func (s *PebbleStateDB) GetCodeHash(addr common.Address) common.Hash {
	acc := s.getAccount(addr)
	if len(acc.CodeHash) == 0 {
		// Tài khoản EOA hoặc chưa có mã: trả về mã băm rỗng theo quy định EVM.
		return common.BytesToHash(crypto.Keccak256(nil))
	}
	return common.BytesToHash(acc.CodeHash)
}

func (s *PebbleStateDB) GetCode(addr common.Address) []byte {
	// Kiểm tra bộ đệm.
	if code, ok := s.code[addr]; ok {
		return append([]byte(nil), code...)
	}
	if s.overlayBase != nil {
		data := s.overlayBase.codeSnapshot(addr)
		s.code[addr] = data
		return append([]byte(nil), data...)
	}

	// Kiểm tra cơ sở dữ liệu.
	data, _ := s.db.Get(codeKey(addr))
	if len(data) > 0 {
		s.code[addr] = data // Lưu lại vào bộ đệm.
		return append([]byte(nil), data...)
	}

	return nil
}

func (s *PebbleStateDB) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	acc := s.getAccount(addr)

	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:          3,
		addr:         addr,
		prevCode:     s.GetCode(addr),
		prevCodeHash: acc.CodeHash,
	})

	// Lưu vào bộ đệm.
	s.code[addr] = code
	s.dirtyCode[addr] = true

	// Cập nhật mã băm của mã hợp đồng.
	if len(code) > 0 {
		hash := crypto.Keccak256(code)
		acc.CodeHash = hash
	} else {
		acc.CodeHash = nil
	}

	s.markDirty(addr)

	// go-ethereum v1.16 trả về mã băm mã hợp đồng cũ.
	return append([]byte(nil), acc.CodeHash...)
}

func (s *PebbleStateDB) GetCodeSize(addr common.Address) int {
	code := s.GetCode(addr)
	return len(code)
}

// --- Hoàn lại gas ---

func (s *PebbleStateDB) AddRefund(gas uint64) {
	s.refund += gas
}

func (s *PebbleStateDB) SubRefund(gas uint64) {
	if gas > s.refund {
		panic("SubRefund: refund counter below zero")
	}
	s.refund -= gas
}

func (s *PebbleStateDB) GetRefund() uint64 {
	return s.refund
}

// --- Vùng lưu trữ hợp đồng (SLOAD/SSTORE) ---

// getStorage đọc ô lưu trữ (bộ đệm trước, rồi đến cơ sở dữ liệu).
func (s *PebbleStateDB) getStorage(addr common.Address, key common.Hash) common.Hash {
	// Kiểm tra bộ đệm.
	if slots, ok := s.storage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	if s.overlayBase != nil {
		value := s.overlayBase.storageSnapshot(addr, key)
		if s.storage[addr] == nil {
			s.storage[addr] = make(map[common.Hash]common.Hash)
		}
		s.storage[addr][key] = value
		return value
	}

	// Đọc cơ sở dữ liệu.
	data, _ := s.db.Get(storageKey(addr, key))
	if len(data) == 0 {
		return common.Hash{}
	}

	val := common.BytesToHash(data)

	// Lưu vào bộ đệm nhưng chưa đánh dấu cần ghi.
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = val
	return val
}

func (s *PebbleStateDB) codeSnapshot(addr common.Address) []byte {
	if code, ok := s.code[addr]; ok {
		return append([]byte(nil), code...)
	}
	code, _ := s.db.Get(codeKey(addr))
	return append([]byte(nil), code...)
}

func (s *PebbleStateDB) storageSnapshot(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.storage[addr]; ok {
		if value, found := slots[key]; found {
			return value
		}
	}
	data, _ := s.db.Get(storageKey(addr, key))
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// GetCommittedState trả về giá trị ô lưu trữ tại lúc bắt đầu giao dịch hiện tại
// (EIP-2200). Đây là giá trị gốc dùng để tính gas SSTORE, không phải giá trị đầu khối.
func (s *PebbleStateDB) GetCommittedState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.originStorage[addr]; ok {
		if v, ok := slots[key]; ok {
			return v
		}
	}
	// Chưa được ghi trong giao dịch này nên giá trị hiện tại cũng là giá trị đầu giao dịch.
	return s.getStorage(addr, key)
}

func (s *PebbleStateDB) GetStateAndCommittedState(addr common.Address, key common.Hash) (common.Hash, common.Hash) {
	return s.GetState(addr, key), s.GetCommittedState(addr, key)
}

func (s *PebbleStateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	return s.getStorage(addr, key)
}

func (s *PebbleStateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	prev := s.GetState(addr, key)

	// Ghi nhớ giá trị đầu giao dịch của ô lưu trữ (chỉ ghi nhớ lần ghi đầu tiên).
	if s.originStorage[addr] == nil {
		s.originStorage[addr] = make(map[common.Hash]common.Hash)
	}
	if _, ok := s.originStorage[addr][key]; !ok {
		s.originStorage[addr][key] = prev
	}

	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:     2,
		addr:    addr,
		key:     key,
		prevVal: prev,
	})

	// Ghi vào bộ đệm.
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = value

	// Đánh dấu cần ghi xuống cơ sở dữ liệu.
	if s.dirtyStorage[addr] == nil {
		s.dirtyStorage[addr] = make(map[common.Hash]bool)
	}
	s.dirtyStorage[addr][key] = true

	s.markDirty(addr)
	return prev
}

func (s *PebbleStateDB) GetStorageRoot(addr common.Address) common.Hash {
	// Giá trị giữ chỗ (chưa có cây Merkle).
	return common.Hash{}
}

// --- Vùng lưu trữ tạm thời (EIP-1153) ---

func (s *PebbleStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.transientStorage[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (s *PebbleStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	// Ghi vào nhật ký để có thể hoàn tác.
	s.journal = append(s.journal, journalEntry{
		typ:              5,
		addr:             addr,
		key:              key,
		prevTransientVal: s.GetTransientState(addr, key),
	})

	if s.transientStorage[addr] == nil {
		s.transientStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.transientStorage[addr][key] = value
}

// --- Tự hủy tài khoản ---

func (s *PebbleStateDB) SelfDestruct(addr common.Address) uint256.Int {
	bal := s.GetBalance(addr)

	// Ghi vào nhật ký.
	s.journal = append(s.journal, journalEntry{
		typ:                4,
		addr:               addr,
		prevSelfDestructed: s.selfDestructed[addr],
		prevBalance:        new(big.Int).Set(s.getAccount(addr).Balance),
	})

	s.selfDestructed[addr] = true

	// Đặt số dư về 0.
	s.getAccount(addr).Balance = big.NewInt(0)
	s.markDirty(addr)
	return *bal
}

func (s *PebbleStateDB) HasSelfDestructed(addr common.Address) bool {
	return s.selfDestructed[addr]
}

func (s *PebbleStateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	bal := s.GetBalance(addr)

	// EIP-6780: chỉ đặt số dư về 0, không xóa tài khoản.
	s.journal = append(s.journal, journalEntry{
		typ:                4,
		addr:               addr,
		prevSelfDestructed: s.selfDestructed[addr],
		prevBalance:        new(big.Int).Set(s.getAccount(addr).Balance),
	})

	s.selfDestructed[addr] = true
	s.getAccount(addr).Balance = big.NewInt(0)
	s.markDirty(addr)
	return *bal, false
}

// --- Sự tồn tại của tài khoản ---

func (s *PebbleStateDB) Exist(addr common.Address) bool {
	acc := s.getAccount(addr)
	// Tài khoản được xem là tồn tại nếu có trạng thái.
	return acc.Nonce > 0 || acc.Balance.Sign() > 0 || len(acc.CodeHash) > 0
}

func (s *PebbleStateDB) Empty(addr common.Address) bool {
	acc := s.getAccount(addr)
	return acc.Nonce == 0 && acc.Balance.Sign() == 0 && len(acc.CodeHash) == 0
}

// --- Danh sách truy cập trạng thái (EIP-2929) ---

func (s *PebbleStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	// Đặt lại danh sách truy cập ở mỗi giao dịch.
	s.accessList = newAccessList()

	// Địa chỉ người gửi luôn được làm nóng.
	s.accessList.addresses[sender] = true

	// Địa chỉ Coinbase được làm nóng (EIP-3651).
	if rules.IsShanghai {
		s.accessList.addresses[coinbase] = true
	}

	// Địa chỉ đích được làm nóng.
	if dest != nil {
		s.accessList.addresses[*dest] = true
	}

	// Các hợp đồng dựng sẵn được làm nóng.
	for _, addr := range precompiles {
		s.accessList.addresses[addr] = true
	}

	// Danh sách truy cập EIP-2930.
	for _, entry := range txAccesses {
		s.accessList.addresses[entry.Address] = true
		if s.accessList.slots[entry.Address] == nil {
			s.accessList.slots[entry.Address] = make(map[common.Hash]bool)
		}
		for _, key := range entry.StorageKeys {
			s.accessList.slots[entry.Address][key] = true
		}
	}

	// Đặt lại vùng lưu trữ tạm (EIP-1153).
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)
}

func (s *PebbleStateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.addresses[addr]
}

func (s *PebbleStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	addressOk = s.accessList.addresses[addr]
	if !addressOk {
		return false, false
	}
	if slots, ok := s.accessList.slots[addr]; ok {
		slotOk = slots[slot]
	}
	return addressOk, slotOk
}

func (s *PebbleStateDB) AddAddressToAccessList(addr common.Address) {
	s.accessList.addresses[addr] = true
}

func (s *PebbleStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	s.accessList.addresses[addr] = true
	if s.accessList.slots[addr] == nil {
		s.accessList.slots[addr] = make(map[common.Hash]bool)
	}
	s.accessList.slots[addr][slot] = true
}

// --- Ảnh chụp / hoàn tác ---

func (s *PebbleStateDB) Snapshot() int {
	id := s.nextRevID
	s.nextRevID++
	s.snapshots = append(s.snapshots, snapshot{
		id:         id,
		journalLen: len(s.journal),
		refund:     s.refund,
		logsLen:    len(s.logs),
	})
	return id
}

func (s *PebbleStateDB) RevertToSnapshot(revid int) {
	idx := -1
	for i := range s.snapshots {
		if s.snapshots[i].id == revid {
			idx = i
			break
		}
	}
	if idx < 0 {
		panic("RevertToSnapshot: invalid revision id")
	}

	snap := s.snapshots[idx]

	// Hoàn tác nhật ký theo thứ tự ngược.
	for i := len(s.journal) - 1; i >= snap.journalLen; i-- {
		entry := s.journal[i]
		switch entry.typ {
		case 0: // Số dư.
			s.getAccount(entry.addr).Balance = entry.prevBalance
		case 1: // Nonce.
			s.getAccount(entry.addr).Nonce = entry.prevNonce
		case 2: // Vùng lưu trữ.
			if s.storage[entry.addr] == nil {
				s.storage[entry.addr] = make(map[common.Hash]common.Hash)
			}
			s.storage[entry.addr][entry.key] = entry.prevVal
		case 3: // Mã hợp đồng.
			acc := s.getAccount(entry.addr)
			if entry.prevCode != nil {
				s.code[entry.addr] = entry.prevCode
			} else {
				delete(s.code, entry.addr)
			}
			acc.CodeHash = entry.prevCodeHash
		case 4: // Tự hủy tài khoản.
			s.selfDestructed[entry.addr] = entry.prevSelfDestructed
			s.getAccount(entry.addr).Balance = entry.prevBalance
		case 5: // Vùng lưu trữ tạm.
			if s.transientStorage[entry.addr] == nil {
				s.transientStorage[entry.addr] = make(map[common.Hash]common.Hash)
			}
			s.transientStorage[entry.addr][entry.key] = entry.prevTransientVal
		}
	}

	// Cắt ngắn nhật ký và danh sách ảnh chụp.
	s.journal = s.journal[:snap.journalLen]
	s.snapshots = s.snapshots[:idx]
	s.refund = snap.refund
	s.logs = s.logs[:snap.logsLen]
}

// --- Nhật ký ---

func (s *PebbleStateDB) AddLog(log *types.Log) {
	s.logs = append(s.logs, log)
}

func (s *PebbleStateDB) GetLogs() []*types.Log {
	return s.logs
}

// --- Các phương thức giao diện khác ---

func (s *PebbleStateDB) AddPreimage(hash common.Hash, preimage []byte) {
	// Không thực hiện thao tác.
}

func (s *PebbleStateDB) ForEachStorage(addr common.Address, cb func(common.Hash, common.Hash) bool) error {
	// Duyệt vùng lưu trữ trong bộ đệm.
	if slots, ok := s.storage[addr]; ok {
		for key, val := range slots {
			if !cb(key, val) {
				return nil
			}
		}
	}
	return nil
}

func (s *PebbleStateDB) Witness() *stateless.Witness {
	return nil
}

func (s *PebbleStateDB) AccessEvents() *state.AccessEvents {
	return nil
}

func (s *PebbleStateDB) PointCache() *utils.PointCache {
	return nil
}

func (s *PebbleStateDB) Finalise(deleteEmptyObjects bool) {
	// Xóa các tài khoản đã tự hủy.
	for addr := range s.selfDestructed {
		if s.selfDestructed[addr] {
			delete(s.storage, addr)
			delete(s.code, addr)
			s.markDirty(addr)
		}
	}

	// Kết thúc giao dịch: xóa nhật ký, ảnh chụp, phần hoàn gas và giá trị gốc để
	// giao dịch tiếp theo bắt đầu với trạng thái sạch.
	s.originStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.selfDestructed = make(map[common.Address]bool)
	s.journal = s.journal[:0]
	s.snapshots = s.snapshots[:0]
	s.refund = 0
}

// Commit ghi toàn bộ trạng thái đã thay đổi xuống PebbleDB.
func (s *PebbleStateDB) Commit() error {
	batch := s.db.NewBatch()
	defer batch.Close()
	writes, err := s.PendingWrites()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(writes))
	for key := range writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if writes[key] == nil {
			if err := batch.Delete([]byte(key)); err != nil {
				return err
			}
		} else if err := batch.Set([]byte(key), writes[key]); err != nil {
			return err
		}
	}
	if err := batch.WriteSync(); err != nil {
		return err
	}
	return nil
}

// StateReader cung cấp góc nhìn ổn định, chỉ đọc cho các bộ thực thi.
func (s *PebbleStateDB) Account(addr common.Address) (*Account, bool) {
	acc := s.getAccount(addr)
	if acc == nil {
		return nil, false
	}
	clone := *acc
	clone.Balance = new(big.Int).Set(acc.Balance)
	clone.CodeHash = append([]byte(nil), acc.CodeHash...)
	return &clone, s.Exist(addr)
}

func (s *PebbleStateDB) Storage(addr common.Address, key common.Hash) common.Hash {
	return s.GetState(addr, key)
}
func (s *PebbleStateDB) Code(addr common.Address) []byte {
	return append([]byte(nil), s.GetCode(addr)...)
}

// PendingWrites trả về phần thay đổi khóa/giá trị chuẩn của Pebble trong khối.
// Giá trị nil biểu thị thao tác xóa. Nhờ đó các bộ thực thi khác dùng chung
// ranh giới lưu trữ mà không cần truy cập chi tiết bên trong PebbleStateDB.
func (s *PebbleStateDB) PendingWrites() (map[string][]byte, error) {
	writes := make(map[string][]byte)
	for addr := range s.dirtyAccounts {
		acc := s.state[addr]
		if acc == nil {
			continue
		}
		value, err := rlp.EncodeToBytes(acc)
		if err != nil {
			return nil, err
		}
		writes[string(append([]byte("acc-"), addr.Bytes()...))] = value
	}
	for addr := range s.dirtyCode {
		writes[string(codeKey(addr))] = append([]byte(nil), s.code[addr]...)
	}
	for addr, slots := range s.dirtyStorage {
		for key := range slots {
			value := s.storage[addr][key]
			if value == (common.Hash{}) {
				writes[string(storageKey(addr, key))] = nil
			} else {
				writes[string(storageKey(addr, key))] = value.Bytes()
			}
		}
	}
	return writes, nil
}

// ComputeAppHash nối phần thay đổi trạng thái đã sắp xếp theo thứ tự chuẩn vào
// mã gốc của khối trước. Tham số dạng biến đổi giúp giữ tương thích với cách
// gọi cũ không truyền đối số.
func (s *PebbleStateDB) ComputeAppHash(previousRoot ...[]byte) []byte {
	var prev []byte
	if len(previousRoot) > 0 {
		prev = previousRoot[0]
	}
	keys := make([]string, 0, len(s.dirtyAccounts)+len(s.dirtyCode))
	values := make(map[string][]byte)
	for addr := range s.dirtyAccounts {
		if acc := s.state[addr]; acc != nil {
			encoded, err := rlp.EncodeToBytes(acc)
			if err != nil {
				continue
			}
			k := "account:" + string(addr.Bytes())
			keys = append(keys, k)
			values[k] = encoded
		}
	}
	for addr, slots := range s.dirtyStorage {
		for key := range slots {
			k := "storage:" + string(addr.Bytes()) + string(key.Bytes())
			keys = append(keys, k)
			val := s.storage[addr][key]
			if val != (common.Hash{}) {
				values[k] = val.Bytes()
			}
		}
	}
	for addr := range s.dirtyCode {
		k := "code:" + string(addr.Bytes())
		keys = append(keys, k)
		if code := s.code[addr]; len(code) > 0 {
			values[k] = code
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte("pluto-state-root-v1"))
	h.Write(prev)
	for _, key := range keys {
		// Tiền tố độ dài tránh ghép chuỗi nhập nhằng. Giá trị bị thiếu biểu thị thao tác xóa.
		writeRootField(h, []byte(key))
		writeRootField(h, values[key])
	}
	return h.Sum(nil)
}

func writeRootField(h interface{ Write([]byte) (int, error) }, field []byte) {
	var size [8]byte
	n := uint64(len(field))
	for i := 7; i >= 0; i-- {
		size[i] = byte(n)
		n >>= 8
	}
	_, _ = h.Write(size[:])
	_, _ = h.Write(field)
}

// AddBalance thêm số dư khởi tạo ở khối khởi nguyên.
func (s *PebbleStateDB) AddBalanceBig(addr common.Address, amount *big.Int) {
	acc := s.getAccount(addr)
	acc.Balance.Add(acc.Balance, amount)
	s.markDirty(addr)
}

// ApplyEOATransfer áp dụng ảnh hưởng giao thức của giao dịch EOA đã xác thực,
// không phí và tốn 21.000 gas, mà không tạo nhật ký EVM. Bên gọi phải kiểm tra
// nonce, số dư, chữ ký, mã của địa chỉ nhận và gas trước đó.
func (s *PebbleStateDB) ApplyEOATransfer(from, to, coinbase common.Address, nonce uint64, value *uint256.Int) error {
	if value == nil {
		return fmt.Errorf("EOA transfer has nil value")
	}
	sender := s.getAccount(from)
	if sender.Nonce != nonce {
		return fmt.Errorf("EOA transfer nonce changed: state %d, tx %d", sender.Nonce, nonce)
	}
	amount := value.ToBig()
	if sender.Balance.Cmp(amount) < 0 {
		return fmt.Errorf("EOA transfer balance changed")
	}
	if from != to {
		sender.Balance.Sub(sender.Balance, amount)
		receiver := s.getAccount(to)
		receiver.Balance.Add(receiver.Balance, amount)
		s.markDirty(to)
	}
	sender.Nonce++
	s.markDirty(from)
	// Dù giao dịch không mất phí, bước chuyển trạng thái của geth vẫn gọi hàm
	// cộng thưởng số dư cho Coinbase, khiến tài khoản đó được đánh dấu cần ghi.
	_ = s.getAccount(coinbase)
	s.markDirty(coinbase)
	return nil
}
