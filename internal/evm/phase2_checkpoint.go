package evm

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// seam kiểm thử cho phép ép hủy resume ở suffix để kiểm chứng nhánh fallback FullReplay.
var runStrictResumedGethFrame = resumeGethFrameStrict

// purePrefixOpcodeTable thay thế switch dài bằng bảng tra O(1) 256 byte cho các opcode thuần tính toán Shanghai.
var purePrefixOpcodeTable = func() [256]bool {
	var t [256]bool
	pureOps := []vm.OpCode{
		vm.STOP,
		vm.ADD, vm.MUL, vm.SUB, vm.DIV, vm.SDIV, vm.MOD, vm.SMOD,
		vm.ADDMOD, vm.MULMOD, vm.EXP, vm.SIGNEXTEND,
		vm.LT, vm.GT, vm.SLT, vm.SGT, vm.EQ, vm.ISZERO,
		vm.AND, vm.OR, vm.XOR, vm.NOT, vm.BYTE, vm.SHL, vm.SHR, vm.SAR,
		vm.KECCAK256,
		vm.ADDRESS, vm.ORIGIN, vm.CALLER, vm.CALLVALUE,
		vm.CALLDATALOAD, vm.CALLDATASIZE, vm.CALLDATACOPY,
		vm.CODESIZE, vm.CODECOPY, vm.GASPRICE,
		vm.RETURNDATASIZE, vm.RETURNDATACOPY,
		vm.BLOCKHASH, vm.COINBASE, vm.TIMESTAMP, vm.NUMBER,
		vm.DIFFICULTY, vm.GASLIMIT, vm.CHAINID, vm.BASEFEE,
		vm.POP, vm.MLOAD, vm.MSTORE, vm.MSTORE8,
		vm.JUMP, vm.JUMPI, vm.PC, vm.MSIZE, vm.GAS, vm.JUMPDEST,
		vm.PUSH0,
		vm.PUSH1, vm.PUSH2, vm.PUSH3, vm.PUSH4, vm.PUSH5, vm.PUSH6, vm.PUSH7, vm.PUSH8,
		vm.PUSH9, vm.PUSH10, vm.PUSH11, vm.PUSH12, vm.PUSH13, vm.PUSH14, vm.PUSH15, vm.PUSH16,
		vm.PUSH17, vm.PUSH18, vm.PUSH19, vm.PUSH20, vm.PUSH21, vm.PUSH22, vm.PUSH23, vm.PUSH24,
		vm.PUSH25, vm.PUSH26, vm.PUSH27, vm.PUSH28, vm.PUSH29, vm.PUSH30, vm.PUSH31, vm.PUSH32,
		vm.DUP1, vm.DUP2, vm.DUP3, vm.DUP4, vm.DUP5, vm.DUP6, vm.DUP7, vm.DUP8,
		vm.DUP9, vm.DUP10, vm.DUP11, vm.DUP12, vm.DUP13, vm.DUP14, vm.DUP15, vm.DUP16,
		vm.SWAP1, vm.SWAP2, vm.SWAP3, vm.SWAP4, vm.SWAP5, vm.SWAP6, vm.SWAP7, vm.SWAP8,
		vm.SWAP9, vm.SWAP10, vm.SWAP11, vm.SWAP12, vm.SWAP13, vm.SWAP14, vm.SWAP15, vm.SWAP16,
		vm.RETURN, vm.REVERT, vm.INVALID,
	}
	for _, op := range pureOps {
		t[byte(op)] = true
	}
	return t
}()

// isPurePrefixOpcode trả true khi opcode Shanghai chỉ thao tác trên stack, memory,
// calldata, bytecode hằng hoặc ngữ cảnh block/call bất biến đã xác thực — tuyệt đối
// không đọc/ghi state, không làm nóng slot/addr, không đổi refund, không phát LOG và
// không tạo subcall/create/selfdestruct.
func isPurePrefixOpcode(op vm.OpCode) bool {
	return purePrefixOpcodeTable[byte(op)]
}

type pendingPhase2Capture struct {
	resID         uint64
	opcode        byte
	step          uint64
	cp            *PurePrefixCheckpoint
	decisionEvent *AdaptiveDecisionEvent
}

// phase2CheckpointCapture điều khiển việc chụp checkpoint thuần prefix trong frame gốc
// cho lần thử đầu tiên của một giao dịch trên OCC / predicted-occ.
type phase2CheckpointCapture struct {
	policyName              string // "off" | "fixed" | "history" | "adaptive"
	effectivePolicy         string // "off" | "fixed" | "history" | "adaptive"
	interval                uint64
	hotSlots                map[MVKey]struct{}
	historyMaxPerTx         int
	observeAdaptive         bool // true khi chạy adaptive (kể cả block 1 bootstrap) để học (seen, useful)
	calibrateFirstState     bool // true trong lệnh calibrate để chụp đúng tại lần truy cập state đầu tiên
	adaptiveHistory         *AdaptiveHistorySnapshot
	adaptiveModel           *AdaptiveModel
	store                   *CheckpointStore
	state                   *TxState
	prefix                  *prefixSnapshot
	evm                     *vm.EVM
	blockNumber             uint64
	txIndex                 int
	txHash                  common.Hash
	attempt                 uint64
	recordTrace             bool

	// ngữ cảnh frame gốc được ghi nhận khi vào OnEnter(depth=0)
	rootEntered          bool
	caller               common.Address
	callee               common.Address
	codeHash             common.Hash
	selector             [4]byte
	initialFrameGas      uint64
	inputHash            common.Hash
	inputLen             int
	rootEnterReadCount   int
	rootEnterWriteCount  int
	rootEnterAccessAddrs int
	rootEnterAccessSlots int
	rootEnterLogs        int
	rootEnterRefund      uint64

	// trạng thái vòng lặp opcode ở frame gốc
	position              uint64
	expectedPC            uint64
	disarmed              bool
	disarmAfterStep       bool
	sawDynamicGasStep     bool
	sawUnsupportedOpcode  bool
	historyCapturedCount  int
	adaptiveCapturedCount int
	observedCandidate     *AdaptiveCandidateObservation
	calibratedCP          *PurePrefixCheckpoint
	pending               *pendingPhase2Capture
	err                   error

	// số liệu quan sát cục bộ cho lần chạy thử này (không chia sẻ khóa giữa worker)
	opcodeSteps            uint64
	captureRequested       uint64
	captureCreated         uint64
	captureSkippedBudget   uint64
	captureSkippedLowScore uint64
	adaptiveCandidates     uint64
	adaptiveCalibHits      uint64
	adaptiveCalibMisses    uint64
	adaptiveHistoryMisses  uint64
	captureNs              uint64
	scoreNs                uint64
	traceEvents            []Phase2TraceEvent
	decisionEvents         []AdaptiveDecisionEvent
}

