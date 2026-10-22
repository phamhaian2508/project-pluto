package evm

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// CheckpointPolicy identifies when the parallel executor captures an in-memory
// execution checkpoint. Selection uses only opcode positions, state-read
// events, and ordered conflict marks; wall-clock time is never consulted.
type CheckpointPolicy uint8

const (
	CheckpointPolicyP0 CheckpointPolicy = iota // never checkpoint
	CheckpointPolicyP1                         // before every state read
	CheckpointPolicyP2                         // every configured opcode interval
	CheckpointPolicyP3                         // before reads of conflict-hot keys
)

// CheckpointConfig configures deterministic checkpoint selection. Interval is
// required only by P2 and denotes the number of executed opcodes between
// checkpoints.
type CheckpointConfig struct {
	Policy   CheckpointPolicy
	Interval uint64
}

func DefaultCheckpointConfig() CheckpointConfig {
	return CheckpointConfig{Policy: CheckpointPolicyP0}
}

func (c CheckpointConfig) Validate() error {
	if c.Policy > CheckpointPolicyP3 {
		return errors.New("unknown checkpoint policy")
	}
	if c.Policy == CheckpointPolicyP2 && c.Interval == 0 {
		return errors.New("P2 checkpoint interval must be greater than zero")
	}
	return nil
}

type CheckpointReason uint8

const (
	CheckpointBeforeStateRead CheckpointReason = iota
	CheckpointOpcodeInterval
	CheckpointBeforeHotRead
)

// CheckpointDecision identifies a boundary immediately before the event at
// OpcodePosition. OpcodePosition is the deterministic count of opcodes already
// executed in the transaction, starting at zero.
type CheckpointDecision struct {
	OpcodePosition uint64
	Reason         CheckpointReason
	ReadKey        MVKey
}

// CheckpointSelector is execution-local. Conflict keys must be marked in
// canonical validation order, making P3 independent of goroutine scheduling.
type CheckpointSelector struct {
	config  CheckpointConfig
	hotKeys map[MVKey]struct{}
}

func newCheckpointSelector(config CheckpointConfig) (*CheckpointSelector, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &CheckpointSelector{config: config, hotKeys: make(map[MVKey]struct{})}, nil
}

// BeforeOpcode selects P2 checkpoints after exactly N, 2N, ... opcodes have
// executed. Position zero is the initial transaction boundary, not a P2 point.
func (s *CheckpointSelector) BeforeOpcode(position uint64) (CheckpointDecision, bool) {
	if s == nil || s.config.Policy != CheckpointPolicyP2 || position == 0 || position%s.config.Interval != 0 {
		return CheckpointDecision{}, false
	}
	return CheckpointDecision{OpcodePosition: position, Reason: CheckpointOpcodeInterval}, true
}

// BeforeStateRead selects P1 checkpoints for every read and P3 checkpoints
// only for keys previously marked hot by an ordered validation conflict.
func (s *CheckpointSelector) BeforeStateRead(position uint64, key MVKey) (CheckpointDecision, bool) {
	if s == nil {
		return CheckpointDecision{}, false
	}
	switch s.config.Policy {
	case CheckpointPolicyP1:
		return CheckpointDecision{OpcodePosition: position, Reason: CheckpointBeforeStateRead, ReadKey: key}, true
	case CheckpointPolicyP3:
		if _, hot := s.hotKeys[key]; hot {
			return CheckpointDecision{OpcodePosition: position, Reason: CheckpointBeforeHotRead, ReadKey: key}, true
		}
	}
	return CheckpointDecision{}, false
}

// MarkReadConflict deterministically promotes a key to hot. The caller must
// feed conflicts in canonical transaction-validation order.
func (s *CheckpointSelector) MarkReadConflict(key MVKey) {
	if s != nil && s.config.Policy == CheckpointPolicyP3 {
		s.hotKeys[key] = struct{}{}
	}
}

// InterpreterState is the geth-fork-neutral state needed to continue between
// opcodes. The production geth adapter will populate it in a later task.
type InterpreterState struct {
	PC                uint64
	Stack             []uint256.Int
	Memory            []byte
	MemoryLastGasCost uint64
	ReturnData        []byte
	Gas               uint64
	CallGasTemp       uint64
	CallFrames        []CallFrameState
}

// CallFrameState owns the execution context and mutable machine state of one
// suspended EVM call frame.
type CallFrameState struct {
	PC          uint64
	Stack       []uint256.Int
	Memory      []byte
	ReturnData  []byte
	Gas         uint64
	Depth       int
	CallType    byte
	Caller      common.Address
	Address     common.Address
	CodeAddress common.Address
	CodeHash    common.Hash
	Code        []byte
	Input       []byte
	Value       uint256.Int
	ReadOnly    bool
	Deployment  bool
	SystemCall  bool
}

