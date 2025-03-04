// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuzz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const (
	// workerFuzzDuration is the amount of time a worker can spend testing random
	// variations of an input given by the coordinator.
	workerFuzzDuration = 100 * time.Millisecond

	// workerTimeoutDuration is the amount of time a worker can go without
	// responding to the coordinator before being stopped.
	workerTimeoutDuration = 1 * time.Second

	// workerExitCode is used as an exit code by fuzz worker processes after an internal error.
	// This distinguishes internal errors from uncontrolled panics and other crashes.
	// Keep in sync with internal/fuzz.workerExitCode.
	workerExitCode = 70

	// workerSharedMemSize is the maximum size of the shared memory file used to
	// communicate with workers. This limits the size of fuzz inputs.
	workerSharedMemSize = 100 << 20 // 100 MB
)

// worker manages a worker process running a test binary. The worker object
// exists only in the coordinator (the process started by 'go test -fuzz').
// workerClient is used by the coordinator to send RPCs to the worker process,
// which handles them with workerServer.
type worker struct {
	dir     string   // working directory, same as package directory
	binPath string   // path to test executable
	args    []string // arguments for test executable
	env     []string // environment for test executable

	coordinator *coordinator

	memMu chan *sharedMem // mutex guarding shared memory with worker; persists across processes.

	cmd         *exec.Cmd     // current worker process
	client      *workerClient // used to communicate with worker process
	waitErr     error         // last error returned by wait, set before termC is closed.
	interrupted bool          // true after stop interrupts a running worker.
	termC       chan struct{} // closed by wait when worker process terminates
}

func newWorker(c *coordinator, dir, binPath string, args, env []string) (*worker, error) {
	mem, err := sharedMemTempFile(workerSharedMemSize)
	if err != nil {
		return nil, err
	}
	memMu := make(chan *sharedMem, 1)
	memMu <- mem
	return &worker{
		dir:         dir,
		binPath:     binPath,
		args:        args,
		env:         env[:len(env):len(env)], // copy on append to ensure workers don't overwrite each other.
		coordinator: c,
		memMu:       memMu,
	}, nil
}

// cleanup releases persistent resources associated with the worker.
func (w *worker) cleanup() error {
	mem := <-w.memMu
	if mem == nil {
		return nil
	}
	close(w.memMu)
	return mem.Close()
}