func countAccessListSlots(al *accessList) int {
	if al == nil {
		return 0
	}
	total := 0
	for _, slots := range al.slots {
		total += len(slots)
	}
	return total
}

func (c *phase2CheckpointCapture) canCapturePolicy() bool {
	if c == nil || c.store == nil || c.store.BudgetBytes() == 0 {
		return false
	}
	return c.effectivePolicy == "fixed" || c.effectivePolicy == "history" || c.effectivePolicy == "adaptive" || c.calibrateFirstState
}

func (c *phase2CheckpointCapture) onEnter(depth int, typ byte, from, to common.Address, input []byte, gas uint64, value *big.Int) {
	if c == nil {
		return
	}
	if depth > 0 {
		// mọi lời gọi con (CALL/STATICCALL/DELEGATECALL/CREATE...) lập tức tắt chụp và đánh dấu ngoài phạm vi hỗ trợ
		c.disarmed = true
		c.sawUnsupportedOpcode = true
		return
	}
	c.caller = from
	c.callee = to
	c.initialFrameGas = gas
	c.inputLen = len(input)
	c.inputHash = crypto.Keccak256Hash(input)
	c.selector = ExtractSelector(input)
	if typ != byte(vm.CALL) || (value != nil && value.Sign() != 0) {
		c.disarmed = true
		c.sawUnsupportedOpcode = true
		return
	}
	if c.state == nil || len(c.state.PebbleStateDB.Code(to)) == 0 {
		c.disarmed = true
	}
}

func (c *phase2CheckpointCapture) onExit(depth int) {
	if c == nil || c.state == nil {
		return
	}
	if depth == 0 {
		// sau khi thoát frame gốc, mọi đọc hậu xử lý (coinbase/refund) thuộc bước >= totalRootSteps
		c.state.SetCurrentStep(int64(c.position))
	}
}

func (c *phase2CheckpointCapture) initRootBaseline(contract *vm.Contract) {
	if c.rootEntered {
		return
	}
	c.rootEntered = true
	if contract != nil {
		c.codeHash = contract.CodeHash
		if contract.Value() != nil && !contract.Value().IsZero() {
			c.disarmed = true
			c.sawUnsupportedOpcode = true
		}
		if contract.IsDeployment || contract.IsSystemCall || len(contract.Code) == 0 {
			c.disarmed = true
			c.sawUnsupportedOpcode = true
		}
	}
	if c.state != nil {
		c.rootEnterReadCount = len(c.state.ReadSet)
		c.rootEnterWriteCount = len(c.state.WriteSet)
		c.rootEnterLogs = len(c.state.GetLogs())
		c.rootEnterRefund = c.state.GetRefund()
		if c.state.accessList != nil {
			c.rootEnterAccessAddrs = len(c.state.accessList.addresses)
			c.rootEnterAccessSlots = countAccessListSlots(c.state.accessList)
		}
	}
}

// isPrefixStatePure xác minh bằng bất biến trên TxState rằng từ lúc vào frame gốc đến ranh giới
// hiện tại chưa phát sinh bất kỳ đọc/ghi state, làm nóng slot/addr, thay đổi refund hay phát LOG nào.
func (c *phase2CheckpointCapture) isPrefixStatePure() bool {
	if c.disarmed || !c.rootEntered || c.state == nil {
		return false
	}
	if len(c.state.ReadSet) != c.rootEnterReadCount || len(c.state.WriteSet) != c.rootEnterWriteCount {
		c.disarmed = true
		return false
	}
	if len(c.state.GetLogs()) != c.rootEnterLogs || c.state.GetRefund() != c.rootEnterRefund {
		c.disarmed = true
		return false
	}
	if c.state.accessList != nil {
		if len(c.state.accessList.addresses) != c.rootEnterAccessAddrs || countAccessListSlots(c.state.accessList) != c.rootEnterAccessSlots {
			c.disarmed = true
			return false
		}
	}
	return true
}

func (c *phase2CheckpointCapture) advanceExpectedPC(pc uint64, op vm.OpCode, stack *vm.Stack) {
	if op >= vm.PUSH1 && op <= vm.PUSH32 {
		c.expectedPC = pc + 1 + uint64(op-vm.PUSH1+1)
		return
	}
	switch op {
	case vm.JUMP:
		if stack != nil {
			data := stack.Data()
			if len(data) >= 1 {
				c.expectedPC = data[len(data)-1].Uint64()
				return
			}
		}
	case vm.JUMPI:
		if stack != nil {
			data := stack.Data()
			if len(data) >= 2 && !data[len(data)-2].IsZero() {
				c.expectedPC = data[len(data)-1].Uint64()
				return
			}
		}
	}
	c.expectedPC = pc + 1
}

func (c *phase2CheckpointCapture) shouldCaptureFixedOrHistory(step uint64, op vm.OpCode, contract *vm.Contract, stack *vm.Stack) (reason string, triggerSlot *MVKey, ok bool) {
	if !c.canCapturePolicy() {
		return "", nil, false
	}
	if c.calibrateFirstState {
		if op != vm.SLOAD && op != vm.SSTORE {
			return "", nil, false
		}
		if contract == nil || stack == nil {
			return "", nil, false
		}
		data := stack.Data()
		if len(data) == 0 {
			return "", nil, false
		}
		top := data[len(data)-1]
		k := MVKey{Address: contract.Address(), Kind: MVStorage, Slot: common.Hash(top.Bytes32())}
		return "calibrate", &k, true
	}
	switch c.effectivePolicy {
	case "fixed":
		if c.interval == 0 {
			return "", nil, false
		}
		if step%c.interval == 0 {
			return "fixed", nil, true
		}
		return "", nil, false
	case "history":
		if c.historyMaxPerTx > 0 && c.historyCapturedCount >= c.historyMaxPerTx {
			return "", nil, false
		}
		if len(c.hotSlots) == 0 {
			return "", nil, false
		}
		if op != vm.SLOAD && op != vm.SSTORE {
			return "", nil, false
		}
		if contract == nil || stack == nil {
			return "", nil, false
		}
		data := stack.Data()
		if len(data) == 0 {
			return "", nil, false
		}
		// đọc slot trên đỉnh stack mà không làm thay đổi stack hay EVM
		top := data[len(data)-1]
		key := MVKey{Address: contract.Address(), Kind: MVStorage, Slot: common.Hash(top.Bytes32())}
		if _, hot := c.hotSlots[key]; hot {
			k := key
			return "history", &k, true
		}
		return "", nil, false
	default:
		return "", nil, false
	}
}

