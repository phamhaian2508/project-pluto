package evm

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"
	"sync"

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
	dbm "github.com/cometbft/cometbft-db"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// ReadOnlyKV là nguồn đọc chỉ-đọc (ví dụ *store.PebbleSnapshot) giúp eth_call/RPC đọc nhất quán 1 thời điểm.
type ReadOnlyKV interface {
	Get(key []byte) ([]byte, error)
	Has(key []byte) (bool, error)
	Iterator(start, end []byte) (dbm.Iterator, error)
}

type stateKV interface {
	ReadOnlyKV
}

var (
	// sentinel khác zero hash và EmptyRootHash để evm.create phát hiện address collision khi có storage != 0
	nonEmptyStorageSentinel = crypto.Keccak256Hash([]byte("pluto-non-empty-storage-root"))
	ripemdPrecompileAddr    = common.HexToAddress("0x0000000000000000000000000000000000000003")
)

// Account biểu diễn tài khoản EVM, được mã hóa bằng RLP khi lưu vào PebbleDB.
type Account struct {
	Nonce       uint64
	Balance     *big.Int
	StorageRoot common.Hash // Giá trị giữ chỗ; hiện chưa dùng cây Merkle.
	CodeHash    []byte      // Mã băm bytecode; nil nếu là tài khoản EOA.
}

func isEmptyAccount(acc *Account) bool {
	if acc == nil {
		return true
	}
	balZero := acc.Balance == nil || acc.Balance.Sign() == 0
	return acc.Nonce == 0 && balZero && len(acc.CodeHash) == 0
}

// Ảnh chụp / nhật ký

const (
	journalBalance = iota
	journalNonce
	journalStorage
	journalCode
	journalSelfDestruct
	journalTransient
	journalAccountCreate
	journalTouch
	journalAccessListAddr
	journalAccessListSlot
	journalNewContract
)

// journalEntry lưu thay đổi để có thể hoàn tác.
type journalEntry struct {
	typ  int
	addr common.Address

	prevBalance *big.Int
	prevNonce   uint64

	key           common.Hash
	prevVal       common.Hash
	prevHadSlot   bool
	prevDirtySlot bool

	prevCode      []byte
	prevHadCode   bool
	prevCodeHash  []byte
	prevDirtyCode bool

	prevSelfDestructed bool
	prevTransientVal   common.Hash

	prevAccount     *Account
	prevExisted     bool
	prevMissing     bool
	prevDeleted     bool
	prevDirtyAcc    bool
	prevTouched     bool
	prevNewContract bool
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
	// kv trỏ tới db ở production; test có thể thay bằng stub trả lỗi I/O để kiểm tra lan truyền lỗi đọc DB
	kv stateKV
	// overlayBase là ảnh chụp giao dịch/khối bất biến, dùng cho các TxState chạy
	// lạc quan. Việc đọc dữ liệu này không làm thay đổi bộ đệm của trạng thái cha.
	overlayBase *PebbleStateDB

	// dbErr ghi lại lỗi đọc Pebble đầu tiên để không âm thầm trả về tài khoản/slot rỗng
	dbErrMu sync.Mutex
	dbErr   error

	// Bộ đệm tài khoản tồn tại trong trạng thái hiện tại.
	state           map[common.Address]*Account
	missingAccounts map[common.Address]bool // negative cache tránh lặp db.Get cho addr vắng
	deletedAccounts map[common.Address]bool // đánh dấu account đã bị xóa trong block
	storageCleared  map[common.Address]bool // account từng bị xóa -> không đọc slot cũ từ DB

	// Bộ đệm mã hợp đồng.
	code      map[common.Address][]byte
	dirtyCode map[common.Address]bool // Đánh dấu mã cần chốt.

	// Bộ đệm vùng lưu trữ.
	storage       map[common.Address]map[common.Hash]common.Hash
	dirtyStorage  map[common.Address]map[common.Hash]bool // Đánh dấu ô lưu trữ cần chốt.
	frozenStorage map[common.Address]bool                 // CoW flag: clone trước khi ghi nếu đã share qua ForkImmutable

	// Tài khoản tự hủy và hợp đồng mới tạo trong tx hiện tại.
	selfDestructed map[common.Address]bool
	newContracts   map[common.Address]bool
	txTouched      map[common.Address]bool // account được tạo/sửa/chạm trong tx hiện tại cho EIP-161 Finalise

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

	// Đánh dấu các tài khoản đã đổi (để tính AppHash / PendingWrites).
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
	var kv stateKV
	if db != nil {
		kv = db
	}
	return newPebbleStateDB(db, kv)
}

// NewPebbleStateDBFromKV tạo StateDB chỉ-đọc gắn với snapshot KV cố định (không hỗ trợ Commit).
func NewPebbleStateDBFromKV(kv ReadOnlyKV) *PebbleStateDB {
	return newPebbleStateDB(nil, kv)
}