// coordinate runs the test binary to perform fuzzing.
//
// coordinate loops until ctx is cancelled or a fatal error is encountered.
// If a test process terminates unexpectedly while fuzzing, coordinate will
// attempt to restart and continue unless the termination can be attributed
// to an interruption (from a timer or the user).
//
// While looping, coordinate receives inputs from the coordinator, passes
// those inputs to the worker process, then passes the results back to
// the coordinator.
func (w *worker) coordinate(ctx context.Context) error {
	// Main event loop.
	for {
		// Start or restart the worker if it's not running.
		if !w.isRunning() {
			if err := w.startAndPing(ctx); err != nil {
				return err
			}
		}

		select {
		case <-ctx.Done():
			// Worker was told to stop.
			err := w.stop()
			if err != nil && !w.interrupted && !isInterruptError(err) {
				return err
			}
			return ctx.Err()

		case <-w.termC:
			// Worker process terminated unexpectedly while waiting for input.
			err := w.stop()
			if w.interrupted {
				panic("worker interrupted after unexpected termination")
			}
			if err == nil || isInterruptError(err) {
				// Worker stopped, either by exiting with status 0 or after being
				// interrupted with a signal that was not sent by the coordinator.
				//
				// When the user presses ^C, on POSIX platforms, SIGINT is delivered to
				// all processes in the group concurrently, and the worker may see it
				// before the coordinator. The worker should exit 0 gracefully (in
				// theory).
				//
				// This condition is probably intended by the user, so suppress
				// the error.
				return nil
			}
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == workerExitCode {
				// Worker exited with a code indicating F.Fuzz was not called correctly,
				// for example, F.Fail was called first.
				return fmt.Errorf("fuzzing process exited unexpectedly due to an internal failure: %w", err)
			}
			// Worker exited non-zero or was terminated by a non-interrupt
			// signal (for example, SIGSEGV) while fuzzing.
			return fmt.Errorf("fuzzing process terminated unexpectedly: %w", err)
			// TODO(jayconrod,katiehockman): if -keepfuzzing, restart worker.

		case input := <-w.coordinator.inputC:
			// Received input from coordinator.
			args := fuzzArgs{
				Limit:        input.limit,
				Timeout:      input.timeout,
				Warmup:       input.warmup,
				CoverageData: input.coverageData,
			}
			entry, resp, err := w.client.fuzz(ctx, input.entry, args)
			canMinimize := true
			if err != nil {
				// Error communicating with worker.
				w.stop()
				if ctx.Err() != nil {
					// Timeout or interruption.
					return ctx.Err()
				}
				if w.interrupted {
					// Communication error before we stopped the worker.
					// Report an error, but don't record a crasher.
					return fmt.Errorf("communicating with fuzzing process: %v", err)
				}
				if w.waitErr == nil || isInterruptError(w.waitErr) {
					// Worker stopped, either by exiting with status 0 or after being
					// interrupted with a signal (not sent by coordinator). See comment in
					// termC case above.
					//
					// Since we expect I/O errors around interrupts, ignore this error.
					return nil
				}
				if sig, ok := terminationSignal(w.waitErr); ok && !isCrashSignal(sig) {
					// Worker terminated by a signal that probably wasn't caused by a
					// specific input to the fuzz function. For example, on Linux,
					// the kernel (OOM killer) may send SIGKILL to a process using a lot
					// of memory. Or the shell might send SIGHUP when the terminal
					// is closed. Don't record a crasher.
					return fmt.Errorf("fuzzing process terminated by unexpected signal; no crash will be recorded: %v", w.waitErr)
				}
				// Unexpected termination. Set error message and fall through.
				// We'll restart the worker on the next iteration.
				// Don't attempt to minimize this since it crashed the worker.
				resp.Err = fmt.Sprintf("fuzzing process terminated unexpectedly: %v", w.waitErr)
				canMinimize = false
			}
			result := fuzzResult{
				limit:         input.limit,
				count:         resp.Count,
				totalDuration: resp.TotalDuration,
				entryDuration: resp.InterestingDuration,
				entry:         entry,
				crasherMsg:    resp.Err,
				coverageData:  resp.CoverageData,
				canMinimize:   canMinimize,
			}
			w.coordinator.resultC <- result

		case input := <-w.coordinator.minimizeC:
			// Received input to minimize from coordinator.
			result, err := w.minimize(ctx, input)
			if err != nil {
				// Error minimizing. Send back the original input. If it didn't cause
				// an error before, report it as causing an error now.
				// TODO: double-check this is handled correctly when
				// implementing -keepfuzzing.
				result = fuzzResult{
					entry:       input.entry,
					crasherMsg:  input.crasherMsg,
					canMinimize: false,
					limit:       input.limit,
				}
				if result.crasherMsg == "" {
					result.crasherMsg = err.Error()
				}
			}
			w.coordinator.resultC <- result
		}
	}
}

// minimize tells a worker process to attempt to find a smaller value that
// either causes an error (if we started minimizing because we found an input
// that causes an error) or preserves new coverage (if we started minimizing
// because we found an input that expands coverage).
func (w *worker) minimize(ctx context.Context, input fuzzMinimizeInput) (min fuzzResult, err error) {
	if w.coordinator.opts.MinimizeTimeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, w.coordinator.opts.MinimizeTimeout)
		defer cancel()
	}

	args := minimizeArgs{
		Limit:        input.limit,
		Timeout:      input.timeout,
		KeepCoverage: input.keepCoverage,
	}
	entry, resp, err := w.client.minimize(ctx, input.entry, args)
	if err != nil {
		// Error communicating with worker.
		w.stop()
		if ctx.Err() != nil || w.interrupted || isInterruptError(w.waitErr) {
			// Worker was interrupted, possibly by the user pressing ^C.
			// Normally, workers can handle interrupts and timeouts gracefully and
			// will return without error. An error here indicates the worker
			// may not have been in a good state, but the error won't be meaningful
			// to the user. Just return the original crasher without logging anything.
			return fuzzResult{
				entry:        input.entry,
				crasherMsg:   input.crasherMsg,
				coverageData: input.keepCoverage,
				canMinimize:  false,
				limit:        input.limit,
			}, nil
		}
		return fuzzResult{}, fmt.Errorf("fuzzing process terminated unexpectedly while minimizing: %w", w.waitErr)
	}

	if input.crasherMsg != "" && resp.Err == "" && !resp.Success {
		return fuzzResult{}, fmt.Errorf("attempted to minimize but could not reproduce")
	}

	return fuzzResult{
		entry:         entry,
		crasherMsg:    resp.Err,
		coverageData:  resp.CoverageData,
		canMinimize:   false,
		limit:         input.limit,
		count:         resp.Count,
		totalDuration: resp.Duration,
	}, nil
}

func (w *worker) isRunning() bool {
	return w.cmd != nil
}

