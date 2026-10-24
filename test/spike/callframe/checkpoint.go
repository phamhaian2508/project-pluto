// Package callframe là bản thử nghiệm cô lập của M5.1 về checkpoint tại ranh
// giới hoàn chỉnh của lời gọi EVM. Bộ thực thi chính của Pluto không dùng gói này.
package callframe

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
)

var errNestedResumeUnsupported = errors.New("geth hooks cannot restore a parent interpreter after a nested call")

type callExit struct {
	depth    int
	output   []byte
	gasUsed  uint64
	err      error
	reverted bool
}

type recorder struct {
	entered []int
	exited  []callExit
}

func (r *recorder) hooks() *tracing.Hooks {
	return &tracing.Hooks{
		OnEnter: func(depth int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			r.entered = append(r.entered, depth)
		},
		OnExit: func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
			r.exited = append(r.exited, callExit{
				depth: depth, output: append([]byte(nil), output...), gasUsed: gasUsed, err: err, reverted: reverted,
			})
		},
	}
}

// boundaryCheckpoint chỉ lưu dữ liệu có thể lấy bên ngoài EVM. Khôi phục điểm
// này cho phép chạy lại toàn bộ khung ngoài cùng, chứ không tiếp tục trình thông
// dịch cha đang tạm dừng sau một CALL lồng nhau.
type boundaryCheckpoint struct {
	snapshot int
	to       common.Address
	input    []byte
	gas      uint64
}

func takeBoundaryCheckpoint(st *state.StateDB, to common.Address, input []byte, gas uint64) boundaryCheckpoint {
	return boundaryCheckpoint{snapshot: st.Snapshot(), to: to, input: append([]byte(nil), input...), gas: gas}
}

func (c boundaryCheckpoint) restore(st *state.StateDB) { st.RevertToSnapshot(c.snapshot) }

// nestedCheckpoint mô tả giới hạn của móc công khai: callExit có kết quả của lời
// gọi con nhưng không có PC, ngăn xếp, bộ nhớ, gas còn lại hoặc vùng dữ liệu trả
// về của lời gọi cha, nên không thể tạo điểm tiếp tục chính xác.
func nestedCheckpoint(callExit) error { return errNestedResumeUnsupported }