// ExecutionCheckpoint is an immutable, RAM-only snapshot. It deliberately has
// no serialization or database API.
type ExecutionCheckpoint struct {
	interpreter InterpreterState
	state       *PebbleStateDB
	txIndex     int
	incarnation uint64
	readSet     []MVKey
	writeSet    []MVKey
	reads       map[MVKey]struct{}
	writes      map[MVKey]struct{}
}

// TakeExecutionCheckpoint manually captures a complete checkpoint. Automatic
// policies call no such function while P0 is active.
func TakeExecutionCheckpoint(interpreter *InterpreterState, state *TxState) (*ExecutionCheckpoint, error) {
	if interpreter == nil || state == nil || state.PebbleStateDB == nil {
		return nil, errors.New("checkpoint requires interpreter and transaction state")
	}
	return &ExecutionCheckpoint{
		interpreter: cloneInterpreterState(*interpreter),
		state:       cloneStateForCheckpoint(state.PebbleStateDB),
		txIndex:     state.TxIndex,
		incarnation: state.Incarnation,
		readSet:     append([]MVKey(nil), state.ReadSet...),
		writeSet:    append([]MVKey(nil), state.WriteSet...),
		reads:       cloneMVKeySet(state.reads),
		writes:      cloneMVKeySet(state.writes),
	}, nil
}

// Restore replaces all interpreter, overlay, access-set, and incarnation state
// with private copies of the captured values, allowing a checkpoint to be used
// more than once without sharing mutable memory.
func (c *ExecutionCheckpoint) Restore(interpreter *InterpreterState, state *TxState) error {
	if c == nil || interpreter == nil || state == nil {
		return errors.New("restore requires checkpoint, interpreter and transaction state")
	}
	*interpreter = cloneInterpreterState(c.interpreter)
	state.PebbleStateDB = cloneStateForCheckpoint(c.state)
	state.TxIndex = c.txIndex
	state.Incarnation = c.incarnation
	state.ReadSet = append([]MVKey(nil), c.readSet...)
	state.WriteSet = append([]MVKey(nil), c.writeSet...)
	state.reads = cloneMVKeySet(c.reads)
	state.writes = cloneMVKeySet(c.writes)
	return nil
}

// EstimatedMemoryBytes reports the deterministic payload owned by a checkpoint.
// It intentionally excludes runtime map/slice allocator overhead, which varies
// between Go versions and would make benchmark comparisons unstable.
func (c *ExecutionCheckpoint) EstimatedMemoryBytes() uint64 {
	if c == nil {
		return 0
	}
	size := interpreterPayloadBytes(c.interpreter)
	size += uint64(len(c.readSet)+len(c.writeSet)+len(c.reads)+len(c.writes)) * 53 // address + kind + slot
	if c.state == nil {
		return size
	}
	for _, account := range c.state.state {
		size += 20
		if account != nil {
			size += 8 + uint64(len(account.CodeHash))
			if account.Balance != nil {
				size += uint64(len(account.Balance.Bytes()))
			}
		}
	}
	for _, code := range c.state.code {
		size += 20 + uint64(len(code))
	}
	size += storagePayloadBytes(c.state.storage)
	size += storagePayloadBytes(c.state.transientStorage)
	size += storagePayloadBytes(c.state.originStorage)
	for _, log := range c.state.logs {
		if log != nil {
			size += 20 + uint64(len(log.Topics))*32 + uint64(len(log.Data))
		}
	}
	for _, entry := range c.state.journal {
		size += 20 + 8 + 32 + 32 + uint64(len(entry.prevCode)+len(entry.prevCodeHash))
		if entry.prevBalance != nil {
			size += uint64(len(entry.prevBalance.Bytes()))
		}
	}
	size += uint64(len(c.state.snapshots)) * 32
	if c.state.accessList != nil {
		size += uint64(len(c.state.accessList.addresses)) * 20
		for _, slots := range c.state.accessList.slots {
			size += 20 + uint64(len(slots))*32
		}
	}
	return size
}

func interpreterPayloadBytes(state InterpreterState) uint64 {
	size := uint64(16 + len(state.Stack)*32 + len(state.Memory) + len(state.ReturnData))
	for _, frame := range state.CallFrames {
		size += uint64(16 + len(frame.Stack)*32 + len(frame.Memory) + len(frame.ReturnData))
		size += 20*3 + uint64(len(frame.Code)+len(frame.Input)) + 32
	}
	return size
}