// startAndPing starts the worker process and sends it a message to make sure it
// can communicate.
//
// startAndPing returns an error if any part of this didn't work, including if
// the context is expired or the worker process was interrupted before it
// responded. Errors that happen after start but before the ping response
// likely indicate that the worker did not call F.Fuzz or called F.Fail first.
// We don't record crashers for these errors.
func (w *worker) startAndPing(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := w.start(); err != nil {
		return err
	}
	if err := w.client.ping(ctx); err != nil {
		w.stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isInterruptError(err) {
			// User may have pressed ^C before worker responded.
			return err
		}
		// TODO: record and return stderr.
		return fmt.Errorf("fuzzing process terminated without fuzzing: %w", err)
	}
	return nil
}

// start runs a new worker process.
//
// If the process couldn't be started, start returns an error. Start won't
// return later termination errors from the process if they occur.
//
// If the process starts successfully, start returns nil. stop must be called
// once later to clean up, even if the process terminates on its own.
//
// When the process terminates, w.waitErr is set to the error (if any), and
// w.termC is closed.
func (w *worker) start() (err error) {
	if w.isRunning() {
		panic("worker already started")
	}
	w.waitErr = nil
	w.interrupted = false
	w.termC = nil

	cmd := exec.Command(w.binPath, w.args...)
	cmd.Dir = w.dir
	cmd.Env = w.env[:len(w.env):len(w.env)] // copy on append to ensure workers don't overwrite each other.

	// Create the "fuzz_in" and "fuzz_out" pipes so we can communicate with
	// the worker. We don't use stdin and stdout, since the test binary may
	// do something else with those.
	//
	// Each pipe has a reader and a writer. The coordinator writes to fuzzInW
	// and reads from fuzzOutR. The worker inherits fuzzInR and fuzzOutW.
	// The coordinator closes fuzzInR and fuzzOutW after starting the worker,
	// since we have no further need of them.
	fuzzInR, fuzzInW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer fuzzInR.Close()
	fuzzOutR, fuzzOutW, err := os.Pipe()
	if err != nil {
		fuzzInW.Close()
		return err
	}
	defer fuzzOutW.Close()
	setWorkerComm(cmd, workerComm{fuzzIn: fuzzInR, fuzzOut: fuzzOutW, memMu: w.memMu})

	// Start the worker process.
	if err := cmd.Start(); err != nil {
		fuzzInW.Close()
		fuzzOutR.Close()
		return err
	}

	// Worker started successfully.
	// After this, w.client owns fuzzInW and fuzzOutR, so w.client.Close must be
	// called later by stop.
	w.cmd = cmd
	w.termC = make(chan struct{})
	comm := workerComm{fuzzIn: fuzzInW, fuzzOut: fuzzOutR, memMu: w.memMu}
	m := newMutator()
	w.client = newWorkerClient(comm, m)

	go func() {
		w.waitErr = w.cmd.Wait()
		close(w.termC)
	}()

	return nil
}

// stop tells the worker process to exit by closing w.client, then blocks until
// it terminates. If the worker doesn't terminate after a short time, stop
// signals it with os.Interrupt (where supported), then os.Kill.
//
// stop returns the error the process terminated with, if any (same as
// w.waitErr).
//
// stop must be called at least once after start returns successfully, even if
// the worker process terminates unexpectedly.
func (w *worker) stop() error {
	if w.termC == nil {
		panic("worker was not started successfully")
	}
	select {
	case <-w.termC:
		// Worker already terminated.
		if w.client == nil {
			// stop already called.
			return w.waitErr
		}
		// Possible unexpected termination.
		w.client.Close()
		w.cmd = nil
		w.client = nil
		return w.waitErr
	default:
		// Worker still running.
	}

	// Tell the worker to stop by closing fuzz_in. It won't actually stop until it
	// finishes with earlier calls.
	closeC := make(chan struct{})
	go func() {
		w.client.Close()
		close(closeC)
	}()

	sig := os.Interrupt
	if runtime.GOOS == "windows" {
		// Per https://golang.org/pkg/os/#Signal, “Interrupt is not implemented on
		// Windows; using it with os.Process.Signal will return an error.”
		// Fall back to Kill instead.
		sig = os.Kill
	}

	t := time.NewTimer(workerTimeoutDuration)
	for {
		select {
		case <-w.termC:
			// Worker terminated.
			t.Stop()
			<-closeC
			w.cmd = nil
			w.client = nil
			return w.waitErr

		case <-t.C:
			// Timer fired before worker terminated.
			w.interrupted = true
			switch sig {
			case os.Interrupt:
				// Try to stop the worker with SIGINT and wait a little longer.
				w.cmd.Process.Signal(sig)
				sig = os.Kill
				t.Reset(workerTimeoutDuration)

			case os.Kill:
				// Try to stop the worker with SIGKILL and keep waiting.
				w.cmd.Process.Signal(sig)
				sig = nil
				t.Reset(workerTimeoutDuration)

			case nil:
				// Still waiting. Print a message to let the user know why.
				fmt.Fprintf(w.coordinator.opts.Log, "waiting for fuzzing process to terminate...\n")
			}
		}
	}
}

