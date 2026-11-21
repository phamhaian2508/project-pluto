package lab

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/params"

	"github.com/phamhaian2508/project-pluto/internal/evm"
)

const (
	// DefaultChainID là chain ID thử nghiệm cho pluto-lab (khớp fixture và evm t8n).
	DefaultChainID uint64 = 1337
	// ForkName giữ nguyên tên fork gửi cho `evm t8n`.
	ForkName = "Shanghai"

	// SuitePhase1, SuitePhase2, SuitePhase3 và SuitePhase4 xác định bộ ca thử của pluto-lab.
	SuitePhase1 = "phase1"
	SuitePhase2 = "phase2"
	SuitePhase3 = "phase3"
	SuitePhase4 = "phase4"

	// Tên chuẩn chính sách checkpoint Phase 2–3.
	CheckpointOff      = "off"
	CheckpointFixed    = "fixed"
	CheckpointHistory  = "history"
	CheckpointAdaptive = "adaptive"

	// DefaultCheckpointInterval là N mặc định cho fixed (và block 1 của history/adaptive).
	DefaultCheckpointInterval uint64 = 64
	// DefaultCheckpointBudgetBytes là M mặc định (64 MiB).
	DefaultCheckpointBudgetBytes uint64 = 64 << 20
)

// CheckpointOptions cấu hình chính sách checkpoint và ngân sách RAM cho đường OCC / predicted-occ.
type CheckpointOptions struct {
	Policy             string             `json:"policy"`
	Interval           uint64             `json:"interval,omitempty"`
	BudgetBytes        uint64             `json:"budgetBytes,omitempty"`
	BudgetTier         string             `json:"budgetTier,omitempty"`
	Phase2Mode         bool               `json:"phase2Mode,omitempty"`
	DisableOffObserver bool               `json:"disableOffObserver,omitempty"`
	HistoryMaxPerTx    int                `json:"historyMaxPerTx,omitempty"`
	ModelPath          string             `json:"modelPath,omitempty"`
	ModelSHA256        string             `json:"modelSha256,omitempty"`
	AdaptiveModel      *evm.AdaptiveModel `json:"-"`
}

func DefaultCheckpointOptions() CheckpointOptions {
	return CheckpointOptions{
		Policy:      CheckpointOff,
		Interval:    DefaultCheckpointInterval,
		BudgetBytes: DefaultCheckpointBudgetBytes,
	}
}

// ParseSuite kiểm tra tên suite đầu vào (phase1|phase2|phase3|phase4).
func ParseSuite(raw string) (string, error) {
	switch raw {
	case "", SuitePhase1:
		return SuitePhase1, nil
	case SuitePhase2:
		return SuitePhase2, nil
	case SuitePhase3:
		return SuitePhase3, nil
	case SuitePhase4:
		return SuitePhase4, nil
	default:
		return "", fmt.Errorf("unsupported suite %q (supported: %q, %q, %q, %q)", raw, SuitePhase1, SuitePhase2, SuitePhase3, SuitePhase4)
	}
}

// ParseCheckpointPolicy kiểm tra tên chính sách checkpoint đơn (off|fixed|history|adaptive).
func ParseCheckpointPolicy(raw string) (string, error) {
	switch raw {
	case "", CheckpointOff:
		return CheckpointOff, nil
	case CheckpointFixed:
		return CheckpointFixed, nil
	case CheckpointHistory:
		return CheckpointHistory, nil
	case CheckpointAdaptive:
		return CheckpointAdaptive, nil
	default:
		return "", fmt.Errorf("unsupported checkpoint policy %q (supported: %q, %q, %q, %q, \"all\")",
			raw, CheckpointOff, CheckpointFixed, CheckpointHistory, CheckpointAdaptive)
	}
}

// ResolveCheckpointPolicies giải mã --checkpoint cho Phase 1/2 (giữ tương thích ngược).
func ResolveCheckpointPolicies(raw string) ([]string, error) {
	return ResolveCheckpointPoliciesForSuite(raw, SuitePhase2)
}

// ResolveCheckpointPoliciesForSuite giải mã --checkpoint theo từng suite (phase3/phase4 mở rộng all gồm adaptive).
func ResolveCheckpointPoliciesForSuite(raw, suite string) ([]string, error) {
	if raw == "all" {
		if suite == SuitePhase3 || suite == SuitePhase4 {
			return []string{CheckpointOff, CheckpointFixed, CheckpointHistory, CheckpointAdaptive}, nil
		}
		return []string{CheckpointOff, CheckpointFixed, CheckpointHistory}, nil
	}
	p, err := ParseCheckpointPolicy(raw)
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// ExecutionMode xác định chiến lược thực thi block trong lab.
type ExecutionMode string

const (
	ModeSequential   ExecutionMode = "sequential"
	ModeOCC          ExecutionMode = "occ"
	ModeGrouped      ExecutionMode = "grouped"
	ModePredictedOCC ExecutionMode = "predicted-occ"
)

// ParseMode kiểm tra tên mode đầu vào từ CLI hoặc API.
func ParseMode(raw string) (ExecutionMode, error) {
	switch ExecutionMode(raw) {
	case ModeSequential:
		return ModeSequential, nil
	case ModeOCC:
		return ModeOCC, nil
	case ModeGrouped:
		return ModeGrouped, nil
	case ModePredictedOCC:
		return ModePredictedOCC, nil
	default:
		return "", fmt.Errorf("unsupported execution mode %q (supported: %q, %q, %q, %q)",
			raw, ModeSequential, ModeOCC, ModeGrouped, ModePredictedOCC)
	}
}

// ShanghaiChainConfig trả về cấu hình Shanghai khớp Geth v1.16.8 tests.Forks["Shanghai"].
func ShanghaiChainConfig(chainID uint64) *params.ChainConfig {
	zeroTime := uint64(0)
	return &params.ChainConfig{
		ChainID:                 new(big.Int).SetUint64(chainID),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		MuirGlacierBlock:        big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		ArrowGlacierBlock:       big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		TerminalTotalDifficulty: big.NewInt(0),
		ShanghaiTime:            &zeroTime,
	}
}
