package lab

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// Mismatch mô tả một sai lệch cụ thể giữa Pluto và Geth (hoặc giữa hai chế độ thực thi Pluto).
type Mismatch struct {
	BlockNumber uint64
	Scope       string
	Address     *common.Address
	Slot        *common.Hash
	TxIndex     *int
	LogIndex    *int
	Field       string
	Expected    string
	Actual      string
}

func (m Mismatch) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "block=%d scope=%s", m.BlockNumber, m.Scope)
	if m.Address != nil {
		fmt.Fprintf(&b, " addr=%s", m.Address.Hex())
	}
	if m.Slot != nil {
		fmt.Fprintf(&b, " slot=%s", m.Slot.Hex())
	}
	if m.TxIndex != nil {
		fmt.Fprintf(&b, " tx=%d", *m.TxIndex)
	}
	if m.LogIndex != nil {
		fmt.Fprintf(&b, " log=%d", *m.LogIndex)
	}
	fmt.Fprintf(&b, " field=%s expected=%s actual=%s", m.Field, m.Expected, m.Actual)
	return b.String()
}

// CompareReport gom tất cả sai lệch phát hiện được trong một lần đối chiếu.
type CompareReport struct {
	Mismatches []Mismatch
}

func (r *CompareReport) OK() bool {
	return r == nil || len(r.Mismatches) == 0
}

func (r *CompareReport) Add(m Mismatch) {
	r.Mismatches = append(r.Mismatches, m)
}

func (r *CompareReport) Merge(other *CompareReport) {
	if other == nil || len(other.Mismatches) == 0 {
		return
	}
	r.Mismatches = append(r.Mismatches, other.Mismatches...)
}

func (r *CompareReport) Error() error {
	if r.OK() {
		return nil
	}
	lines := make([]string, 0, len(r.Mismatches)+1)
	lines = append(lines, fmt.Sprintf("comparison found %d mismatch(es):", len(r.Mismatches)))
	maxPrint := len(r.Mismatches)
	if maxPrint > 20 {
		maxPrint = 20
	}
	for i := 0; i < maxPrint; i++ {
		lines = append(lines, "  - "+r.Mismatches[i].String())
	}
	if len(r.Mismatches) > maxPrint {
		lines = append(lines, fmt.Sprintf("  ... and %d more", len(r.Mismatches)-maxPrint))
	}
	return errors.New(strings.Join(lines, "\n"))
}