// RunFuzzWorker is called in a worker process to communicate with the
// coordinator process in order to fuzz random inputs. RunFuzzWorker loops
// until the coordinator tells it to stop.
//
// fn is a wrapper on the fuzz function. It may return an error to indicate
// a given input "crashed". The coordinator will also record a crasher if
// the function times out or terminates the process.
//
// RunFuzzWorker returns an error if it could not communicate with the
// coordinator process.
func RunFuzzWorker(ctx context.Context, fn func(CorpusEntry) error) error {
	comm, err := getWorkerComm()
	if err != nil {
		return err
	}
	srv := &workerServer{
		workerComm: comm,
		fuzzFn:     fn,
		m:          newMutator(),
	}
	return srv.serve(ctx)
}

// call is serialized and sent from the coordinator on fuzz_in. It acts as
// a minimalist RPC mechanism. Exactly one of its fields must be set to indicate
// which method to call.
type call struct {
	Ping     *pingArgs
	Fuzz     *fuzzArgs
	Minimize *minimizeArgs
}

// minimizeArgs contains arguments to workerServer.minimize. The value to
// minimize is already in shared memory.
type minimizeArgs struct {
	// Timeout is the time to spend minimizing. This may include time to start up,
	// especially if the input causes the worker process to terminated, requiring
	// repeated restarts.
	Timeout time.Duration

	// Limit is the maximum number of values to test, without spending more time
	// than Duration. 0 indicates no limit.
	Limit int64

	// KeepCoverage is a set of coverage counters the worker should attempt to
	// keep in minimized values. When provided, the worker will reject inputs that
	// don't cause at least one of these bits to be set.
	KeepCoverage []byte
}

// minimizeResponse contains results from workerServer.minimize.
type minimizeResponse struct {
	// Success is true if the worker found a smaller input, stored in shared
	// memory, that was "interesting" for the same reason as the original input.
	// If minimizeArgs.KeepCoverage was set, the minimized input preserved at
	// least one coverage bit and did not cause an error. Otherwise, the
	// minimized input caused some error, recorded in Err.
	Success bool

	// Err is the error string caused by the value in shared memory, if any.
	Err string

	// CoverageData is the set of coverage bits activated by the minimized value
	// in shared memory. When set, it contains at least one bit from KeepCoverage.
	// CoverageData will be nil if Err is set or if minimization failed.
	CoverageData []byte

	// Duration is the time spent minimizing, not including starting or cleaning up.
	Duration time.Duration

	// Count is the number of values tested.
	Count int64
}

// fuzzArgs contains arguments to workerServer.fuzz. The value to fuzz is
// passed in shared memory.
type fuzzArgs struct {
	// Timeout is the time to spend fuzzing, not including starting or
	// cleaning up.
	Timeout time.Duration

	// Limit is the maximum number of values to test, without spending more time
	// than Duration. 0 indicates no limit.
	Limit int64

	// Warmup indicates whether this is part of a warmup run, meaning that
	// fuzzing should not occur. If coverageEnabled is true, then coverage data
	// should be reported.
	Warmup bool

	// CoverageData is the coverage data. If set, the worker should update its
	// local coverage data prior to fuzzing.
	CoverageData []byte
}

// fuzzResponse contains results from workerServer.fuzz.
type fuzzResponse struct {
	// Duration is the time spent fuzzing, not including starting or cleaning up.
	TotalDuration       time.Duration
	InterestingDuration time.Duration

	// Count is the number of values tested.
	Count int64

	// CoverageData is set if the value in shared memory expands coverage
	// and therefore may be interesting to the coordinator.
	CoverageData []byte

	// Err is the error string caused by the value in shared memory, which is
	// non-empty if the value in shared memory caused a crash.
	Err string
}

// pingArgs contains arguments to workerServer.ping.
type pingArgs struct{}

// pingResponse contains results from workerServer.ping.
type pingResponse struct{}