func (c *phase2CheckpointCapture) evaluateAdaptiveAndCapture(
	step uint64,
	opcode byte,
	evmEnv *vm.EVM,
	contract *vm.Contract,
	stack *vm.Stack,
	memory *vm.Memory,
	constantGas uint64,
	slotKey MVKey,
) {
	if !c.canCapturePolicy() {
		return
	}
	if _, hot := c.hotSlots[slotKey]; !hot {
		if c.recordTrace {
			var threshold float64
			if c.adaptiveModel != nil {
				threshold = c.adaptiveModel.ScoreThreshold
			}
			c.decisionEvents = append(c.decisionEvents, AdaptiveDecisionEvent{
				Policy:             c.policyName,
				EffectivePolicy:    c.effectivePolicy,
				BlockNumber:        c.blockNumber,
				TxIndex:            c.txIndex,
				TxHash:             c.txHash.Hex(),
				CodeHash:           c.codeHash.Hex(),
				Selector:           FormatSelectorHex(c.selector),
				PC:                 c.expectedPC,
				Steps:              step,
				Opcode:             vm.OpCode(opcode).String(),
				SlotKey:            slotKey.String(),
				ThresholdNsPerByte: threshold,
				Decision:           "skipped_cold_slot",
				Reason:             "slot_not_in_prev_block_hot_top32",
			})
		}
		return
	}

	scoreStart := time.Now()
	c.adaptiveCandidates++

	pc := c.expectedPC
	loc := AdaptiveLocationKey{
		CodeHash: c.codeHash,
		Selector: c.selector,
		PC:       pc,
		Slot:     slotKey,
	}

	var threshold float64
	if c.adaptiveModel != nil {
		threshold = c.adaptiveModel.ScoreThreshold
	}

	recordDecision := func(ev AdaptiveDecisionEvent) {
		if c.recordTrace {
			c.decisionEvents = append(c.decisionEvents, ev)
		}
	}
	baseEv := AdaptiveDecisionEvent{
		Policy:             c.policyName,
		EffectivePolicy:    c.effectivePolicy,
		BlockNumber:        c.blockNumber,
		TxIndex:            c.txIndex,
		TxHash:             c.txHash.Hex(),
		CodeHash:           c.codeHash.Hex(),
		Selector:           FormatSelectorHex(c.selector),
		PC:                 pc,
		Steps:              step,
		Opcode:             vm.OpCode(opcode).String(),
		SlotKey:            slotKey.String(),
		ThresholdNsPerByte: threshold,
	}

	// mỗi tx nhận tối đa 1 điểm từ nhánh tính điểm adaptive
	if c.adaptiveCapturedCount >= 1 {
		if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
			c.scoreNs += uint64(dt)
		}
		ev := baseEv
		ev.Decision = "skipped_tx_limit"
		ev.Reason = "max_one_adaptive_checkpoint_per_tx"
		recordDecision(ev)
		return
	}

	p, seen, useful, probSource, hasHist := c.adaptiveHistory.LookupProb(loc)
	baseEv.Prob = p
	baseEv.Seen = seen
	baseEv.Useful = useful
	baseEv.ProbSource = probSource
	if !hasHist {
		c.adaptiveHistoryMisses++
		if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
			c.scoreNs += uint64(dt)
		}
		ev := baseEv
		ev.Decision = "skipped_no_history"
		ev.Reason = "missing_history_sample"
		recordDecision(ev)
		return
	}

	returnData, rErr := fastGethReturnDataView(evmEnv)
	if rErr != nil {
		if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
			c.scoreNs += uint64(dt)
		}
		c.err = rErr
		return
	}
	preReadsLen := 0
	if c.state != nil {
		preReadsLen = len(c.state.ReadSet)
	}
	estimatedBytes, _, estErr := EstimatePureCheckpointBytes(len(stack.Data()), len(memory.Data()), len(returnData), preReadsLen)
	if estErr != nil {
		if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
			c.scoreNs += uint64(dt)
		}
		c.err = estErr
		return
	}
	bucket := SizeBucket(estimatedBytes)
	baseEv.Bytes = estimatedBytes
	baseEv.SizeBucket = bucket

	calKey := AdaptiveCalibrationKey{
		CodeHash:   c.codeHash,
		Selector:   c.selector,
		PC:         pc,
		SizeBucket: bucket,
	}
	calEntry, hasCal := c.adaptiveModel.Lookup(calKey)
	if !hasCal {
		c.adaptiveCalibMisses++
		if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
			c.scoreNs += uint64(dt)
		}
		ev := baseEv
		ev.Decision = "skipped_no_calibration"
		ev.Reason = "missing_calibration_sample"
		recordDecision(ev)
		return
	}
	c.adaptiveCalibHits++
	baseEv.GainNs = calEntry.GainNs
	baseEv.CaptureNs = calEntry.CaptureNs

	sr := ScoreAdaptiveCandidate(AdaptiveScoreInput{
		Prob:           p,
		GainNs:         calEntry.GainNs,
		CaptureNs:      calEntry.CaptureNs,
		AccountedBytes: estimatedBytes,
		Threshold:      threshold,
	})
	baseEv.NetNs = sr.NetNs
	baseEv.ScoreNsPerByte = sr.Score
	if dt := time.Since(scoreStart).Nanoseconds(); dt > 0 {
		c.scoreNs += uint64(dt)
	}

	if !sr.Accept {
		c.captureSkippedLowScore++
		ev := baseEv
		if sr.Reason == "non_positive_net" || sr.Reason == "below_threshold" {
			ev.Decision = "skipped_low_score"
		} else {
			ev.Decision = "skipped_invalid"
		}
		ev.Reason = sr.Reason
		recordDecision(ev)
		return
	}

	if contract.Gas > ^uint64(0)-constantGas {
		c.err = fmt.Errorf("pre-gas checkpoint gas overflow")
		return
	}
	k := slotKey
	cp, resID, capErr := c.reserveAndBuildCheckpoint(step, pc, opcode, contract.Gas+constantGas, stack, memory, returnData, "adaptive", &k)
	if capErr != nil {
		c.err = capErr
		return
	}
	if cp == nil {
		ev := baseEv
		ev.Decision = "skipped_budget"
		ev.Reason = "budget_full"
		recordDecision(ev)
		return
	}
	c.adaptiveCapturedCount++
	ev := baseEv
	ev.Decision = "captured"
	ev.Reason = "score_above_threshold"
	var evPtr *AdaptiveDecisionEvent
	if c.recordTrace {
		evPtr = &ev
	}
	c.pending = &pendingPhase2Capture{resID: resID, opcode: opcode, step: step, cp: cp, decisionEvent: evPtr}
}