func newPebbleStateDB(db *store.PebbleDB, kv stateKV) *PebbleStateDB {
	return &PebbleStateDB{
		db:               db,
		kv:               kv,
		state:            make(map[common.Address]*Account),
		missingAccounts:  make(map[common.Address]bool),
		deletedAccounts:  make(map[common.Address]bool),
		storageCleared:   make(map[common.Address]bool),
		code:             make(map[common.Address][]byte),
		dirtyCode:        make(map[common.Address]bool),
		storage:          make(map[common.Address]map[common.Hash]common.Hash),
		dirtyStorage:     make(map[common.Address]map[common.Hash]bool),
		frozenStorage:    make(map[common.Address]bool),
		selfDestructed:   make(map[common.Address]bool),
		newContracts:     make(map[common.Address]bool),
		txTouched:        make(map[common.Address]bool),
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
		logs:             make([]*types.Log, 0),
		journal:          make([]journalEntry, 0, 64),
		snapshots:        make([]snapshot, 0, 4),
		accessList:       newAccessList(),
		dirtyAccounts:    make(map[common.Address]bool),
		originStorage:    make(map[common.Address]map[common.Hash]common.Hash),
	}
}

// recordDBError lưu lỗi đọc DB đầu tiên (thread-safe).
func (s *PebbleStateDB) recordDBError(err error) {
	if err == nil || s == nil {
		return
	}
	s.dbErrMu.Lock()
	if s.dbErr == nil {
		s.dbErr = err
	}
	s.dbErrMu.Unlock()
}

// Err trả về lỗi đọc DB đã xảy ra trong phiên StateDB (hoặc overlayBase).
func (s *PebbleStateDB) Err() error {
	if s == nil {
		return nil
	}
	s.dbErrMu.Lock()
	err := s.dbErr
	s.dbErrMu.Unlock()
	if err != nil {
		return err
	}
	if s.overlayBase != nil {
		return s.overlayBase.Err()
	}
	return nil
}

// ============================================================
// Hàm hỗ trợ nội bộ.
// ============================================================

func cloneAccount(acc *Account) *Account {
	if acc == nil {
		return nil
	}
	clone := *acc
	if acc.Balance != nil {
		clone.Balance = new(big.Int).Set(acc.Balance)
	} else {
		clone.Balance = new(big.Int)
	}
	clone.CodeHash = append([]byte(nil), acc.CodeHash...)
	return &clone
}

// loadAccount đọc tài khoản đã tồn tại mà không tạo tài khoản rỗng trong s.state.
func (s *PebbleStateDB) loadAccount(addr common.Address) (*Account, bool) {
	if s.deletedAccounts[addr] {
		return nil, false
	}
	if acc, ok := s.state[addr]; ok {
		return acc, true
	}
	if s.missingAccounts[addr] {
		return nil, false
	}
	if s.overlayBase != nil {
		acc, ok, err := s.overlayBase.accountSnapshot(addr)
		if err != nil {
			s.recordDBError(err)
			return nil, false
		}
		if !ok {
			s.missingAccounts[addr] = true
			return nil, false
		}
		s.state[addr] = acc
		return acc, true
	}
	if s.kv == nil {
		s.missingAccounts[addr] = true
		return nil, false
	}

	key := append([]byte("acc-"), addr.Bytes()...)
	data, err := s.kv.Get(key)
	if err != nil {
		s.recordDBError(fmt.Errorf("db get account %s: %w", addr.Hex(), err))
		return nil, false
	}
	if len(data) == 0 {
		s.missingAccounts[addr] = true
		return nil, false
	}

	var acc Account
	if err := rlp.DecodeBytes(data, &acc); err != nil {
		s.recordDBError(fmt.Errorf("decode account %s: %w", addr.Hex(), err))
		return nil, false
	}
	if acc.Balance == nil {
		acc.Balance = new(big.Int)
	}
	s.state[addr] = &acc
	return &acc, true
}

// existedInBase kiểm tra tài khoản có tồn tại trong overlayBase hoặc PebbleDB gốc trước block hay không.
func (s *PebbleStateDB) existedInBase(addr common.Address) bool {
	if s.overlayBase != nil {
		_, ok, err := s.overlayBase.accountSnapshot(addr)
		if err != nil {
			s.recordDBError(err)
			return false
		}
		return ok
	}
	if s.kv == nil {
		return false
	}
	has, err := s.kv.Has(append([]byte("acc-"), addr.Bytes()...))
	if err != nil {
		s.recordDBError(fmt.Errorf("db has account %s: %w", addr.Hex(), err))
		return false
	}
	return has
}

// getOrCreateAccount trả về tài khoản hiện có hoặc tạo mới có ghi journal để RevertToSnapshot hoàn tác sạch.
func (s *PebbleStateDB) getOrCreateAccount(addr common.Address) *Account {
	if acc, ok := s.loadAccount(addr); ok {
		return acc
	}
	prevAcc, prevExisted := s.state[addr]
	s.journal = append(s.journal, journalEntry{
		typ:          journalAccountCreate,
		addr:         addr,
		prevAccount:  cloneAccount(prevAcc),
		prevExisted:  prevExisted,
		prevMissing:  s.missingAccounts[addr],
		prevDeleted:  s.deletedAccounts[addr],
		prevDirtyAcc: s.dirtyAccounts[addr],
		prevTouched:  s.txTouched[addr],
	})
	acc := &Account{
		Nonce:       0,
		Balance:     big.NewInt(0),
		StorageRoot: common.Hash{},
		CodeHash:    nil,
	}
	s.state[addr] = acc
	delete(s.missingAccounts, addr)
	delete(s.deletedAccounts, addr)
	s.txTouched[addr] = true
	return acc
}

