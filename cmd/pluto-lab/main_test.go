package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phamhaian2508/project-pluto/internal/lab"
)

func TestCLIRejectsInvalidArgumentsAndModes(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{}, &out, &errBuf); code == 0 {
		t.Fatal("expected non-zero exit for empty args")
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"unknown-cmd"}, &out, &errBuf); code == 0 {
		t.Fatal("expected non-zero exit for unknown subcommand")
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "--case", "counter-2tx", "--mode", "bad-mode"}, &out, &errBuf); code == 0 {
		t.Fatal("expected non-zero exit for invalid mode")
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "--case", "does-not-exist", "--mode", "sequential"}, &out, &errBuf); code == 0 {
		t.Fatal("expected non-zero exit for missing fixture")
	}
}

func TestCLIGenSwapPoolsVerifyManifest(t *testing.T) {
	testdata := filepath.Join("..", "..", "testdata", "phase1", "swap-pools")
	outDir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{
		"gen-swap-pools",
		"--artifacts", testdata,
		"--out", outDir,
		"--verify-manifest", filepath.Join(testdata, "manifest.json"),
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gen-swap-pools failed (code=%d): stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "PASS: regenerated swap-pools") {
		t.Fatalf("unexpected stdout: %s", out.String())
	}
}

func TestCLIVerifyCounter2TxWithEVMBin(t *testing.T) {
	if _, err := lab.ResolveEVMBin(""); err != nil {
		if _, statErr := os.Stat("/tmp/plutof-evm-v1168/evm"); statErr != nil {
			t.Skipf("evm binary not available: %v", err)
		}
		t.Setenv("EVM_BIN", "/tmp/plutof-evm-v1168/evm")
	}

	testdata := filepath.Join("..", "..", "testdata", "phase1")
	var out, errBuf bytes.Buffer
	code := run([]string{
		"verify",
		"--case", "counter-2tx",
		"--mode", "all",
		"--workers", "4",
		"--testdata", testdata,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("verify failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), out.String())
	}
	if !strings.Contains(out.String(), "PASS: pluto matches geth t8n") {
		t.Fatalf("expected PASS line in stdout, got:\n%s", out.String())
	}
}

func TestCLIPhase2FlagValidationAndTraceOutput(t *testing.T) {
	badCases := [][]string{
		{"verify", "--suite", "phase99", "--case", "counter-2tx"},
		{"verify", "--suite", "phase2", "--case", "checkpoint-late", "--mode", "sequential"},
		{"verify", "--case", "counter-2tx", "--mode", "occ", "--checkpoint", "bogus"},
		{"verify", "--case", "counter-2tx", "--mode", "occ", "--checkpoint", "fixed", "--checkpoint-n", "0"},
		{"verify", "--case", "counter-2tx", "--mode", "occ", "--checkpoint", "fixed", "--checkpoint-n", "32", "--checkpoint-every", "64"},
		{"verify", "--case", "counter-2tx", "--mode", "occ", "--checkpoint", "fixed", "--checkpoint-budget-bytes", "1024", "--checkpoint-budget-mib", "1"},
		{"verify", "--case", "counter-2tx", "--mode", "grouped", "--checkpoint", "fixed"},
	}
	for _, args := range badCases {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code == 0 {
			t.Fatalf("expected non-zero exit for invalid args %v, stdout=%s", args, out.String())
		}
	}

	if _, err := lab.ResolveEVMBin(""); err != nil {
		if _, statErr := os.Stat("/tmp/plutof-evm-v1168/evm"); statErr != nil {
			t.Skipf("evm binary not available: %v", err)
		}
		t.Setenv("EVM_BIN", "/tmp/plutof-evm-v1168/evm")
	}

	// Kiểm tra chạy verify + --checkpoint all + --trace trên fixture counter-2tx có sẵn
	testdata := filepath.Join("..", "..", "testdata", "phase1")
	traceFile := filepath.Join(t.TempDir(), "sub", "trace.jsonl")
	var out, errBuf bytes.Buffer
	code := run([]string{
		"verify",
		"--case", "counter-2tx",
		"--mode", "occ",
		"--checkpoint", "all",
		"--checkpoint-n", "4",
		"--checkpoint-budget-bytes", "67108864",
		"--workers", "2",
		"--testdata", testdata,
		"--trace", traceFile,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("verify --checkpoint all failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), out.String())
	}
	traceBytes, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("expected trace file to be created: %v", err)
	}
	if len(traceBytes) == 0 {
		t.Fatal("expected non-empty trace JSONL output")
	}

	// Kiểm tra bench sinh CSV có đủ cột Phase 2 khi bật --checkpoint all
	benchOut := t.TempDir()
	out.Reset()
	errBuf.Reset()
	code = run([]string{
		"bench",
		"--case", "counter-2tx",
		"--mode", "occ",
		"--checkpoint", "all",
		"--checkpoint-n", "8",
		"--checkpoint-budget-bytes", "67108864",
		"--workers", "2",
		"--runs", "1",
		"--testdata", testdata,
		"--out", benchOut,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("bench --checkpoint all failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), out.String())
	}
	rawCSV, err := os.ReadFile(filepath.Join(benchOut, "raw.csv"))
	if err != nil {
		t.Fatalf("read raw.csv: %v", err)
	}
	headerLine := strings.SplitN(string(rawCSV), "\n", 2)[0]
	for _, wantCol := range []string{
		"checkpoint_policy", "checkpoint_every", "checkpoint_budget_bytes",
		"capture_requested", "capture_created", "capture_skipped_budget",
		"resume_attempted", "resume_committed", "resume_cancelled",
		"restore_rejected_prefix", "accounted_peak_bytes",
		"capture_ns", "select_ns", "restore_ns",
		"opcode_steps", "prefix_steps_saved", "fallback_reasons",
	} {
		if !strings.Contains(headerLine, wantCol) {
			t.Fatalf("Phase 2 raw.csv header missing column %q: %s", wantCol, headerLine)
		}
	}
}

func TestCLIPhase3FlagValidationAndPredictedOCCVerify(t *testing.T) {
	badCases := [][]string{
		{"calibrate", "--suite", "phase1"},
		{"verify", "--suite", "phase3", "--case", "merkle-2tx-late", "--mode", "occ", "--checkpoint", "adaptive"},
		{"verify", "--suite", "phase3", "--case", "merkle-2tx-late", "--mode", "occ", "--checkpoint", "adaptive", "--model", "does-not-exist.json"},
		{"verify", "--suite", "phase3", "--case", "merkle-2tx-late", "--mode", "grouped", "--checkpoint", "fixed"},
		{"bench", "--suite", "phase3", "--matrix"},
	}
	for _, args := range badCases {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code == 0 {
			t.Fatalf("expected non-zero exit for invalid Phase 3 args %v, stdout=%s", args, out.String())
		}
	}

	if _, err := lab.ResolveEVMBin(""); err != nil {
		if _, statErr := os.Stat("/tmp/plutof-evm-v1168/evm"); statErr != nil {
			t.Skipf("evm binary not available: %v", err)
		}
		t.Setenv("EVM_BIN", "/tmp/plutof-evm-v1168/evm")
	}

	testdata := filepath.Join("..", "..", "testdata", "phase3")
	var out, errBuf bytes.Buffer
	code := run([]string{
		"verify",
		"--suite", "phase3",
		"--case", "merkle-2tx-late",
		"--mode", "predicted-occ",
		"--checkpoint", "history",
		"--workers", "2",
		"--testdata", testdata,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("verify phase3 predicted-occ failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), out.String())
	}
	if !strings.Contains(out.String(), "PASS: pluto matches geth t8n") {
		t.Fatalf("expected PASS in stdout, got:\n%s", out.String())
	}
}