// workerComm holds pipes and shared memory used for communication
// between the coordinator process (client) and a worker process (server).
// These values are unique to each worker; they are shared only with the
// coordinator, not with other workers.
//
// Access to shared memory is synchronized implicitly over the RPC protocol
// implemented in workerServer and workerClient. During a call, the client
// (worker) has exclusive access to shared memory; at other times, the server
// (coordinator) has exclusive access.
type workerComm struct {
	fuzzIn, fuzzOut *os.File
	memMu           chan *sharedMem // mutex guarding shared memory
}

// workerServer is a minimalist RPC server, run by fuzz worker processes.
// It allows the coordinator process (using workerClient) to call methods in a
// worker process. This system allows the coordinator to run multiple worker
// processes in parallel and to collect inputs that caused crashes from shared
// memory after a worker process terminates unexpectedly.
type workerServer struct {
	workerComm
	m *mutator

	// coverageMask is the local coverage data for the worker. It is
	// periodically updated to reflect the data in the coordinator when new
	// coverage is found.
	coverageMask []byte

	// fuzzFn runs the worker's fuzz function on the given input and returns
	// an error if it finds a crasher (the process may also exit or crash).
	fuzzFn func(CorpusEntry) error
}

// serve reads serialized RPC messages on fuzzIn. When serve receives a message,
// it calls the corresponding method, then sends the serialized result back
// on fuzzOut.
//
// serve handles RPC calls synchronously; it will not attempt to read a message
// until the previous call has finished.
//
// serve returns errors that occurred when communicating over pipes. serve
// does not return errors from method calls; those are passed through serialized
// responses.
func (ws *workerServer) serve(ctx context.Context) error {
	enc := json.NewEncoder(ws.fuzzOut)
	dec := json.NewDecoder(&contextReader{ctx: ctx, r: ws.fuzzIn})
	for {
		var c call
		if err := dec.Decode(&c); err != nil {
			if err == io.EOF || err == ctx.Err() {
				return nil
			} else {
				return err
			}
		}

		var resp interface{}
		switch {
		case c.Fuzz != nil:
			resp = ws.fuzz(ctx, *c.Fuzz)
		case c.Minimize != nil:
			resp = ws.minimize(ctx, *c.Minimize)
		case c.Ping != nil:
			resp = ws.ping(ctx, *c.Ping)
		default:
			return errors.New("no arguments provided for any call")
		}

		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// fuzz runs the test function on random variations of the input value in shared
// memory for a limited duration or number of iterations.
//
// fuzz returns early if it finds an input that crashes the fuzz function (with
// fuzzResponse.Err set) or an input that expands coverage (with
// fuzzResponse.InterestingDuration set).
//
// fuzz does not modify the input in shared memory. Instead, it saves the
// initial PRNG state in shared memory and increments a counter in shared
// memory before each call to the test function. The caller may reconstruct
// the crashing input with this information, since the PRNG is deterministic.
func (ws *workerServer) fuzz(ctx context.Context, args fuzzArgs) (resp fuzzResponse) {
	if args.CoverageData != nil {
		if ws.coverageMask != nil && len(args.CoverageData) != len(ws.coverageMask) {
			panic(fmt.Sprintf("unexpected size for CoverageData: got %d, expected %d", len(args.CoverageData), len(ws.coverageMask)))
		}
		ws.coverageMask = args.CoverageData
	}
	start := time.Now()
	defer func() { resp.TotalDuration = time.Since(start) }()

	if args.Timeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, args.Timeout)
		defer cancel()
	}
	mem := <-ws.memMu
	ws.m.r.save(&mem.header().randState, &mem.header().randInc)
	defer func() {
		resp.Count = mem.header().count
		ws.memMu <- mem
	}()
	if args.Limit > 0 && mem.header().count >= args.Limit {
		panic(fmt.Sprintf("mem.header().count %d already exceeds args.Limit %d", mem.header().count, args.Limit))
	}

	vals, err := unmarshalCorpusFile(mem.valueCopy())
	if err != nil {
		panic(err)
	}

	shouldStop := func() bool {
		return args.Limit > 0 && mem.header().count >= args.Limit
	}
	fuzzOnce := func(entry CorpusEntry) (dur time.Duration, cov []byte, errMsg string) {
		mem.header().count++
		start := time.Now()
		err := ws.fuzzFn(entry)
		dur = time.Since(start)
		if err != nil {
			errMsg = err.Error()
			if errMsg == "" {
				errMsg = "fuzz function failed with no input"
			}
			return dur, nil, errMsg
		}
		if ws.coverageMask != nil && countNewCoverageBits(ws.coverageMask, coverageSnapshot) > 0 {
			return dur, coverageSnapshot, ""
		}
		return dur, nil, ""
	}

	if args.Warmup {
		dur, _, errMsg := fuzzOnce(CorpusEntry{Values: vals})
		if errMsg != "" {
			resp.Err = errMsg
			return resp
		}
		resp.InterestingDuration = dur
		if coverageEnabled {
			resp.CoverageData = coverageSnapshot
		}
		return resp
	}

	for {
		select {
		case <-ctx.Done():
			return resp

		default:
			ws.m.mutate(vals, cap(mem.valueRef()))
			entry := CorpusEntry{Values: vals}
			dur, cov, errMsg := fuzzOnce(entry)
			if errMsg != "" {
				resp.Err = errMsg
				return resp
			}
			if cov != nil {
				// Found new coverage. Before reporting to the coordinator,
				// run the same values once more to deflake.
				if !shouldStop() {
					dur, cov, errMsg = fuzzOnce(entry)
					if errMsg != "" {
						resp.Err = errMsg
						return resp
					}
				}
				if cov != nil {
					resp.CoverageData = cov
					resp.InterestingDuration = dur
					return resp
				}
			}
			if shouldStop() {
				return resp
			}
		}
	}
}