// CompareBlockWithGeth so toàn bộ gas, biên nhận, log theo đúng thứ tự và post-state logic ở cuối block.
func CompareBlockWithGeth(
	blockNum uint64,
	inputTxCount int,
	plutoRes *BlockResult,
	plutoPost *evm.LogicalState,
	gethRes *T8NBlockResult,
) *CompareReport {
	report := &CompareReport{}
	if gethRes == nil {
		report.Add(Mismatch{BlockNumber: blockNum, Scope: "geth", Field: "result", Expected: "non-nil", Actual: "<nil>"})
		return report
	}
	if plutoRes == nil {
		report.Add(Mismatch{BlockNumber: blockNum, Scope: "pluto", Field: "result", Expected: "non-nil", Actual: "<nil>"})
		return report
	}

	if len(gethRes.Rejected) > 0 {
		first := gethRes.Rejected[0]
		idx := first.Index
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "rejected",
			TxIndex:     &idx,
			Field:       "rejectedCount",
			Expected:    "0",
			Actual:      fmt.Sprintf("%d (first: idx=%d err=%q)", len(gethRes.Rejected), first.Index, first.Error),
		})
	}

	if len(plutoRes.Receipts) != inputTxCount {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "block",
			Field:       "plutoReceiptCount",
			Expected:    fmt.Sprintf("%d", inputTxCount),
			Actual:      fmt.Sprintf("%d", len(plutoRes.Receipts)),
		})
	}
	if len(gethRes.Receipts) != inputTxCount {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "block",
			Field:       "gethReceiptCount",
			Expected:    fmt.Sprintf("%d", inputTxCount),
			Actual:      fmt.Sprintf("%d", len(gethRes.Receipts)),
		})
	}
	if len(plutoRes.Receipts) != len(gethRes.Receipts) {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "receipts",
			Field:       "count",
			Expected:    fmt.Sprintf("%d", len(gethRes.Receipts)),
			Actual:      fmt.Sprintf("%d", len(plutoRes.Receipts)),
		})
	}

	if plutoRes.GasUsed != gethRes.GasUsed {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "block",
			Field:       "gasUsed",
			Expected:    fmt.Sprintf("%d", gethRes.GasUsed),
			Actual:      fmt.Sprintf("%d", plutoRes.GasUsed),
		})
	}

	minRecs := len(plutoRes.Receipts)
	if len(gethRes.Receipts) < minRecs {
		minRecs = len(gethRes.Receipts)
	}
	for i := 0; i < minRecs; i++ {
		txIdx := i
		p := plutoRes.Receipts[i]
		g := gethRes.Receipts[i]
		if p == nil || g == nil {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "receipt",
				TxIndex:     &txIdx,
				Field:       "nil",
				Expected:    fmt.Sprintf("%v", g != nil),
				Actual:      fmt.Sprintf("%v", p != nil),
			})
			continue
		}
		if p.Type != g.Type {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "type", Expected: fmt.Sprintf("%d", g.Type), Actual: fmt.Sprintf("%d", p.Type)})
		}
		if p.TxHash != g.TxHash {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "txHash", Expected: g.TxHash.Hex(), Actual: p.TxHash.Hex()})
		}
		if p.TransactionIndex != g.TransactionIndex {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "transactionIndex", Expected: fmt.Sprintf("%d", g.TransactionIndex), Actual: fmt.Sprintf("%d", p.TransactionIndex)})
		}
		if p.Status != g.Status {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "status", Expected: fmt.Sprintf("%d", g.Status), Actual: fmt.Sprintf("%d", p.Status)})
		}
		if p.GasUsed != g.GasUsed {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "gasUsed", Expected: fmt.Sprintf("%d", g.GasUsed), Actual: fmt.Sprintf("%d", p.GasUsed)})
		}
		if p.CumulativeGasUsed != g.CumulativeGasUsed {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "cumulativeGasUsed", Expected: fmt.Sprintf("%d", g.CumulativeGasUsed), Actual: fmt.Sprintf("%d", p.CumulativeGasUsed)})
		}
		if p.ContractAddress != g.ContractAddress {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "contractAddress", Expected: g.ContractAddress.Hex(), Actual: p.ContractAddress.Hex()})
		}
		if p.Bloom != g.Bloom {
			report.Add(Mismatch{BlockNumber: blockNum, Scope: "receipt", TxIndex: &txIdx, Field: "logsBloom", Expected: hex.EncodeToString(g.Bloom[:]), Actual: hex.EncodeToString(p.Bloom[:])})
		}

		if len(p.Logs) != len(g.Logs) {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "receipt",
				TxIndex:     &txIdx,
				Field:       "logs.len",
				Expected:    fmt.Sprintf("%d", len(g.Logs)),
				Actual:      fmt.Sprintf("%d", len(p.Logs)),
			})
		}
		minLogs := len(p.Logs)
		if len(g.Logs) < minLogs {
			minLogs = len(g.Logs)
		}
		for j := 0; j < minLogs; j++ {
			logIdx := j
			pl := p.Logs[j]
			gl := g.Logs[j]
			if pl.Address != gl.Address {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "address", Expected: gl.Address.Hex(), Actual: pl.Address.Hex()})
			}
			if len(pl.Topics) != len(gl.Topics) {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "topics.len", Expected: fmt.Sprintf("%d", len(gl.Topics)), Actual: fmt.Sprintf("%d", len(pl.Topics))})
			} else {
				for tIdx := range gl.Topics {
					if pl.Topics[tIdx] != gl.Topics[tIdx] {
						report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: fmt.Sprintf("topics[%d]", tIdx), Expected: gl.Topics[tIdx].Hex(), Actual: pl.Topics[tIdx].Hex()})
					}
				}
			}
			if !bytes.Equal(pl.Data, gl.Data) {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "data", Expected: "0x" + hex.EncodeToString(gl.Data), Actual: "0x" + hex.EncodeToString(pl.Data)})
			}
			if pl.TxHash != gl.TxHash {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "transactionHash", Expected: gl.TxHash.Hex(), Actual: pl.TxHash.Hex()})
			}
			if pl.TxIndex != gl.TxIndex {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "transactionIndex", Expected: fmt.Sprintf("%d", gl.TxIndex), Actual: fmt.Sprintf("%d", pl.TxIndex)})
			}
			if pl.Index != gl.Index {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "logIndex", Expected: fmt.Sprintf("%d", gl.Index), Actual: fmt.Sprintf("%d", pl.Index)})
			}
			if pl.BlockNumber != gl.BlockNumber {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "blockNumber", Expected: fmt.Sprintf("%d", gl.BlockNumber), Actual: fmt.Sprintf("%d", pl.BlockNumber)})
			}
			if pl.BlockTimestamp != gl.BlockTimestamp {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "blockTimestamp", Expected: fmt.Sprintf("%d", gl.BlockTimestamp), Actual: fmt.Sprintf("%d", pl.BlockTimestamp)})
			}
			if pl.Removed != gl.Removed {
				report.Add(Mismatch{BlockNumber: blockNum, Scope: "log", TxIndex: &txIdx, LogIndex: &logIdx, Field: "removed", Expected: fmt.Sprintf("%v", gl.Removed), Actual: fmt.Sprintf("%v", pl.Removed)})
			}
		}
	}

	report.Merge(CompareLogicalState(blockNum, plutoPost, gethRes.PostState))
	return report
}