// beforeDynamicGas chạy từ bảng nhảy riêng trước khi Geth gọi hàm dynamicGas của opcode.
// Lúc này Geth mới trừ constantGas; dynamicGas chưa đọc storage, chưa làm nóng accessList,
// chưa đổi refund và chưa cập nhật memory.lastGasCost.
func (c *phase2CheckpointCapture) beforeDynamicGas(opcode byte, evmEnv *vm.EVM, contract *vm.Contract, stack *vm.Stack, memory *vm.Memory, constantGas uint64) {
	if c == nil || c.err != nil {
		return
	}
	if c.disarmed {
		if c.state != nil && !purePrefixOpcodeTable[opcode] {
			c.state.SetCurrentStep(int64(c.position))
		}
		return
	}
	depth, err := fastGethDepth(evmEnv)
	if err != nil {
		c.err = err
		return
	}
	if depth != 1 {
		c.disarmed = true
		c.sawUnsupportedOpcode = true
		return
	}
	c.initRootBaseline(contract)
	c.sawDynamicGasStep = true
	step := c.position
	op := vm.OpCode(opcode)

	if !c.disarmed {
		// quan sát ứng viên đầu tiên cho adaptive ở MỌI block (kể cả block 1 bootstrap fixed và khóa chưa nóng)
		isStateSlotOp := op == vm.SLOAD || op == vm.SSTORE
		var slotKey MVKey
		hasSlotKey := false
		if isStateSlotOp && contract != nil && stack != nil {
			data := stack.Data()
			if len(data) > 0 {
				top := data[len(data)-1]
				slotKey = MVKey{Address: contract.Address(), Kind: MVStorage, Slot: common.Hash(top.Bytes32())}
				hasSlotKey = true
			}
		}

		if c.observeAdaptive && isStateSlotOp && hasSlotKey && c.observedCandidate == nil {
			if c.isPrefixStatePure() {
				c.observedCandidate = &AdaptiveCandidateObservation{
					Key: AdaptiveLocationKey{
						CodeHash: c.codeHash,
						Selector: c.selector,
						PC:       c.expectedPC,
						Slot:     slotKey,
					},
					Step:        step,
					Opcode:      opcode,
					PrefixValid: true,
				}
				if c.recordTrace && c.policyName == "adaptive" && c.effectivePolicy == "fixed" {
					var threshold float64
					if c.adaptiveModel != nil {
						threshold = c.adaptiveModel.ScoreThreshold
					}
					c.decisionEvents = append(c.decisionEvents, AdaptiveDecisionEvent{
						Policy:             c.policyName,
						EffectivePolicy:    c.effectivePolicy,
						BlockNumber:        c.blockNumber,
						TxIndex:            c.txIndex,
						TxHash:             c.txHash.Hex(),
						CodeHash:           c.codeHash.Hex(),
						Selector:           FormatSelectorHex(c.selector),
						PC:                 c.expectedPC,
						Steps:              step,
						Opcode:             vm.OpCode(opcode).String(),
						SlotKey:            slotKey.String(),
						ThresholdNsPerByte: threshold,
						Decision:           "fixed_bootstrap",
						Reason:             "block1_no_history_fixed_bootstrap",
					})
				}
			}
		}

		if c.effectivePolicy == "adaptive" && !c.calibrateFirstState {
			if isStateSlotOp && hasSlotKey && c.isPrefixStatePure() {
				c.evaluateAdaptiveAndCapture(step, opcode, evmEnv, contract, stack, memory, constantGas, slotKey)
			}
		} else if reason, triggerSlot, want := c.shouldCaptureFixedOrHistory(step, op, contract, stack); want {
			if c.isPrefixStatePure() {
				if contract.Gas > ^uint64(0)-constantGas {
					c.err = fmt.Errorf("pre-gas checkpoint gas overflow")
					return
				}
				returnData, rErr := fastGethReturnDataView(evmEnv)
				if rErr != nil {
					c.err = rErr
					return
				}
				cp, resID, capErr := c.reserveAndBuildCheckpoint(step, c.expectedPC, opcode, contract.Gas+constantGas, stack, memory, returnData, reason, triggerSlot)
				if capErr != nil {
					c.err = capErr
					return
				}
				if cp != nil {
					if reason == "history" {
						c.historyCapturedCount++
					}
					c.pending = &pendingPhase2Capture{resID: resID, opcode: opcode, step: step, cp: cp}
				}
			}
		}
	}

	// gắn chỉ số bước TRƯỚC khi dynamicGas chạy cho opcode có thể đọc state (vd SLOAD/SSTORE/CALL)
	if !purePrefixOpcodeTable[opcode] {
		if c.state != nil {
			c.state.SetCurrentStep(int64(step))
		}
		c.disarmAfterStep = true
	}
}