// getAccount giữ tương thích nội bộ: trả tài khoản (tạo mới nếu chưa có).
func (s *PebbleStateDB) getAccount(addr common.Address) *Account {
	return s.getOrCreateAccount(addr)
}

// accountSnapshot đọc tài khoản mà không điền vào các bộ đệm có thể thay đổi
// của StateDB nguồn. An toàn cho nhiều goroutine đọc đồng thời.
func (s *PebbleStateDB) accountSnapshot(addr common.Address) (*Account, bool, error) {
	if s.deletedAccounts[addr] {
		return nil, false, nil
	}
	if acc, ok := s.state[addr]; ok {
		return cloneAccount(acc), true, nil
	}
	if s.missingAccounts[addr] {
		return nil, false, nil
	}
	if s.overlayBase != nil {
		return s.overlayBase.accountSnapshot(addr)
	}
	if s.kv == nil {
		return nil, false, nil
	}
	data, err := s.kv.Get(append([]byte("acc-"), addr.Bytes()...))
	if err != nil {
		return nil, false, fmt.Errorf("db get account snapshot %s: %w", addr.Hex(), err)
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	var account Account
	if err := rlp.DecodeBytes(data, &account); err != nil {
		return nil, false, fmt.Errorf("decode account snapshot %s: %w", addr.Hex(), err)
	}
	if account.Balance == nil {
		account.Balance = new(big.Int)
	}
	account.CodeHash = append([]byte(nil), account.CodeHash...)
	return &account, true, nil
}

func (s *PebbleStateDB) ensureMutableStorage(addr common.Address) {
	if !s.frozenStorage[addr] {
		return
	}
	if slots, ok := s.storage[addr]; ok {
		s.storage[addr] = cloneHashMap(slots)
	}
	delete(s.frozenStorage, addr)
}

// storageKey tạo khóa vùng lưu trữ trong cơ sở dữ liệu: "storage-" + địa chỉ + khóa.
func storageKey(addr common.Address, key common.Hash) []byte {
	k := make([]byte, 0, 8+20+32)
	k = append(k, []byte("storage-")...)
	k = append(k, addr.Bytes()...)
	k = append(k, key.Bytes()...)
	return k
}

func storagePrefix(addr common.Address) []byte {
	k := make([]byte, 0, 8+20)
	k = append(k, []byte("storage-")...)
	k = append(k, addr.Bytes()...)
	return k
}

func prefixUpperBound(prefix []byte) []byte {
	out := append([]byte(nil), prefix...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

// codeKey tạo khóa mã hợp đồng trong cơ sở dữ liệu: "code-" + địa chỉ.
func codeKey(addr common.Address) []byte {
	k := make([]byte, 0, 5+20)
	k = append(k, []byte("code-")...)
	k = append(k, addr.Bytes()...)
	return k
}

// markDirty đánh dấu tài khoản đã thay đổi để tính AppHash / PendingWrites.
func (s *PebbleStateDB) markDirty(addr common.Address) {
	s.dirtyAccounts[addr] = true
	s.txTouched[addr] = true
}

func (s *PebbleStateDB) touchAccount(addr common.Address) {
	if !s.txTouched[addr] {
		s.journal = append(s.journal, journalEntry{
			typ:         journalTouch,
			addr:        addr,
			prevTouched: false,
		})
		s.txTouched[addr] = true
	}
}

// ============================================================
// Cài đặt giao diện vm.StateDB (go-ethereum v1.16).
// ============================================================

func (s *PebbleStateDB) CreateAccount(addr common.Address) {
	prevAcc, prevExisted := s.loadAccount(addr)
	s.journal = append(s.journal, journalEntry{
		typ:          journalAccountCreate,
		addr:         addr,
		prevAccount:  cloneAccount(prevAcc),
		prevExisted:  prevExisted,
		prevMissing:  s.missingAccounts[addr],
		prevDeleted:  s.deletedAccounts[addr],
		prevDirtyAcc: s.dirtyAccounts[addr],
		prevTouched:  s.txTouched[addr],
	})
	bal := big.NewInt(0)
	if prevExisted && prevAcc != nil && prevAcc.Balance != nil {
		// giữ số dư có sẵn khi tạo tài khoản (EIP-161)
		bal = new(big.Int).Set(prevAcc.Balance)
	}
	s.state[addr] = &Account{
		Nonce:       0,
		Balance:     bal,
		StorageRoot: common.Hash{},
		CodeHash:    nil,
	}
	delete(s.missingAccounts, addr)
	delete(s.deletedAccounts, addr)
	s.markDirty(addr)
}

func (s *PebbleStateDB) CreateContract(addr common.Address) {
	// Geth evm.create tự gọi SetNonce(addr, 1) ngay sau CreateContract; chỉ đánh dấu newContract cho EIP-6780
	if !s.newContracts[addr] {
		s.journal = append(s.journal, journalEntry{
			typ:             journalNewContract,
			addr:            addr,
			prevNewContract: false,
		})
		s.newContracts[addr] = true
	}
}

// --- Số dư ---

func (s *PebbleStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if amount == nil || amount.IsZero() {
		return *s.GetBalance(addr)
	}
	acc := s.getOrCreateAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	s.journal = append(s.journal, journalEntry{
		typ:          journalBalance,
		addr:         addr,
		prevBalance:  new(big.Int).Set(acc.Balance),
		prevDirtyAcc: s.dirtyAccounts[addr],
		prevTouched:  s.txTouched[addr],
	})

	acc.Balance.Sub(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if amount == nil || amount.IsZero() {
		acc, ok := s.loadAccount(addr)
		if !ok || isEmptyAccount(acc) {
			// giữ đúng EIP-161: AddBalance(0) lên account rỗng chỉ chạm để Finalise(true) xóa, không giữ account rỗng
			if ok || addr == ripemdPrecompileAddr {
				s.touchAccount(addr)
			}
			return uint256.Int{}
		}
		prev := new(uint256.Int)
		prev.SetFromBig(acc.Balance)
		return *prev
	}
	acc := s.getOrCreateAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	s.journal = append(s.journal, journalEntry{
		typ:          journalBalance,
		addr:         addr,
		prevBalance:  new(big.Int).Set(acc.Balance),
		prevDirtyAcc: s.dirtyAccounts[addr],
		prevTouched:  s.txTouched[addr],
	})

	acc.Balance.Add(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) GetBalance(addr common.Address) *uint256.Int {
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil || acc.Balance == nil {
		return new(uint256.Int)
	}
	bal := new(uint256.Int)
	bal.SetFromBig(acc.Balance)
	return bal
}

// --- Số thứ tự giao dịch (nonce) ---

func (s *PebbleStateDB) GetNonce(addr common.Address) uint64 {
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil {
		return 0
	}
	return acc.Nonce
}

func (s *PebbleStateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	acc := s.getOrCreateAccount(addr)

	s.journal = append(s.journal, journalEntry{
		typ:          journalNonce,
		addr:         addr,
		prevNonce:    acc.Nonce,
		prevDirtyAcc: s.dirtyAccounts[addr],
		prevTouched:  s.txTouched[addr],
	})

	acc.Nonce = nonce
	s.markDirty(addr)
}

// --- Mã (bytecode hợp đồng thông minh) ---

func (s *PebbleStateDB) GetCodeHash(addr common.Address) common.Hash {
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil {
		// EIP-1052: tài khoản không tồn tại trả zero hash
		return common.Hash{}
	}
	if len(acc.CodeHash) == 0 {
		return types.EmptyCodeHash
	}
	return common.BytesToHash(acc.CodeHash)
}

func (s *PebbleStateDB) GetCode(addr common.Address) []byte {
	if s.deletedAccounts[addr] {
		return nil
	}
	if code, ok := s.code[addr]; ok {
		return append([]byte(nil), code...)
	}
	if s.storageCleared[addr] {
		return nil
	}
	if s.overlayBase != nil {
		data, err := s.overlayBase.codeSnapshot(addr)
		if err != nil {
			s.recordDBError(err)
			return nil
		}
		s.code[addr] = data
		return append([]byte(nil), data...)
	}
	if s.kv == nil {
		return nil
	}

	data, err := s.kv.Get(codeKey(addr))
	if err != nil {
		s.recordDBError(fmt.Errorf("db get code %s: %w", addr.Hex(), err))
		return nil
	}
	if len(data) > 0 {
		s.code[addr] = data
		return append([]byte(nil), data...)
	}
	return nil
}

func (s *PebbleStateDB) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	acc := s.getOrCreateAccount(addr)
	prevCode := s.GetCode(addr)
	_, prevHadCode := s.code[addr]

	s.journal = append(s.journal, journalEntry{
		typ:           journalCode,
		addr:          addr,
		prevCode:      prevCode,
		prevHadCode:   prevHadCode,
		prevCodeHash:  append([]byte(nil), acc.CodeHash...),
		prevDirtyCode: s.dirtyCode[addr],
		prevDirtyAcc:  s.dirtyAccounts[addr],
		prevTouched:   s.txTouched[addr],
	})

	s.code[addr] = append([]byte(nil), code...)
	s.dirtyCode[addr] = true

	if len(code) > 0 {
		acc.CodeHash = crypto.Keccak256(code)
	} else {
		acc.CodeHash = nil
	}

	s.markDirty(addr)
	return prevCode
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

// getStorage đọc ô lưu trữ (bộ đệm trước, rồi đến overlayBase / cơ sở dữ liệu).
func (s *PebbleStateDB) getStorage(addr common.Address, key common.Hash) common.Hash {
	if s.deletedAccounts[addr] {
		return common.Hash{}
	}
	if slots, ok := s.storage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	if s.storageCleared[addr] {
		return common.Hash{}
	}
	if s.overlayBase != nil {
		value, err := s.overlayBase.storageSnapshot(addr, key)
		if err != nil {
			s.recordDBError(err)
			return common.Hash{}
		}
		s.ensureMutableStorage(addr)
		if s.storage[addr] == nil {
			s.storage[addr] = make(map[common.Hash]common.Hash)
		}
		s.storage[addr][key] = value
		return value
	}
	if s.kv == nil {
		return common.Hash{}
	}

	data, err := s.kv.Get(storageKey(addr, key))
	if err != nil {
		s.recordDBError(fmt.Errorf("db get storage %s[%s]: %w", addr.Hex(), key.Hex(), err))
		return common.Hash{}
	}
	var val common.Hash
	if len(data) > 0 {
		val = common.BytesToHash(data)
	}
	s.ensureMutableStorage(addr)
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = val
	return val
}

func (s *PebbleStateDB) codeSnapshot(addr common.Address) ([]byte, error) {
	if s.deletedAccounts[addr] {
		return nil, nil
	}
	if code, ok := s.code[addr]; ok {
		return append([]byte(nil), code...), nil
	}
	if s.storageCleared[addr] {
		return nil, nil
	}
	if s.overlayBase != nil {
		return s.overlayBase.codeSnapshot(addr)
	}
	if s.kv == nil {
		return nil, nil
	}
	code, err := s.kv.Get(codeKey(addr))
	if err != nil {
		return nil, fmt.Errorf("db get code snapshot %s: %w", addr.Hex(), err)
	}
	return append([]byte(nil), code...), nil
}

func (s *PebbleStateDB) storageSnapshot(addr common.Address, key common.Hash) (common.Hash, error) {
	if s.deletedAccounts[addr] {
		return common.Hash{}, nil
	}
	if slots, ok := s.storage[addr]; ok {
		if value, found := slots[key]; found {
			return value, nil
		}
	}
	if s.storageCleared[addr] {
		return common.Hash{}, nil
	}
	if s.overlayBase != nil {
		return s.overlayBase.storageSnapshot(addr, key)
	}
	if s.kv == nil {
		return common.Hash{}, nil
	}
	data, err := s.kv.Get(storageKey(addr, key))
	if err != nil {
		return common.Hash{}, fmt.Errorf("db get storage snapshot %s[%s]: %w", addr.Hex(), key.Hex(), err)
	}
	if len(data) == 0 {
		return common.Hash{}, nil
	}
	return common.BytesToHash(data), nil
}

// GetCommittedState trả về giá trị ô lưu trữ tại lúc bắt đầu giao dịch hiện tại
// (EIP-2200). Đây là giá trị gốc dùng để tính gas SSTORE, không phải giá trị đầu khối.
func (s *PebbleStateDB) GetCommittedState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.originStorage[addr]; ok {
		if v, ok := slots[key]; ok {
			return v
		}
	}
	return s.getStorage(addr, key)
}

func (s *PebbleStateDB) GetStateAndCommittedState(addr common.Address, key common.Hash) (common.Hash, common.Hash) {
	return s.GetState(addr, key), s.GetCommittedState(addr, key)
}

func (s *PebbleStateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	return s.getStorage(addr, key)
}

func (s *PebbleStateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	_ = s.getOrCreateAccount(addr)
	prev := s.GetState(addr, key)

	// Ghi nhớ giá trị đầu giao dịch của ô lưu trữ (chỉ ghi nhớ lần ghi đầu tiên).
	if s.originStorage[addr] == nil {
		s.originStorage[addr] = make(map[common.Hash]common.Hash)
	}
	if _, ok := s.originStorage[addr][key]; !ok {
		s.originStorage[addr][key] = prev
	}

	prevHadSlot := false
	if slots, ok := s.storage[addr]; ok {
		_, prevHadSlot = slots[key]
	}
	prevDirtySlot := false
	if slots, ok := s.dirtyStorage[addr]; ok {
		prevDirtySlot = slots[key]
	}

	s.journal = append(s.journal, journalEntry{
		typ:           journalStorage,
		addr:          addr,
		key:           key,
		prevVal:       prev,
		prevHadSlot:   prevHadSlot,
		prevDirtySlot: prevDirtySlot,
		prevDirtyAcc:  s.dirtyAccounts[addr],
		prevTouched:   s.txTouched[addr],
	})

	s.ensureMutableStorage(addr)
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = value

	if s.dirtyStorage[addr] == nil {
		s.dirtyStorage[addr] = make(map[common.Hash]bool)
	}
	s.dirtyStorage[addr][key] = true

	s.markDirty(addr)
	return prev
}

// GetStorageRoot phân biệt storage rỗng (EmptyRootHash) với storage có slot != 0 để evm.create bắt collision đúng chuẩn Shanghai.
func (s *PebbleStateDB) GetStorageRoot(addr common.Address) common.Hash {
	if !s.Exist(addr) {
		return common.Hash{}
	}
	overridden := make(map[common.Hash]bool)
	cur := s
	for cur != nil {
		if cur.deletedAccounts[addr] {
			return types.EmptyRootHash
		}
		if slots, ok := cur.storage[addr]; ok {
			for slot, val := range slots {
				if overridden[slot] {
					continue
				}
				overridden[slot] = true
				if val != (common.Hash{}) {
					return nonEmptyStorageSentinel
				}
			}
		}
		if cur.storageCleared[addr] {
			return types.EmptyRootHash
		}
		cur = cur.overlayBase
	}
	if s.db != nil {
		prefix := storagePrefix(addr)
		iter, err := s.db.Iterator(prefix, prefixUpperBound(prefix))
		if err != nil {
			s.recordDBError(fmt.Errorf("db iter storage root %s: %w", addr.Hex(), err))
			return types.EmptyRootHash
		}
		defer iter.Close()
		for ; iter.Valid(); iter.Next() {
			k := iter.Key()
			if len(k) != len(prefix)+common.HashLength {
				continue
			}
			slot := common.BytesToHash(k[len(prefix):])
			if overridden[slot] {
				continue
			}
			if common.BytesToHash(iter.Value()) != (common.Hash{}) {
				return nonEmptyStorageSentinel
			}
		}
		if err := iter.Error(); err != nil {
			s.recordDBError(fmt.Errorf("db iter storage root err %s: %w", addr.Hex(), err))
		}
	}
	return types.EmptyRootHash
}

// --- Vùng lưu trữ tạm thời (EIP-1153) ---

func (s *PebbleStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.transientStorage[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (s *PebbleStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	s.journal = append(s.journal, journalEntry{
		typ:              journalTransient,
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
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil {
		return uint256.Int{}
	}
	bal := s.GetBalance(addr)

	s.journal = append(s.journal, journalEntry{
		typ:                journalSelfDestruct,
		addr:               addr,
		prevSelfDestructed: s.selfDestructed[addr],
		prevBalance:        new(big.Int).Set(acc.Balance),
		prevDirtyAcc:       s.dirtyAccounts[addr],
		prevTouched:        s.txTouched[addr],
	})

	s.selfDestructed[addr] = true
	acc.Balance = big.NewInt(0)
	s.markDirty(addr)
	return *bal
}

func (s *PebbleStateDB) HasSelfDestructed(addr common.Address) bool {
	return s.selfDestructed[addr]
}

func (s *PebbleStateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil {
		return uint256.Int{}, false
	}
	if s.newContracts[addr] {
		return s.SelfDestruct(addr), true
	}
	return *s.GetBalance(addr), false
}

// --- Sự tồn tại của tài khoản ---

func (s *PebbleStateDB) Exist(addr common.Address) bool {
	_, ok := s.loadAccount(addr)
	return ok
}

func (s *PebbleStateDB) Empty(addr common.Address) bool {
	acc, ok := s.loadAccount(addr)
	return !ok || isEmptyAccount(acc)
}

// --- Danh sách truy cập trạng thái (EIP-2929) ---

func (s *PebbleStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessList = newAccessList()

	s.accessList.addresses[sender] = true

	if rules.IsShanghai {
		s.accessList.addresses[coinbase] = true
	}

	if dest != nil {
		s.accessList.addresses[*dest] = true
	}

	for _, addr := range precompiles {
		s.accessList.addresses[addr] = true
	}

	for _, entry := range txAccesses {
		s.accessList.addresses[entry.Address] = true
		if s.accessList.slots[entry.Address] == nil {
			s.accessList.slots[entry.Address] = make(map[common.Hash]bool)
		}
		for _, key := range entry.StorageKeys {
			s.accessList.slots[entry.Address][key] = true
		}
	}

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
	if !s.accessList.addresses[addr] {
		s.accessList.addresses[addr] = true
		s.journal = append(s.journal, journalEntry{
			typ:  journalAccessListAddr,
			addr: addr,
		})
	}
}

func (s *PebbleStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	if !s.accessList.addresses[addr] {
		s.accessList.addresses[addr] = true
		s.journal = append(s.journal, journalEntry{
			typ:  journalAccessListAddr,
			addr: addr,
		})
	}
	if s.accessList.slots[addr] == nil {
		s.accessList.slots[addr] = make(map[common.Hash]bool)
	}
	if !s.accessList.slots[addr][slot] {
		s.accessList.slots[addr][slot] = true
		s.journal = append(s.journal, journalEntry{
			typ:  journalAccessListSlot,
			addr: addr,
			key:  slot,
		})
	}
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

	for i := len(s.journal) - 1; i >= snap.journalLen; i-- {
		entry := s.journal[i]
		switch entry.typ {
		case journalBalance:
			if acc := s.state[entry.addr]; acc != nil {
				acc.Balance = entry.prevBalance
			}
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalNonce:
			if acc := s.state[entry.addr]; acc != nil {
				acc.Nonce = entry.prevNonce
			}
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalStorage:
			s.ensureMutableStorage(entry.addr)
			if entry.prevHadSlot {
				if s.storage[entry.addr] == nil {
					s.storage[entry.addr] = make(map[common.Hash]common.Hash)
				}
				s.storage[entry.addr][entry.key] = entry.prevVal
			} else if s.storage[entry.addr] != nil {
				delete(s.storage[entry.addr], entry.key)
			}
			if entry.prevDirtySlot {
				if s.dirtyStorage[entry.addr] == nil {
					s.dirtyStorage[entry.addr] = make(map[common.Hash]bool)
				}
				s.dirtyStorage[entry.addr][entry.key] = true
			} else if s.dirtyStorage[entry.addr] != nil {
				delete(s.dirtyStorage[entry.addr], entry.key)
				if len(s.dirtyStorage[entry.addr]) == 0 {
					delete(s.dirtyStorage, entry.addr)
				}
			}
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalCode:
			if acc := s.state[entry.addr]; acc != nil {
				acc.CodeHash = entry.prevCodeHash
			}
			if entry.prevHadCode {
				s.code[entry.addr] = entry.prevCode
			} else {
				delete(s.code, entry.addr)
			}
			restoreBoolMap(s.dirtyCode, entry.addr, entry.prevDirtyCode)
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalSelfDestruct:
			restoreBoolMap(s.selfDestructed, entry.addr, entry.prevSelfDestructed)
			if acc := s.state[entry.addr]; acc != nil {
				acc.Balance = entry.prevBalance
			}
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalTransient:
			if s.transientStorage[entry.addr] == nil {
				s.transientStorage[entry.addr] = make(map[common.Hash]common.Hash)
			}
			s.transientStorage[entry.addr][entry.key] = entry.prevTransientVal
		case journalAccountCreate:
			if entry.prevExisted && entry.prevAccount != nil {
				s.state[entry.addr] = entry.prevAccount
			} else {
				delete(s.state, entry.addr)
			}
			restoreBoolMap(s.missingAccounts, entry.addr, entry.prevMissing)
			restoreBoolMap(s.deletedAccounts, entry.addr, entry.prevDeleted)
			restoreBoolMap(s.dirtyAccounts, entry.addr, entry.prevDirtyAcc)
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalTouch:
			restoreBoolMap(s.txTouched, entry.addr, entry.prevTouched)
		case journalAccessListAddr:
			delete(s.accessList.addresses, entry.addr)
			delete(s.accessList.slots, entry.addr)
		case journalAccessListSlot:
			if slots := s.accessList.slots[entry.addr]; slots != nil {
				delete(slots, entry.key)
				if len(slots) == 0 {
					delete(s.accessList.slots, entry.addr)
				}
			}
		case journalNewContract:
			restoreBoolMap(s.newContracts, entry.addr, entry.prevNewContract)
		}
	}

	s.journal = s.journal[:snap.journalLen]
	s.snapshots = s.snapshots[:idx]
	s.refund = snap.refund
	s.logs = s.logs[:snap.logsLen]
}

func restoreBoolMap(m map[common.Address]bool, addr common.Address, val bool) {
	if val {
		m[addr] = true
	} else {
		delete(m, addr)
	}
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
	for addr, destructed := range s.selfDestructed {
		if !destructed {
			continue
		}
		delete(s.state, addr)
		delete(s.storage, addr)
		delete(s.dirtyStorage, addr)
		delete(s.frozenStorage, addr)
		delete(s.code, addr)
		delete(s.dirtyCode, addr)
		s.storageCleared[addr] = true
		if s.existedInBase(addr) {
			s.deletedAccounts[addr] = true
			s.dirtyAccounts[addr] = true
		} else {
			delete(s.deletedAccounts, addr)
			delete(s.dirtyAccounts, addr)
			s.missingAccounts[addr] = true
		}
	}

	// EIP-161 (Shanghai): xóa mọi tài khoản rỗng đã được chạm/tạo/sửa trong giao dịch
	if deleteEmptyObjects {
		for addr := range s.txTouched {
			if s.deletedAccounts[addr] {
				continue
			}
			acc, ok := s.state[addr]
			if !ok {
				continue
			}
			if !isEmptyAccount(acc) {
				continue
			}
			delete(s.state, addr)
			delete(s.code, addr)
			delete(s.dirtyCode, addr)
			delete(s.storage, addr)
			delete(s.dirtyStorage, addr)
			delete(s.frozenStorage, addr)
			s.storageCleared[addr] = true
			if s.existedInBase(addr) {
				s.deletedAccounts[addr] = true
				s.dirtyAccounts[addr] = true
			} else {
				delete(s.deletedAccounts, addr)
				delete(s.dirtyAccounts, addr)
				s.missingAccounts[addr] = true
			}
		}
	}

	// Kết thúc giao dịch: xóa nhật ký, ảnh chụp, phần hoàn gas và giá trị gốc để
	// giao dịch tiếp theo bắt đầu với trạng thái sạch.
	s.originStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.selfDestructed = make(map[common.Address]bool)
	s.newContracts = make(map[common.Address]bool)
	s.txTouched = make(map[common.Address]bool)
	s.journal = s.journal[:0]
	s.snapshots = s.snapshots[:0]
	s.refund = 0
}

// Commit ghi toàn bộ trạng thái đã thay đổi xuống PebbleDB.
func (s *PebbleStateDB) Commit() error {
	if err := s.Err(); err != nil {
		return fmt.Errorf("state db read error before commit: %w", err)
	}
	writes, err := s.PendingWrites()
	if err != nil {
		return err
	}
	batch := s.db.NewBatch()
	defer batch.Close()
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
	acc, ok := s.loadAccount(addr)
	if !ok || acc == nil {
		return &Account{Balance: new(big.Int)}, false
	}
	return cloneAccount(acc), true
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
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("state db read error: %w", err)
	}
	writes := make(map[string][]byte)
	// emit tombstone cho code/storage cũ của account bị SELFDESTRUCT rồi tái tạo cùng block (tránh hồi sinh ở block H+1)
	for addr, cleared := range s.storageCleared {
		if !cleared || s.deletedAccounts[addr] {
			continue
		}
		hasBaseCode := false
		if s.overlayBase != nil {
			c, err := s.overlayBase.codeSnapshot(addr)
			if err != nil {
				return nil, fmt.Errorf("code snapshot for cleared account %s: %w", addr.Hex(), err)
			}
			hasBaseCode = len(c) > 0
		} else if s.kv != nil {
			has, err := s.kv.Has(codeKey(addr))
			if err != nil {
				return nil, fmt.Errorf("has code for cleared account %s: %w", addr.Hex(), err)
			}
			hasBaseCode = has
		}
		if hasBaseCode {
			writes[string(codeKey(addr))] = nil
		}
		if s.overlayBase != nil {
			for cur := s.overlayBase; cur != nil; cur = cur.overlayBase {
				for slot := range cur.storage[addr] {
					writes[string(storageKey(addr, slot))] = nil
				}
			}
		}
		if s.kv != nil {
			prefix := storagePrefix(addr)
			iter, err := s.kv.Iterator(prefix, prefixUpperBound(prefix))
			if err != nil {
				return nil, fmt.Errorf("iter storage for cleared account %s: %w", addr.Hex(), err)
			}
			for ; iter.Valid(); iter.Next() {
				writes[string(iter.Key())] = nil
			}
			iterErr := iter.Error()
			_ = iter.Close()
			if iterErr != nil {
				return nil, fmt.Errorf("iter storage error for cleared account %s: %w", addr.Hex(), iterErr)
			}
		}
	}
	for addr := range s.dirtyAccounts {
		if s.deletedAccounts[addr] {
			writes[string(append([]byte("acc-"), addr.Bytes()...))] = nil
			writes[string(codeKey(addr))] = nil
			// xóa sạch mọi slot storage cũ của tài khoản bị xóa để không còn khóa mồ côi trong Pebble
			if s.overlayBase != nil {
				for cur := s.overlayBase; cur != nil; cur = cur.overlayBase {
					for slot := range cur.storage[addr] {
						writes[string(storageKey(addr, slot))] = nil
					}
				}
			}
			if s.kv != nil {
				prefix := storagePrefix(addr)
				iter, err := s.kv.Iterator(prefix, prefixUpperBound(prefix))
				if err != nil {
					return nil, fmt.Errorf("iter storage for deleted account %s: %w", addr.Hex(), err)
				}
				for ; iter.Valid(); iter.Next() {
					writes[string(iter.Key())] = nil
				}
				iterErr := iter.Error()
				_ = iter.Close()
				if iterErr != nil {
					return nil, fmt.Errorf("iter storage error for deleted account %s: %w", addr.Hex(), iterErr)
				}
			}
			continue
		}
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
		if s.deletedAccounts[addr] {
			continue
		}
		if len(s.code[addr]) == 0 {
			writes[string(codeKey(addr))] = nil
		} else {
			writes[string(codeKey(addr))] = append([]byte(nil), s.code[addr]...)
		}
	}
	for addr, slots := range s.dirtyStorage {
		if s.deletedAccounts[addr] {
			continue
		}
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
		if s.deletedAccounts[addr] {
			k := "account:" + string(addr.Bytes())
			keys = append(keys, k)
			// giá trị thiếu (nil) biểu thị thao tác xóa tài khoản
			continue
		}
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
		if s.deletedAccounts[addr] {
			continue
		}
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
		if s.deletedAccounts[addr] {
			continue
		}
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

// AddBalanceBig thêm số dư khởi tạo ở khối khởi nguyên.
func (s *PebbleStateDB) AddBalanceBig(addr common.Address, amount *big.Int) {
	acc := s.getOrCreateAccount(addr)
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
	sender := s.getOrCreateAccount(from)
	if sender.Nonce != nonce {
		return fmt.Errorf("EOA transfer nonce changed: state %d, tx %d", sender.Nonce, nonce)
	}
	amount := value.ToBig()
	if sender.Balance.Cmp(amount) < 0 {
		return fmt.Errorf("EOA transfer balance changed")
	}
	if from != to && amount.Sign() > 0 {
		sender.Balance.Sub(sender.Balance, amount)
		receiver := s.getOrCreateAccount(to)
		receiver.Balance.Add(receiver.Balance, amount)
		s.markDirty(to)
	}
	sender.Nonce++
	s.markDirty(from)
	// khớp EIP-161 khi tip=0: nếu coinbase tồn tại nhưng rỗng -> xóa; nếu không tồn tại hoặc không rỗng -> giữ nguyên
	if cbAcc, ok := s.loadAccount(coinbase); ok && isEmptyAccount(cbAcc) {
		delete(s.state, coinbase)
		delete(s.code, coinbase)
		delete(s.dirtyCode, coinbase)
		if s.existedInBase(coinbase) {
			s.deletedAccounts[coinbase] = true
			s.dirtyAccounts[coinbase] = true
		} else {
			delete(s.dirtyAccounts, coinbase)
			s.missingAccounts[coinbase] = true
		}
	}
	return nil
}