// CompareLogicalState so trạng thái đầy đủ theo hợp địa chỉ và slot, bắt cả khóa thừa và dữ liệu mồ côi.
func CompareLogicalState(blockNum uint64, plutoPost, gethPost *evm.LogicalState) *CompareReport {
	report := &CompareReport{}
	if plutoPost == nil {
		plutoPost = evm.NewLogicalState()
	}
	if gethPost == nil {
		gethPost = evm.NewLogicalState()
	}

	for _, okKey := range plutoPost.OrphanKeys {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "state",
			Field:       "orphanKey",
			Expected:    "<absent>",
			Actual:      "0x" + hex.EncodeToString(okKey),
		})
	}

	addrSet := make(map[common.Address]struct{}, len(plutoPost.Accounts)+len(gethPost.Accounts))
	for addr := range plutoPost.Accounts {
		addrSet[addr] = struct{}{}
	}
	for addr := range gethPost.Accounts {
		addrSet[addr] = struct{}{}
	}
	addrs := make([]common.Address, 0, len(addrSet))
	for addr := range addrSet {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})

	for _, addr := range addrs {
		a := addr
		pAcc, pOK := plutoPost.Accounts[addr]
		gAcc, gOK := gethPost.Accounts[addr]
		if !pOK && gOK {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "state",
				Address:     &a,
				Field:       "account",
				Expected:    "present",
				Actual:      "<absent>",
			})
			continue
		}
		if pOK && !gOK {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "state",
				Address:     &a,
				Field:       "account",
				Expected:    "<absent>",
				Actual:      fmt.Sprintf("present(nonce=%d,balance=%s)", pAcc.Nonce, pAcc.Balance),
			})
			continue
		}
		if pAcc.Nonce != gAcc.Nonce {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "state",
				Address:     &a,
				Field:       "nonce",
				Expected:    fmt.Sprintf("%d", gAcc.Nonce),
				Actual:      fmt.Sprintf("%d", pAcc.Nonce),
			})
		}
		pBal := "0"
		if pAcc.Balance != nil {
			pBal = pAcc.Balance.Dec()
		}
		gBal := "0"
		if gAcc.Balance != nil {
			gBal = gAcc.Balance.Dec()
		}
		if pBal != gBal {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "state",
				Address:     &a,
				Field:       "balance",
				Expected:    gBal,
				Actual:      pBal,
			})
		}
		if !bytes.Equal(pAcc.Code, gAcc.Code) {
			report.Add(Mismatch{
				BlockNumber: blockNum,
				Scope:       "state",
				Address:     &a,
				Field:       "code",
				Expected:    "0x" + hex.EncodeToString(gAcc.Code),
				Actual:      "0x" + hex.EncodeToString(pAcc.Code),
			})
		}

		slotSet := make(map[common.Hash]struct{}, len(pAcc.Storage)+len(gAcc.Storage))
		for s := range pAcc.Storage {
			slotSet[s] = struct{}{}
		}
		for s := range gAcc.Storage {
			slotSet[s] = struct{}{}
		}
		slots := make([]common.Hash, 0, len(slotSet))
		for s := range slotSet {
			slots = append(slots, s)
		}
		sort.Slice(slots, func(i, j int) bool {
			return bytes.Compare(slots[i][:], slots[j][:]) < 0
		})
		for _, slot := range slots {
			s := slot
			pVal, pHas := pAcc.Storage[slot]
			gVal := gAcc.Storage[slot]
			if pHas && pVal == (common.Hash{}) {
				report.Add(Mismatch{
					BlockNumber: blockNum,
					Scope:       "state",
					Address:     &a,
					Slot:        &s,
					Field:       "storageZeroKey",
					Expected:    "<absent>",
					Actual:      pVal.Hex(),
				})
				continue
			}
			if pVal != gVal {
				report.Add(Mismatch{
					BlockNumber: blockNum,
					Scope:       "state",
					Address:     &a,
					Slot:        &s,
					Field:       "storage",
					Expected:    gVal.Hex(),
					Actual:      pVal.Hex(),
				})
			}
		}
	}
	return report
}