func (ws *workerServer) minimize(ctx context.Context, args minimizeArgs) (resp minimizeResponse) {
	start := time.Now()
	defer func() { resp.Duration = time.Now().Sub(start) }()
	mem := <-ws.memMu
	defer func() { ws.memMu <- mem }()
	vals, err := unmarshalCorpusFile(mem.valueCopy())
	if err != nil {
		panic(err)
	}
	if args.Timeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, args.Timeout)
		defer cancel()
	}

	// Minimize the values in vals, then write to shared memory. We only write
	// to shared memory after completing minimization. If the worker terminates
	// unexpectedly before then, the coordinator will use the original input.
	resp.Success, err = ws.minimizeInput(ctx, vals, &mem.header().count, args.Limit, args.KeepCoverage)
	if resp.Success {
		writeToMem(vals, mem)
	}
	if err != nil {
		resp.Err = err.Error()
	} else if resp.Success {
		resp.CoverageData = coverageSnapshot
	}
	return resp
}

// minimizeInput applies a series of minimizing transformations on the provided
// vals, ensuring that each minimization still causes an error in fuzzFn. Before
// every call to fuzzFn, it marshals the new vals and writes it to the provided
// mem just in case an unrecoverable error occurs. It uses the context to
// determine how long to run, stopping once closed. It returns a bool
// indicating whether minimization was successful and an error if one was found.
func (ws *workerServer) minimizeInput(ctx context.Context, vals []interface{}, count *int64, limit int64, keepCoverage []byte) (success bool, retErr error) {
	wantError := keepCoverage == nil
	shouldStop := func() bool {
		return ctx.Err() != nil ||
			(limit > 0 && *count >= limit) ||
			(retErr != nil && !wantError)
	}
	if shouldStop() {
		return false, nil
	}

	// Check that the original value preserves coverage or causes an error.
	// If not, then whatever caused us to think the value was interesting may
	// have been a flake, and we can't minimize it.
	*count++
	if retErr = ws.fuzzFn(CorpusEntry{Values: vals}); retErr == nil && wantError {
		return false, nil
	} else if retErr != nil && !wantError {
		return false, retErr
	} else if keepCoverage != nil && !hasCoverageBit(keepCoverage, coverageSnapshot) {
		return false, nil
	}

	var valI int
	// tryMinimized runs the fuzz function with candidate replacing the value
	// at index valI. tryMinimized returns whether the input with candidate is
	// interesting for the same reason as the original input: it returns
	// an error if one was expected, or it preserves coverage.
	tryMinimized := func(candidate interface{}) bool {
		prev := vals[valI]
		// Set vals[valI] to the candidate after it has been
		// properly cast. We know that candidate must be of
		// the same type as prev, so use that as a reference.
		switch c := candidate.(type) {
		case float64:
			switch prev.(type) {
			case float32:
				vals[valI] = float32(c)
			case float64:
				vals[valI] = c
			default:
				panic("impossible")
			}
		case uint:
			switch prev.(type) {
			case uint:
				vals[valI] = c
			case uint8:
				vals[valI] = uint8(c)
			case uint16:
				vals[valI] = uint16(c)
			case uint32:
				vals[valI] = uint32(c)
			case uint64:
				vals[valI] = uint64(c)
			case int:
				vals[valI] = int(c)
			case int8:
				vals[valI] = int8(c)
			case int16:
				vals[valI] = int16(c)
			case int32:
				vals[valI] = int32(c)
			case int64:
				vals[valI] = int64(c)
			default:
				panic("impossible")
			}
		case []byte:
			switch prev.(type) {
			case []byte:
				vals[valI] = c
			case string:
				vals[valI] = string(c)
			default:
				panic("impossible")
			}
		default:
			panic("impossible")
		}
		*count++
		err := ws.fuzzFn(CorpusEntry{Values: vals})
		if err != nil {
			retErr = err
			return wantError
		}
		if keepCoverage != nil && hasCoverageBit(keepCoverage, coverageSnapshot) {
			return true
		}
		vals[valI] = prev
		return false
	}

	for valI = range vals {
		if shouldStop() {
			break
		}
		switch v := vals[valI].(type) {
		case bool:
			continue // can't minimize
		case float32:
			minimizeFloat(float64(v), tryMinimized, shouldStop)
		case float64:
			minimizeFloat(v, tryMinimized, shouldStop)
		case uint:
			minimizeInteger(v, tryMinimized, shouldStop)
		case uint8:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case uint16:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case uint32:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case uint64:
			if uint64(uint(v)) != v {
				// Skip minimizing a uint64 on 32 bit platforms, since we'll truncate the
				// value when casting
				continue
			}
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case int:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case int8:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case int16:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case int32:
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case int64:
			if int64(int(v)) != v {
				// Skip minimizing a int64 on 32 bit platforms, since we'll truncate the
				// value when casting
				continue
			}
			minimizeInteger(uint(v), tryMinimized, shouldStop)
		case string:
			minimizeBytes([]byte(v), tryMinimized, shouldStop)
		case []byte:
			minimizeBytes(v, tryMinimized, shouldStop)
		default:
			panic("unreachable")
		}
	}
	return (wantError || retErr == nil), retErr
}

