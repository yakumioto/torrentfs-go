package session_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const unavailableClientMountEnv = "TORRENTFS_TEST_UNAVAILABLE_FUSE_MOUNT"

type unavailableClientResult struct {
	Index int
	Data  []byte
	Error string
}

type unavailableFuseClient struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	results <-chan unavailableClientResult
	done    <-chan error
	stderr  *bytes.Buffer
}

// The Go runtime's SIGURG async-preemption signal can interrupt a blocked FUSE
// Pread. The kernel then sends FUSE_INTERRUPT, legitimately canceling that read's
// context before the backend has even started. Use a separate client process
// with that signal disabled, leaving the tested FUSE server and session under
// normal scheduling (and the race detector) in the parent process.
func startUnavailableFuseClient(t *testing.T, mountpoint string) *unavailableFuseClient {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnavailableFuseClientProcess$", "-test.timeout=45s")
	var debug string
	for _, item := range os.Environ() {
		if value, ok := strings.CutPrefix(item, "GODEBUG="); ok {
			debug = value
			continue
		}
		if !strings.HasPrefix(item, unavailableClientMountEnv+"=") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	if debug != "" {
		debug += ","
	}
	cmd.Env = append(cmd.Env, "GODEBUG="+debug+"asyncpreemptoff=1", unavailableClientMountEnv+"="+mountpoint)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	results := make(chan unavailableClientResult, 32)
	done := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(output)
		for {
			var result unavailableClientResult
			if err := decoder.Decode(&result); err != nil {
				if err != io.EOF {
					results <- unavailableClientResult{Index: -2, Error: "decode client response: " + err.Error()}
				}
				break
			}
			results <- result
		}
		done <- cmd.Wait()
	}()
	return &unavailableFuseClient{cmd: cmd, input: input, results: results, done: done, stderr: stderr}
}

func (c *unavailableFuseClient) command(t *testing.T, value string) {
	t.Helper()
	if _, err := fmt.Fprintln(c.input, value); err != nil {
		t.Fatalf("send FUSE client command: %v", err)
	}
}

func (c *unavailableFuseClient) stop(t *testing.T) {
	t.Helper()
	_, _ = fmt.Fprintln(c.input, "stop")
	_ = c.input.Close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Errorf("FUSE client stopped: %v: %s", err, c.stderr.String())
		}
	case <-time.After(externalProbeWait):
		_ = c.cmd.Process.Kill()
		select {
		case <-c.done:
			t.Errorf("FUSE client did not drain after session shutdown: %s", c.stderr.String())
		case <-time.After(5 * time.Second):
			t.Error("FUSE client did not exit after kill")
		}
	}
}

// TestUnavailableFuseClientProcess is invoked only by the parent regression;
// ordinary test-suite execution does not launch or read a filesystem here.
func TestUnavailableFuseClientProcess(t *testing.T) {
	mountpoint := os.Getenv(unavailableClientMountEnv)
	if mountpoint == "" {
		return
	}
	var mu sync.Mutex
	var readers sync.WaitGroup
	encoder := json.NewEncoder(os.Stdout)
	emit := func(result unavailableClientResult) {
		mu.Lock()
		defer mu.Unlock()
		if err := encoder.Encode(result); err != nil {
			os.Exit(2)
		}
	}
	start := func(index int, name string) {
		file, err := os.Open(filepath.Join(mountpoint, name))
		if err != nil {
			emit(unavailableClientResult{Index: index, Error: err.Error()})
			return
		}
		readers.Add(1)
		go func() {
			defer readers.Done()
			defer func() { _ = file.Close() }()
			data := make([]byte, concurrencyReadChunk)
			n, err := file.ReadAt(data, 0)
			result := unavailableClientResult{Index: index, Data: data[:n]}
			if err != nil {
				result.Error = err.Error()
			}
			emit(result)
		}()
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) == 1 && parts[0] == "healthy" {
			start(-1, "healthy.bin")
			continue
		}
		if len(parts) == 1 && parts[0] == "stop" {
			readers.Wait()
			os.Exit(0)
		}
		if len(parts) == 2 && parts[0] == "missing" {
			index, err := strconv.Atoi(parts[1])
			if err == nil && index >= 0 && index < 12 {
				start(index, filepath.Join("missing", fmt.Sprintf("%03d.bin", index)))
				continue
			}
		}
		_, _ = fmt.Fprintln(os.Stderr, "invalid FUSE test client command")
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(os.Stderr, "FUSE test client input closed before stop")
	os.Exit(2)
}