func (c *phase2CheckpointCapture) onOpcode(pc uint64, opcode byte, gas, cost uint64, context tracing.OpContext, returnData []byte, depth int, opErr error) {
	if c == nil {
		return
	}
	// đếm mọi bước interpreter đi tới ranh giới trước gas, gồm cả opcode cuối gây lỗi
	c.opcodeSteps++

	if c.err != nil {
		c.cancelPending()
		return
	}
	if depth != 1 {
		c.disarmed = true
		c.sawUnsupportedOpcode = true
		c.cancelPending()
		return
	}

	step := c.position
	c.position++
	op := vm.OpCode(opcode)
	if isOutOfScopeSuffixOpcode(op) {
		c.sawUnsupportedOpcode = true
	}

	if c.disarmed {
		if c.state != nil && !purePrefixOpcodeTable[opcode] {
			c.state.SetCurrentStep(int64(step))
		}
		return
	}

	sawDyn := c.sawDynamicGasStep
	c.sawDynamicGasStep = false
	disarmAfter := c.disarmAfterStep
	c.disarmAfterStep = false

	// đường nhanh: history/adaptive/calibrate chỉ chụp ở SLOAD/SSTORE (qua beforeDynamicGas -> pending != nil)
	if c.pending == nil && c.effectivePolicy != "fixed" && purePrefixOpcodeTable[opcode] && !disarmAfter {
		if c.effectivePolicy == "adaptive" {
			if scope, ok := context.(*vm.ScopeContext); ok && scope != nil {
				if !c.rootEntered && scope.Contract != nil {
					c.initRootBaseline(scope.Contract)
				}
				c.advanceExpectedPC(pc, op, scope.Stack)
			}
		} else if !c.rootEntered {
			if scope, ok := context.(*vm.ScopeContext); ok && scope != nil && scope.Contract != nil {
				c.initRootBaseline(scope.Contract)
			}
		}
		return
	}

	scope, _ := context.(*vm.ScopeContext)
	if !c.rootEntered && scope != nil && scope.Contract != nil {
		c.initRootBaseline(scope.Contract)
	}
	if c.state != nil && !purePrefixOpcodeTable[opcode] {
		c.state.SetCurrentStep(int64(step))
	}

	if pending := c.pending; pending != nil {
		c.pending = nil
		if pending.opcode != opcode || pending.step != step {
			c.store.CancelReservation(pending.resID)
			c.err = fmt.Errorf("pre-gas checkpoint boundary mismatch: got op=%d step=%d want op=%d step=%d", opcode, step, pending.opcode, pending.step)
			return
		}
		pending.cp.PC = pc
		if c.observedCandidate != nil && c.observedCandidate.Step == step {
			c.observedCandidate.Key.PC = pc
		}
		if c.recordTrace && len(c.decisionEvents) > 0 && c.decisionEvents[len(c.decisionEvents)-1].Steps == step {
			c.decisionEvents[len(c.decisionEvents)-1].PC = pc
		}
		// ngay cả khi dynamicGas gây OOG (opErr != nil), ảnh chụp trước gas vẫn hợp lệ và pc là pcCopy chính xác
		if err := c.store.CommitReservation(pending.resID, c.txIndex, pending.cp); err != nil {
			c.err = err
			return
		}
		if c.calibrateFirstState && c.calibratedCP == nil {
			c.calibratedCP = pending.cp
		}
		c.captureCreated++
		if c.recordTrace {
			c.appendCaptureTrace("capture_created", pending.cp, pending.cp.Reason)
			if pending.decisionEvent != nil {
				pending.decisionEvent.PC = pc
				c.decisionEvents = append(c.decisionEvents, *pending.decisionEvent)
			}
		}
	} else {
		if c.observedCandidate != nil && c.observedCandidate.Step == step {
			c.observedCandidate.Key.PC = pc
		}
		if c.recordTrace && len(c.decisionEvents) > 0 && c.decisionEvents[len(c.decisionEvents)-1].Steps == step {
			c.decisionEvents[len(c.decisionEvents)-1].PC = pc
		}
		if !sawDyn && opErr == nil && gas >= cost && scope != nil && scope.Contract != nil && scope.Stack != nil && scope.Memory != nil {
			// opcode không có dynamicGas (vd ADD, PUSH, DUP, JUMPI...): tại OnOpcode, gas là gasCopy (trước trừ constantGas)
			if reason, triggerSlot, want := c.shouldCaptureFixedOrHistory(step, op, scope.Contract, scope.Stack); want {
				if c.isPrefixStatePure() {
					cp, resID, capErr := c.reserveAndBuildCheckpoint(step, pc, opcode, gas, scope.Stack, scope.Memory, returnData, reason, triggerSlot)
					if capErr != nil {
						c.err = capErr
						return
					}
					if cp != nil {
						if err := c.store.CommitReservation(resID, c.txIndex, cp); err != nil {
							c.err = err
							return
						}
						if reason == "history" {
							c.historyCapturedCount++
						}
						c.captureCreated++
						if c.recordTrace {
							c.appendCaptureTrace("capture_created", cp, cp.Reason)
						}
					}
				}
			}
		}
	}

	if disarmAfter || !purePrefixOpcodeTable[opcode] {
		c.disarmed = true
		return
	}
	if c.effectivePolicy == "adaptive" && scope != nil {
		c.advanceExpectedPC(pc, op, scope.Stack)
	}
}