func writeToMem(vals []interface{}, mem *sharedMem) {
	b := marshalCorpusFile(vals...)
	mem.setValue(b)
}

// ping does nothing. The coordinator calls this method to ensure the worker
// has called F.Fuzz and can communicate.
func (ws *workerServer) ping(ctx context.Context, args pingArgs) pingResponse {
	return pingResponse{}
}

// workerClient is a minimalist RPC client. The coordinator process uses a
// workerClient to call methods in each worker process (handled by
// workerServer).
type workerClient struct {
	workerComm
	mu sync.Mutex
	m  *mutator
}

func newWorkerClient(comm workerComm, m *mutator) *workerClient {
	return &workerClient{workerComm: comm, m: m}
}

// Close shuts down the connection to the RPC server (the worker process) by
// closing fuzz_in. Close drains fuzz_out (avoiding a SIGPIPE in the worker),
// and closes it after the worker process closes the other end.
func (wc *workerClient) Close() error {
	wc.mu.Lock()
	defer wc.mu.Unlock()

	// Close fuzzIn. This signals to the server that there are no more calls,
	// and it should exit.
	if err := wc.fuzzIn.Close(); err != nil {
		wc.fuzzOut.Close()
		return err
	}

	// Drain fuzzOut and close it. When the server exits, the kernel will close
	// its end of fuzzOut, and we'll get EOF.
	if _, err := io.Copy(ioutil.Discard, wc.fuzzOut); err != nil {
		wc.fuzzOut.Close()
		return err
	}
	return wc.fuzzOut.Close()
}

// errSharedMemClosed is returned by workerClient methods that cannot access
// shared memory because it was closed and unmapped by another goroutine. That
// can happen when worker.cleanup is called in the worker goroutine while a
// workerClient.fuzz call runs concurrently.
//
// This error should not be reported. It indicates the operation was
// interrupted.
var errSharedMemClosed = errors.New("internal error: shared memory was closed and unmapped")

// minimize tells the worker to call the minimize method. See
// workerServer.minimize.
func (wc *workerClient) minimize(ctx context.Context, entryIn CorpusEntry, args minimizeArgs) (entryOut CorpusEntry, resp minimizeResponse, err error) {
	wc.mu.Lock()
	defer wc.mu.Unlock()

	mem, ok := <-wc.memMu
	if !ok {
		return CorpusEntry{}, minimizeResponse{}, errSharedMemClosed
	}
	mem.header().count = 0
	inp, err := CorpusEntryData(entryIn)
	if err != nil {
		return CorpusEntry{}, minimizeResponse{}, err
	}
	mem.setValue(inp)
	wc.memMu <- mem

	c := call{Minimize: &args}
	callErr := wc.callLocked(ctx, c, &resp)
	mem, ok = <-wc.memMu
	if !ok {
		return CorpusEntry{}, minimizeResponse{}, errSharedMemClosed
	}
	defer func() { wc.memMu <- mem }()
	resp.Count = mem.header().count
	if resp.Success {
		entryOut.Data = mem.valueCopy()
		entryOut.Values, err = unmarshalCorpusFile(entryOut.Data)
		h := sha256.Sum256(entryOut.Data)
		name := fmt.Sprintf("%x", h[:4])
		entryOut.Path = name
		entryOut.Parent = entryIn.Parent
		entryOut.Generation = entryIn.Generation
		if err != nil {
			panic(fmt.Sprintf("workerClient.minimize unmarshaling minimized value: %v", err))
		}
	} else {
		// Did not minimize, but the original input may still be interesting,
		// for example, if there was an error.
		entryOut = entryIn
	}

	return entryOut, resp, callErr
}

