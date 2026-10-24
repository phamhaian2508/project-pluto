package evm

import (
	"errors"
	"fmt"
	"time"
)

const maxCheckpointRetries = 2

var errResumeUnsupported = errors.New("execution state does not support checkpoint resume")

// attemptCheckpoint gắn ảnh chụp của trình thông dịch/trạng thái với ranh giới
// đọc và các phiên bản còn hợp lệ tại lúc tạo ảnh chụp. beforeRead đánh số từ 0:
// giá trị k nghĩa là checkpoint nằm ngay trước lần đọc thứ k.
type attemptCheckpoint struct {
	beforeRead     int
	opcodePosition uint64
	execution      *ExecutionCheckpoint
	readVersions   map[MVKey]uint64
}

type attemptRecoveryRunner interface {
	Resume(*attemptCheckpoint, uint64) (*optimisticAttempt, replayWork, error)
	FullReplay(uint64) (*optimisticAttempt, replayWork, error)
}

type replayWork struct {
	opcodes uint64
	gas     uint64
}

type recoveryOutcome struct {
	attempt           *optimisticAttempt
	retries           int
	usedCheckpoint    bool
	replayedOpcodes   uint64
	replayedGas       uint64
	replayNanoseconds uint64
	resumes           uint64
	fullFallbacks     uint64
	reusedPositions   []uint64
}

// recoverOptimisticAttempt không bao giờ áp dụng trạng thái. Hàm chỉ trả về một
// lần thực thi cô lập; bộ phân giải có thứ tự vẫn là nơi duy nhất được chốt phần
// thay đổi.
func recoverOptimisticAttempt(versions *MVMemory, failed *optimisticAttempt, failedRead int, firstIncarnation uint64, runner attemptRecoveryRunner) (recoveryOutcome, error) {
	if versions == nil || failed == nil || runner == nil {
		return recoveryOutcome{}, errors.New("checkpoint recovery requires versions, failed attempt and runner")
	}
	checkpoints := failed.checkpoints
	var outcome recoveryOutcome
	for retry := 0; retry < maxCheckpointRetries; retry++ {
		incarnation := firstIncarnation + uint64(retry)
		checkpoint := nearestValidCheckpoint(checkpoints, failedRead, versions)
		var (
			next       *optimisticAttempt
			work       replayWork
			err        error
			usedResume bool
		)
		replayStarted := time.Now()
		if checkpoint != nil {
			next, work, err = runner.Resume(checkpoint, incarnation)
			usedResume = err == nil
		}
		if checkpoint == nil || errors.Is(err, errResumeUnsupported) {
			next, work, err = runner.FullReplay(incarnation)
			usedResume = false
			outcome.fullFallbacks++
		} else if usedResume {
			outcome.resumes++
			outcome.reusedPositions = append(outcome.reusedPositions, checkpoint.opcodePosition)
		}
		elapsed := time.Since(replayStarted).Nanoseconds()
		if elapsed > 0 {
			outcome.replayNanoseconds += uint64(elapsed)
		}
		outcome.replayedOpcodes += work.opcodes
		outcome.replayedGas += work.gas
		if err != nil {
			return recoveryOutcome{}, err
		}
		if next == nil {
			return recoveryOutcome{}, errors.New("checkpoint recovery produced no attempt")
		}
		if versions.Validate(next.readVersion) {
			outcome.attempt = next
			outcome.retries = retry + 1
			outcome.usedCheckpoint = usedResume
			return outcome, nil
		}
		failedRead = firstInvalidRead(next, versions)
		checkpoints = next.checkpoints
	}
	return recoveryOutcome{}, fmt.Errorf("validation still failed after %d retries", maxCheckpointRetries)
}

func firstInvalidRead(attempt *optimisticAttempt, versions *MVMemory) int {
	if attempt == nil || attempt.state == nil {
		return -1
	}
	for index, key := range attempt.state.ReadSet {
		if versions.Version(key) != attempt.readVersion[key] {
			return index
		}
	}
	return -1
}

func nearestValidCheckpoint(checkpoints []attemptCheckpoint, failedRead int, versions *MVMemory) *attemptCheckpoint {
	if failedRead < 0 {
		return nil
	}
	best := -1
	for i := range checkpoints {
		candidate := &checkpoints[i]
		if candidate.execution == nil || candidate.beforeRead > failedRead || !versions.Validate(candidate.readVersions) {
			continue
		}
		if best < 0 || candidate.beforeRead > checkpoints[best].beforeRead ||
			(candidate.beforeRead == checkpoints[best].beforeRead && candidate.opcodePosition > checkpoints[best].opcodePosition) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &checkpoints[best]
}