// CompareInvalidBlock xác minh ca cố ý sai đều bị Pluto và t8n từ chối tại cùng chỉ số và nhóm lỗi đầu tiên.
func CompareInvalidBlock(blockNum uint64, plutoErr error, gethRes *T8NBlockResult) *CompareReport {
	report := &CompareReport{}
	if gethRes == nil {
		report.Add(Mismatch{BlockNumber: blockNum, Scope: "invalidBlock", Field: "gethResult", Expected: "non-nil", Actual: "<nil>"})
		return report
	}
	if len(gethRes.Rejected) == 0 {
		report.Add(Mismatch{BlockNumber: blockNum, Scope: "invalidBlock", Field: "gethRejected", Expected: ">=1", Actual: "0"})
		return report
	}
	var rejErr *evm.TxRejectedError
	if !errors.As(plutoErr, &rejErr) {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "invalidBlock",
			Field:       "plutoError",
			Expected:    fmt.Sprintf("TxRejectedError(index=%d)", gethRes.Rejected[0].Index),
			Actual:      fmt.Sprintf("%v", plutoErr),
		})
		return report
	}

	first := gethRes.Rejected[0]
	if rejErr.Index != first.Index {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "invalidBlock",
			Field:       "firstRejectedIndex",
			Expected:    fmt.Sprintf("%d", first.Index),
			Actual:      fmt.Sprintf("%d", rejErr.Index),
		})
	}
	gethCat := evm.ClassifyRejectionError(first.Error)
	if rejErr.Category != gethCat {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "invalidBlock",
			Field:       "rejectionCategory",
			Expected:    fmt.Sprintf("%s (%s)", gethCat, first.Error),
			Actual:      fmt.Sprintf("%s (%v)", rejErr.Category, rejErr.Err),
		})
	}
	return report
}

// CompareAppHashes đối chiếu AppHash giữa các mode Pluto ở cùng block.
func CompareAppHashes(blockNum uint64, modeA string, hashA common.Hash, modeB string, hashB common.Hash) *CompareReport {
	report := &CompareReport{}
	if hashA != hashB {
		report.Add(Mismatch{
			BlockNumber: blockNum,
			Scope:       "appHash",
			Field:       fmt.Sprintf("%s_vs_%s", modeA, modeB),
			Expected:    hashA.Hex(),
			Actual:      hashB.Hex(),
		})
	}
	return report
}