func (c *phase2CheckpointCapture) reserveAndBuildCheckpoint(
	step, pc uint64,
	opcode byte,
	gas uint64,
	stack *vm.Stack,
	memory *vm.Memory,
	returnData []byte,
	reason string,
	triggerSlot *MVKey,
) (*PurePrefixCheckpoint, uint64, error) {
	started := time.Now()
	defer func() {
		if dt := time.Since(started).Nanoseconds(); dt > 0 {
			c.captureNs += uint64(dt)
		}
	}()

	c.captureRequested++

	stackData := stack.Data()
	memData := memory.Data()
	preReadsLen := 0
	if c.state != nil {
		preReadsLen = len(c.state.ReadSet)
	}

	estimatedBytes, _, err := EstimatePureCheckpointBytes(len(stackData), len(memData), len(returnData), preReadsLen)
	if err != nil {
		return nil, 0, err
	}

	// reserve-before-copy: xin chỗ đồng bộ TRƯỚC khi cấp phát và sao chép stack/memory
	resID, ok := c.store.Reserve(estimatedBytes)
	if !ok {
		c.captureSkippedBudget++
		if c.recordTrace {
			ev := Phase2TraceEvent{
				Policy:         c.policyName,
				BlockNumber:    c.blockNumber,
				TxIndex:        c.txIndex,
				TxHash:         c.txHash.Hex(),
				Attempt:        int(c.attempt),
				Event:          "capture_skipped_budget",
				Step:           step,
				PC:             pc,
				Opcode:         vm.OpCode(opcode).String(),
				GasRemaining:   gas,
				Reason:         reason,
				AccountedBytes: estimatedBytes,
			}
			if triggerSlot != nil {
				ev.SlotKey = triggerSlot.String()
			}
			c.traceEvents = append(c.traceEvents, ev)
		}
		return nil, 0, nil
	}

	memLastGas, err := gethMemoryLastGasCost(memory)
	if err != nil {
		c.store.CancelReservation(resID)
		return nil, 0, err
	}
	callGasTemp, err := gethPrivateUint64(c.evm, "callGasTemp")
	if err != nil {
		c.store.CancelReservation(resID)
		return nil, 0, err
	}

	// cấp phát đúng len == cap để tránh buffer thừa và đảm bảo cô lập tuyệt đối
	stackCopy := make([]uint256.Int, len(stackData))
	copy(stackCopy, stackData)

	var memCopy []byte
	if len(memData) > 0 {
		memCopy = make([]byte, len(memData))
		copy(memCopy, memData)
	}

	var retCopy []byte
	if len(returnData) > 0 {
		retCopy = make([]byte, len(returnData))
		copy(retCopy, returnData)
	}

	var preReads []MVKey
	var preVersions map[MVKey]uint64
	if preReadsLen > 0 {
		preReads = make([]MVKey, preReadsLen)
		copy(preReads, c.state.ReadSet)
		preVersions = make(map[MVKey]uint64, preReadsLen)
		for _, k := range preReads {
			if c.prefix != nil {
				preVersions[k] = c.prefix.versions[k]
			}
		}
	}

	var trigCopy *MVKey
	if triggerSlot != nil {
		k := *triggerSlot
		trigCopy = &k
	}

	cp := &PurePrefixCheckpoint{
		FormatVersion:       PureCheckpointFormatVersion,
		Fork:                "Shanghai",
		BlockNumber:         c.blockNumber,
		TxIndex:             c.txIndex,
		TxHash:              c.txHash,
		Attempt:             c.attempt,
		Caller:              c.caller,
		Callee:              c.callee,
		CodeHash:            c.codeHash,
		InitialFrameGas:     c.initialFrameGas,
		InputHash:           c.inputHash,
		InputLen:            c.inputLen,
		PC:                  pc,
		Opcode:              opcode,
		NextStep:            step,
		Reason:              reason,
		TriggerSlot:         trigCopy,
		Stack:               stackCopy,
		Memory:              memCopy,
		MemoryLen:           uint64(len(memCopy)),
		MemoryLastGasCost:   memLastGas,
		Gas:                 gas,
		ReturnData:          retCopy,
		CallGasTemp:         callGasTemp,
		PreExecReads:        preReads,
		PreExecReadVersions: preVersions,
		BeforeRead:          preReadsLen,
		AccountedBytes:      estimatedBytes,
	}
	return cp, resID, nil
}

func (c *phase2CheckpointCapture) appendCaptureTrace(event string, cp *PurePrefixCheckpoint, reason string) {
	ev := Phase2TraceEvent{
		Policy:         c.policyName,
		BlockNumber:    c.blockNumber,
		TxIndex:        c.txIndex,
		TxHash:         c.txHash.Hex(),
		Attempt:        int(c.attempt),
		Event:          event,
		Step:           cp.NextStep,
		PC:             cp.PC,
		Opcode:         vm.OpCode(cp.Opcode).String(),
		GasRemaining:   cp.Gas,
		Reason:         reason,
		AccountedBytes: cp.AccountedBytes,
	}
	if cp.TriggerSlot != nil {
		ev.SlotKey = cp.TriggerSlot.String()
	}
	c.traceEvents = append(c.traceEvents, ev)
}

func (c *phase2CheckpointCapture) cancelPending() {
	if c != nil && c.pending != nil {
		if c.store != nil {
			c.store.CancelReservation(c.pending.resID)
		}
		c.pending = nil
	}
}

func (c *phase2CheckpointCapture) finalize() {
	c.cancelPending()
}

// selectPurePrefixCheckpoint duyệt checkpoint từ mới về cũ, chọn điểm có
// NextStep <= earliestInvalidStep và toàn bộ phụ thuộc bước chuẩn bị vẫn khớp versions hiện tại.
func selectPurePrefixCheckpoint(
	checkpoints []*PurePrefixCheckpoint,
	earliestInvalidStep int64,
	versions *MVMemory,
) (selected *PurePrefixCheckpoint, rejectedPrefix uint64, fallbackReason string) {
	if earliestInvalidStep < 0 {
		// phụ thuộc sai nằm ở bước chuẩn bị giao dịch trước bước 0 -> bắt buộc chạy lại từ đầu
		if len(checkpoints) > 0 {
			rejectedPrefix++
		}
		return nil, rejectedPrefix, "prep_read_stale"
	}
	if len(checkpoints) == 0 {
		return nil, 0, "no_checkpoint"
	}
	limitStep := uint64(earliestInvalidStep)
	sawBeforeConflict := false
	for i := len(checkpoints) - 1; i >= 0; i-- {
		cp := checkpoints[i]
		if cp == nil {
			continue
		}
		if cp.NextStep > limitStep {
			continue
		}
		sawBeforeConflict = true
		if !versions.Validate(cp.PreExecReadVersions) {
			rejectedPrefix++
			continue
		}
		return cp, rejectedPrefix, ""
	}
	if sawBeforeConflict {
		return nil, rejectedPrefix, "prefix_validation_failed"
	}
	return nil, rejectedPrefix, "no_checkpoint_before_conflict"
}