// fuzz tells the worker to call the fuzz method. See workerServer.fuzz.
func (wc *workerClient) fuzz(ctx context.Context, entryIn CorpusEntry, args fuzzArgs) (entryOut CorpusEntry, resp fuzzResponse, err error) {
	wc.mu.Lock()
	defer wc.mu.Unlock()

	mem, ok := <-wc.memMu
	if !ok {
		return CorpusEntry{}, fuzzResponse{}, errSharedMemClosed
	}
	mem.header().count = 0
	inp, err := CorpusEntryData(entryIn)
	if err != nil {
		return CorpusEntry{}, fuzzResponse{}, err
	}
	mem.setValue(inp)
	wc.memMu <- mem

	c := call{Fuzz: &args}
	callErr := wc.callLocked(ctx, c, &resp)
	mem, ok = <-wc.memMu
	if !ok {
		return CorpusEntry{}, fuzzResponse{}, errSharedMemClosed
	}
	defer func() { wc.memMu <- mem }()
	resp.Count = mem.header().count

	if !bytes.Equal(inp, mem.valueRef()) {
		panic("workerServer.fuzz modified input")
	}
	needEntryOut := callErr != nil || resp.Err != "" ||
		(!args.Warmup && resp.CoverageData != nil)
	if needEntryOut {
		valuesOut, err := unmarshalCorpusFile(inp)
		if err != nil {
			panic(fmt.Sprintf("unmarshaling fuzz input value after call: %v", err))
		}
		wc.m.r.restore(mem.header().randState, mem.header().randInc)
		if !args.Warmup {
			// Only mutate the valuesOut if fuzzing actually occurred.
			for i := int64(0); i < mem.header().count; i++ {
				wc.m.mutate(valuesOut, cap(mem.valueRef()))
			}
		}
		dataOut := marshalCorpusFile(valuesOut...)

		h := sha256.Sum256(dataOut)
		name := fmt.Sprintf("%x", h[:4])
		entryOut = CorpusEntry{
			Parent:     entryIn.Path,
			Path:       name,
			Data:       dataOut,
			Generation: entryIn.Generation + 1,
		}
		if args.Warmup {
			// The bytes weren't mutated, so if entryIn was a seed corpus value,
			// then entryOut is too.
			entryOut.IsSeed = entryIn.IsSeed
		}
	}

	return entryOut, resp, callErr
}

// ping tells the worker to call the ping method. See workerServer.ping.
func (wc *workerClient) ping(ctx context.Context) error {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	c := call{Ping: &pingArgs{}}
	var resp pingResponse
	return wc.callLocked(ctx, c, &resp)
}

// callLocked sends an RPC from the coordinator to the worker process and waits
// for the response. The callLocked may be cancelled with ctx.
func (wc *workerClient) callLocked(ctx context.Context, c call, resp interface{}) (err error) {
	enc := json.NewEncoder(wc.fuzzIn)
	dec := json.NewDecoder(&contextReader{ctx: ctx, r: wc.fuzzOut})
	if err := enc.Encode(c); err != nil {
		return err
	}
	return dec.Decode(resp)
}

// contextReader wraps a Reader with a Context. If the context is cancelled
// while the underlying reader is blocked, Read returns immediately.
//
// This is useful for reading from a pipe. Closing a pipe file descriptor does
// not unblock pending Reads on that file descriptor. All copies of the pipe's
// other file descriptor (the write end) must be closed in all processes that
// inherit it. This is difficult to do correctly in the situation we care about
// (process group termination).
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *contextReader) Read(b []byte) (int, error) {
	if ctxErr := cr.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	done := make(chan struct{})

	// This goroutine may stay blocked after Read returns because the underlying
	// read is blocked.
	var n int
	var err error
	go func() {
		n, err = cr.r.Read(b)
		close(done)
	}()

	select {
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	case <-done:
		return n, err
	}
}
