package evm

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CheckpointMetrics chỉ dùng để quan sát. Không trường nào được đọc trong quá
// trình thực thi giao dịch, xác thực, chọn checkpoint hoặc chốt trạng thái.
type CheckpointMetrics struct {
	ReplayedOpcodes       uint64
	ReplayedGas           uint64
	ReplayNanoseconds     uint64
	CheckpointsCreated    uint64
	CheckpointsReused     uint64
	CheckpointMemoryBytes uint64
	Aborts                uint64
	Retries               uint64
	Resumes               uint64
	FullFallbacks         uint64
	ReusedPositions       []uint64
}

// CheckpointTraceEvent giải thích một xung đột lạc quan theo cách tất định và
// chỉ đọc. Bộ phân giải của chương trình điền dữ liệu sau khi quyết định khôi
// phục; quá trình thực thi và đồng thuận không sử dụng dữ liệu này.
type CheckpointTraceEvent struct {
	TxIndex             int
	FailedRead          int
	CheckpointsCreated  uint64
	CheckpointPositions []uint64
	Action              string
	ReusedPositions     []uint64
	ReplayedOpcodes     uint64
	FullFallbacks       uint64
}

type CheckpointCSVRow struct {
	Run      string
	Policy   CheckpointPolicy
	Interval uint64
	Metrics  CheckpointMetrics
}

var CheckpointCSVHeader = []string{
	"run", "policy", "interval", "replayed_opcodes", "replayed_gas", "replay_ns",
	"checkpoints_created", "checkpoints_reused", "checkpoint_memory_bytes",
	"aborts", "retries", "resumes", "full_fallbacks", "reused_positions",
}

func (p CheckpointPolicy) String() string {
	switch p {
	case CheckpointPolicyP0:
		return "P0"
	case CheckpointPolicyP1:
		return "P1"
	case CheckpointPolicyP2:
		return "P2"
	case CheckpointPolicyP3:
		return "P3"
	default:
		return fmt.Sprintf("UNKNOWN-%d", p)
	}
}

// WriteCheckpointCSV ghi một dòng CSV ổn định cho mỗi lần chạy được cung cấp.
func WriteCheckpointCSV(w io.Writer, rows []CheckpointCSVRow) error {
	writer := csv.NewWriter(w)
	if err := writer.Write(CheckpointCSVHeader); err != nil {
		return err
	}
	for _, row := range rows {
		positions := make([]string, len(row.Metrics.ReusedPositions))
		for i, position := range row.Metrics.ReusedPositions {
			positions[i] = strconv.FormatUint(position, 10)
		}
		record := []string{
			row.Run, row.Policy.String(), strconv.FormatUint(row.Interval, 10),
			strconv.FormatUint(row.Metrics.ReplayedOpcodes, 10),
			strconv.FormatUint(row.Metrics.ReplayedGas, 10),
			strconv.FormatUint(row.Metrics.ReplayNanoseconds, 10),
			strconv.FormatUint(row.Metrics.CheckpointsCreated, 10),
			strconv.FormatUint(row.Metrics.CheckpointsReused, 10),
			strconv.FormatUint(row.Metrics.CheckpointMemoryBytes, 10),
			strconv.FormatUint(row.Metrics.Aborts, 10),
			strconv.FormatUint(row.Metrics.Retries, 10),
			strconv.FormatUint(row.Metrics.Resumes, 10),
			strconv.FormatUint(row.Metrics.FullFallbacks, 10),
			strings.Join(positions, ";"),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}