func storagePayloadBytes(storage map[common.Address]map[common.Hash]common.Hash) uint64 {
	var size uint64
	for _, slots := range storage {
		size += 20 + uint64(len(slots))*64
	}
	return size
}

func cloneInterpreterState(src InterpreterState) InterpreterState {
	dst := InterpreterState{
		PC: src.PC, Stack: append([]uint256.Int(nil), src.Stack...),
		Memory: append([]byte(nil), src.Memory...), MemoryLastGasCost: src.MemoryLastGasCost,
		ReturnData: append([]byte(nil), src.ReturnData...), Gas: src.Gas, CallGasTemp: src.CallGasTemp,
		CallFrames: make([]CallFrameState, len(src.CallFrames)),
	}
	for i, frame := range src.CallFrames {
		dst.CallFrames[i] = frame
		dst.CallFrames[i].Stack = append([]uint256.Int(nil), frame.Stack...)
		dst.CallFrames[i].Memory = append([]byte(nil), frame.Memory...)
		dst.CallFrames[i].ReturnData = append([]byte(nil), frame.ReturnData...)
		dst.CallFrames[i].Code = append([]byte(nil), frame.Code...)
		dst.CallFrames[i].Input = append([]byte(nil), frame.Input...)
	}
	return dst
}

func cloneStateForCheckpoint(src *PebbleStateDB) *PebbleStateDB {
	dst := NewPebbleStateDB(src.db)
	dst.overlayBase = src.overlayBase
	for addr, account := range src.state {
		if account == nil {
			dst.state[addr] = nil
			continue
		}
		clone := *account
		if account.Balance != nil {
			clone.Balance = new(big.Int).Set(account.Balance)
		}
		clone.CodeHash = append([]byte(nil), account.CodeHash...)
		dst.state[addr] = &clone
	}
	for addr, code := range src.code {
		dst.code[addr] = append([]byte(nil), code...)
	}
	dst.dirtyCode = cloneAddressBoolMap(src.dirtyCode)
	dst.storage = cloneAddressStorage(src.storage)
	dst.dirtyStorage = cloneAddressBoolStorage(src.dirtyStorage)
	dst.selfDestructed = cloneAddressBoolMap(src.selfDestructed)
	dst.transientStorage = cloneAddressStorage(src.transientStorage)
	dst.refund = src.refund
	dst.logs = cloneLogs(src.logs)
	dst.journal = cloneJournal(src.journal)
	dst.snapshots = append([]snapshot(nil), src.snapshots...)
	dst.nextRevID = src.nextRevID
	dst.accessList = cloneAccessList(src.accessList)
	dst.dirtyAccounts = cloneAddressBoolMap(src.dirtyAccounts)
	dst.originStorage = cloneAddressStorage(src.originStorage)
	return dst
}

func cloneJournal(src []journalEntry) []journalEntry {
	dst := make([]journalEntry, len(src))
	for i, entry := range src {
		dst[i] = entry
		if entry.prevBalance != nil {
			dst[i].prevBalance = new(big.Int).Set(entry.prevBalance)
		}
		dst[i].prevCode = append([]byte(nil), entry.prevCode...)
		dst[i].prevCodeHash = append([]byte(nil), entry.prevCodeHash...)
	}
	return dst
}

func cloneAccessList(src *accessList) *accessList {
	if src == nil {
		return nil
	}
	dst := newAccessList()
	dst.addresses = cloneAddressBoolMap(src.addresses)
	dst.slots = cloneAddressBoolStorage(src.slots)
	return dst
}

func cloneAddressStorage(src map[common.Address]map[common.Hash]common.Hash) map[common.Address]map[common.Hash]common.Hash {
	dst := make(map[common.Address]map[common.Hash]common.Hash, len(src))
	for addr, slots := range src {
		dst[addr] = cloneHashMap(slots)
	}
	return dst
}

func cloneAddressBoolStorage(src map[common.Address]map[common.Hash]bool) map[common.Address]map[common.Hash]bool {
	dst := make(map[common.Address]map[common.Hash]bool, len(src))
	for addr, slots := range src {
		cloned := make(map[common.Hash]bool, len(slots))
		for key, value := range slots {
			cloned[key] = value
		}
		dst[addr] = cloned
	}
	return dst
}

func cloneAddressBoolMap(src map[common.Address]bool) map[common.Address]bool {
	dst := make(map[common.Address]bool, len(src))
	for addr, value := range src {
		dst[addr] = value
	}
	return dst
}

func cloneMVKeySet(src map[MVKey]struct{}) map[MVKey]struct{} {
	dst := make(map[MVKey]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}