// resumePurePrefixAttempt dựng trạng thái và gasPool tạm mới trên nền đã chốt curPrefix,
// chạy mới toàn bộ bước chuẩn bị giao dịch Geth (preCheck, buyGas, IntrinsicGas, Prepare, SetNonce),
// xác thực ngữ cảnh khớp checkpoint rồi gọi resumeGethFrameStrict.
// Nếu không tương thích hoặc gặp CALL/opcode vượt phạm vi -> trả errResumeUnsupported cùng fallbackReason
// và hoàn toàn không chạm vào remainingGas của block.
func resumePurePrefixAttempt(
	txIndex int,
	attempt int,
	tx *types.Transaction,
	prefix *prefixSnapshot,
	env BlockEnv,
	config *params.ChainConfig,
	signer types.Signer,
	cp *PurePrefixCheckpoint,
) (out *windowAttemptOutcome, fallbackReason string, opcodeSteps uint64, err error) {
	if cp == nil || tx == nil || tx.To() == nil || prefix == nil || prefix.state == nil {
		return nil, "invalid_checkpoint", 0, errResumeUnsupported
	}
	if cp.FormatVersion != PureCheckpointFormatVersion {
		return nil, "unsupported_format_version", 0, errResumeUnsupported
	}
	if tx.Value() != nil && tx.Value().Sign() != 0 {
		return nil, "nonzero_tx_value", 0, errResumeUnsupported
	}
	rules := config.Rules(big.NewInt(env.Height), true, env.Time)
	// resume khung đơn chỉ chứng nhận tối đa Shanghai (chưa hỗ trợ Cancun/EIP-6780/TLOAD/Prague)
	if rules.IsCancun || rules.IsEIP4762 || rules.IsPrague {
		return nil, "unsupported_fork", 0, errResumeUnsupported
	}

	msg, err := core.TransactionToMessage(tx, signer, new(big.Int).Set(env.BaseFee))
	if err != nil {
		return nil, "tx_to_message_error", 0, errResumeUnsupported
	}

	// tạo lớp phủ TxState mới và gasPool tạm riêng -> nếu hủy resume, block remainingGas nguyên vẹn
	state := newTxState(prefix.state, txIndex, uint64(attempt), nil, nil)
	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     env.GetHash,
		Coinbase:    env.Coinbase,
		BlockNumber: big.NewInt(env.Height),
		Time:        env.Time,
		Difficulty:  new(big.Int),
		Random:      &env.Random,
		BaseFee:     new(big.Int).Set(env.BaseFee),
		BlobBaseFee: new(big.Int),
		GasLimit:    env.GasLimit,
	}
	evmEnv := vm.NewEVM(blockContext, state, config, vm.Config{})
	evmEnv.SetTxContext(core.NewEVMTxContext(msg))

	tempGasPool := new(core.GasPool).AddGas(prefix.remainingGas)
	initialFrameGas, prepErr := preparePureResumeTransaction(evmEnv, state, rules, msg, tempGasPool)
	if prepErr != nil {
		// giao dịch có thể trở nên không hợp lệ trên nền mới -> fallback FullReplay để ra đúng lỗi Geth
		return nil, "prep_failed", 0, errResumeUnsupported
	}

	// đối chiếu đầy đủ ngữ cảnh trước khi nạp frame
	callSnap := state.Snapshot()
	callee := *msg.To
	for _, pre := range vm.ActivePrecompiles(rules) {
		if pre == callee {
			return nil, "target_is_precompile", 0, errResumeUnsupported
		}
	}
	if !state.Exist(callee) {
		return nil, "target_missing", 0, errResumeUnsupported
	}
	code := state.GetCode(callee)
	codeHash := state.GetCodeHash(callee)
	if len(code) == 0 {
		return nil, "target_empty_code", 0, errResumeUnsupported
	}
	if msg.From != cp.Caller || callee != cp.Callee || codeHash != cp.CodeHash ||
		initialFrameGas != cp.InitialFrameGas || len(msg.Data) != cp.InputLen ||
		crypto.Keccak256Hash(msg.Data) != cp.InputHash {
		return nil, "context_mismatch", 0, errResumeUnsupported
	}

	// dựng bản sao frame độc lập hoàn toàn khỏi checkpoint bất biến trong store
	machine := InterpreterState{
		PC:                cp.PC,
		Stack:             append([]uint256.Int(nil), cp.Stack...),
		Memory:            append([]byte(nil), cp.Memory...),
		MemoryLastGasCost: cp.MemoryLastGasCost,
		ReturnData:        append([]byte(nil), cp.ReturnData...),
		Gas:               cp.Gas,
		CallGasTemp:       cp.CallGasTemp,
		CallFrames: []CallFrameState{{
			PC:          cp.PC,
			Stack:       append([]uint256.Int(nil), cp.Stack...),
			Memory:      append([]byte(nil), cp.Memory...),
			ReturnData:  append([]byte(nil), cp.ReturnData...),
			Gas:         cp.Gas,
			Depth:       1,
			Caller:      msg.From,
			Address:     callee,
			CodeAddress: callee,
			CodeHash:    codeHash,
			Code:        append([]byte(nil), code...),
			Input:       append([]byte(nil), msg.Data...),
		}},
	}

	meter := new(attemptReplayMeter)
	ret, gasRemaining, vmErr := runStrictResumedGethFrame(evmEnv, machine.CallFrames[0], machine, meter)
	if errors.Is(vmErr, errResumeUnsupported) {
		// gặp CALL/CREATE/SELFDESTRUCT ở suffix -> hủy bản tạm, không trừ gas block, fallback FullReplay
		return nil, "unsupported_suffix_opcode", meter.opcodes, errResumeUnsupported
	}
	if vmErr != nil {
		state.RevertToSnapshot(callSnap)
		if vmErr != vm.ErrExecutionReverted {
			gasRemaining = 0
		}
	}

	usedGas, finErr := finalizePureResumedMessage(evmEnv, state, rules, env, msg, msg.GasLimit, gasRemaining, tempGasPool)
	if finErr != nil {
		return nil, "", meter.opcodes, finErr
	}
	if dbErr := state.Err(); dbErr != nil {
		return nil, "", meter.opcodes, fmt.Errorf("tx %d state db error during resume: %w", txIndex, dbErr)
	}
	state.Finalise(true)
	if dbErr := state.Err(); dbErr != nil {
		return nil, "", meter.opcodes, fmt.Errorf("tx %d state db error after resume finalise: %w", txIndex, dbErr)
	}

	delta, err := state.PendingWrites()
	if err != nil {
		return nil, "", meter.opcodes, fmt.Errorf("tx %d collect resumed pending writes: %w", txIndex, err)
	}

	res := &windowAttemptOutcome{
		txIndex:     txIndex,
		attempt:     attempt,
		txHash:      tx.Hash(),
		prefix:      prefix,
		state:       state,
		delta:       delta,
		opcodeSteps: meter.opcodes,
		result: TxResult{
			Index:      txIndex,
			GasUsed:    usedGas,
			Failed:     vmErr != nil,
			ReturnData: append([]byte(nil), ret...),
		},
	}
	if vmErr != nil {
		res.result.ErrMsg = vmErr.Error()
	}
	for _, entry := range state.GetLogs() {
		copyLog := *entry
		copyLog.TxHash = tx.Hash()
		copyLog.TxIndex = uint(txIndex)
		copyLog.BlockNumber = uint64(env.Height)
		copyLog.BlockTimestamp = env.Time
		copyLog.Topics = append([]common.Hash(nil), entry.Topics...)
		copyLog.Data = append([]byte(nil), entry.Data...)
		res.result.Logs = append(res.result.Logs, &copyLog)
	}
	return res, "", meter.opcodes, nil
}

// preparePureResumeTransaction chạy đúng chuỗi tiền xử lý của core.stateTransition.execute() trên Shanghai
// trước khi vào evm.Call: kiểm tra nonce, EOA, fee cap, buyGas, IntrinsicGas, Prepare(accessList) và SetNonce.
func preparePureResumeTransaction(evmEnv *vm.EVM, state *TxState, rules params.Rules, msg *core.Message, gp *core.GasPool) (uint64, error) {
	if !msg.SkipNonceChecks {
		stNonce := state.GetNonce(msg.From)
		if stNonce < msg.Nonce {
			return 0, fmt.Errorf("%w: address %v, tx: %d state: %d", core.ErrNonceTooHigh, msg.From.Hex(), msg.Nonce, stNonce)
		}
		if stNonce > msg.Nonce {
			return 0, fmt.Errorf("%w: address %v, tx: %d state: %d", core.ErrNonceTooLow, msg.From.Hex(), msg.Nonce, stNonce)
		}
		if stNonce+1 < stNonce {
			return 0, fmt.Errorf("%w: address %v, nonce: %d", core.ErrNonceMax, msg.From.Hex(), stNonce)
		}
	}
	if !msg.SkipTransactionChecks {
		code := state.GetCode(msg.From)
		if len(code) > 0 {
			return 0, fmt.Errorf("%w: address %v, len(code): %d", core.ErrSenderNoEOA, msg.From.Hex(), len(code))
		}
	}
	if rules.IsLondon {
		skipCheck := evmEnv.Config.NoBaseFee && msg.GasFeeCap.BitLen() == 0 && msg.GasTipCap.BitLen() == 0
		if !skipCheck {
			if l := msg.GasFeeCap.BitLen(); l > 256 {
				return 0, fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", core.ErrFeeCapVeryHigh, msg.From.Hex(), l)
			}
			if l := msg.GasTipCap.BitLen(); l > 256 {
				return 0, fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", core.ErrTipVeryHigh, msg.From.Hex(), l)
			}
			if msg.GasFeeCap.Cmp(msg.GasTipCap) < 0 {
				return 0, fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", core.ErrTipAboveFeeCap, msg.From.Hex(), msg.GasTipCap, msg.GasFeeCap)
			}
			if msg.GasFeeCap.Cmp(evmEnv.Context.BaseFee) < 0 {
				return 0, fmt.Errorf("%w: address %v, maxFeePerGas: %s, baseFee: %s", core.ErrFeeCapTooLow, msg.From.Hex(), msg.GasFeeCap, evmEnv.Context.BaseFee)
			}
		}
	}

	// buyGas
	mgval := new(big.Int).SetUint64(msg.GasLimit)
	mgval.Mul(mgval, msg.GasPrice)
	balanceCheck := new(big.Int).Set(mgval)
	if msg.GasFeeCap != nil {
		balanceCheck.SetUint64(msg.GasLimit)
		balanceCheck.Mul(balanceCheck, msg.GasFeeCap)
	}
	balanceCheck.Add(balanceCheck, msg.Value)
	balanceCheckU256, overflow := uint256.FromBig(balanceCheck)
	if overflow {
		return 0, fmt.Errorf("%w: address %v required balance exceeds 256 bits", core.ErrInsufficientFunds, msg.From.Hex())
	}
	if have := state.GetBalance(msg.From); have.Cmp(balanceCheckU256) < 0 {
		return 0, fmt.Errorf("%w: address %v have %v want %v", core.ErrInsufficientFunds, msg.From.Hex(), have, balanceCheckU256)
	}
	if err := gp.SubGas(msg.GasLimit); err != nil {
		return 0, err
	}
	mgvalU256, _ := uint256.FromBig(mgval)
	state.SubBalance(msg.From, mgvalU256, tracing.BalanceDecreaseGasBuy)

	gasRemaining := msg.GasLimit
	intrinsicGas, err := core.IntrinsicGas(msg.Data, msg.AccessList, msg.SetCodeAuthorizations, false, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai)
	if err != nil {
		return 0, err
	}
	if gasRemaining < intrinsicGas {
		return 0, fmt.Errorf("%w: have %d, want %d", core.ErrIntrinsicGas, gasRemaining, intrinsicGas)
	}
	gasRemaining -= intrinsicGas

	// làm nóng access list + đặt lại transient storage đúng như Geth
	state.Prepare(rules, msg.From, evmEnv.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)
	state.SetNonce(msg.From, state.GetNonce(msg.From)+1, tracing.NonceChangeEoACall)
	return gasRemaining, nil
}

func finalizePureResumedMessage(
	evmEnv *vm.EVM,
	state *TxState,
	rules params.Rules,
	env BlockEnv,
	msg *core.Message,
	initialGas, gasRemaining uint64,
	gp *core.GasPool,
) (uint64, error) {
	usedBeforeRefund := initialGas - gasRemaining
	quotient := uint64(params.RefundQuotient)
	if rules.IsLondon {
		quotient = params.RefundQuotientEIP3529
	}
	refund := usedBeforeRefund / quotient
	if avail := state.GetRefund(); refund > avail {
		refund = avail
	}
	gasRemaining += refund

	remainingCost := new(uint256.Int).SetUint64(gasRemaining)
	remainingCost.Mul(remainingCost, uint256.MustFromBig(msg.GasPrice))
	state.AddBalance(msg.From, remainingCost, tracing.BalanceIncreaseGasReturn)
	gp.AddGas(gasRemaining)

	usedGas := initialGas - gasRemaining
	effectiveTip := new(big.Int).Set(msg.GasPrice)
	if rules.IsLondon {
		effectiveTip.Sub(effectiveTip, env.BaseFee)
	}
	if !(evmEnv.Config.NoBaseFee && msg.GasFeeCap.Sign() == 0 && msg.GasTipCap.Sign() == 0) {
		effectiveTipU256, _ := uint256.FromBig(effectiveTip)
		fee := new(uint256.Int).SetUint64(usedGas)
		fee.Mul(fee, effectiveTipU256)
		state.AddBalance(env.Coinbase, fee, tracing.BalanceIncreaseRewardTransactionFee)
	}
	return usedGas, nil
}
