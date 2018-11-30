package executor

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/armon/circbuf"
	hclog "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/client/stats"
	shelpers "github.com/hashicorp/nomad/helper/stats"

	"github.com/hashicorp/consul-template/signals"
	cstructs "github.com/hashicorp/nomad/client/structs"
)

const (
	// ExecutorVersionLatest is the current and latest version of the executor
	ExecutorVersionLatest = "2.0.0"

	// ExecutorVersionPre0_9 is the version of executor use prior to the release
	// of 0.9.x
	ExecutorVersionPre0_9 = "1.1.0"
)

var (
	// The statistics the basic executor exposes
	ExecutorBasicMeasuredMemStats = []string{"RSS", "Swap"}
	ExecutorBasicMeasuredCpuStats = []string{"System Mode", "User Mode", "Percent"}
)

// Executor is the interface which allows a driver to launch and supervise
// a process
type Executor interface {
	// Launch a user process configured by the given ExecCommand
	Launch(launchCmd *ExecCommand) (*ProcessState, error)

	// Wait blocks until the process exits or an error occures
	Wait() (*ProcessState, error)

	// Shutdown will shutdown the executor by stopping the user process,
	// cleaning up and resources created by the executor. The shutdown sequence
	// will first send the given signal to the process. This defaults to "SIGINT"
	// if not specified. The executor will then wait for the process to exit
	// before cleaning up other resources. If the executor waits longer than the
	// given grace period, the process is forcefully killed.
	//
	// To force kill the user process, gracePeriod can be set to 0.
	Shutdown(signal string, gracePeriod time.Duration) error

	// UpdateResources updates any resource isolation enforcement with new
	// constraints if supported.
	UpdateResources(*Resources) error

	// Version returns the executor API version
	Version() (*ExecutorVersion, error)

	// Stats fetchs process usage stats for the executor and each pid if available
	Stats() (*cstructs.TaskResourceUsage, error)

	// Signal sends the given signal to the user process
	Signal(os.Signal) error

	// Exec executes the given command and args inside the executor context
	// and returns the output and exit code.
	Exec(deadline time.Time, cmd string, args []string) ([]byte, int, error)
}

// Resources describes the resource isolation required
type Resources struct {
	CPU      int
	MemoryMB int
	DiskMB   int
	IOPS     int
}

// ExecCommand holds the user command, args, and other isolation related
// settings.
type ExecCommand struct {
	// Cmd is the command that the user wants to run.
	Cmd string

	// Args is the args of the command that the user wants to run.
	Args []string

	// Resources defined by the task
	Resources *Resources

	// StdoutPath is the path the procoess stdout should be written to
	StdoutPath string
	stdout     io.WriteCloser

	// StderrPath is the path the procoess stderr should be written to
	StderrPath string
	stderr     io.WriteCloser

	// Env is the list of KEY=val pairs of environment variables to be set
	Env []string

	// User is the user which the executor uses to run the command.
	User string

	// TaskDir is the directory path on the host where for the task
	TaskDir string

	// ResourceLimits determines whether resource limits are enforced by the
	// executor.
	ResourceLimits bool

	// Cgroup marks whether we put the process in a cgroup. Setting this field
	// doesn't enforce resource limits. To enforce limits, set ResourceLimits.
	// Using the cgroup does allow more precise cleanup of processes.
	BasicProcessCgroup bool
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// Stdout returns a writer for the configured file descriptor
func (c *ExecCommand) Stdout() (io.WriteCloser, error) {
	if c.stdout == nil {
		if c.StdoutPath != "" {
			f, err := fifo.Open(c.StdoutPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create stdout: %v", err)
			}
			c.stdout = f
		} else {
			c.stdout = nopCloser{ioutil.Discard}
		}
	}
	return c.stdout, nil
}

// Stderr returns a writer for the configured file descriptor
func (c *ExecCommand) Stderr() (io.WriteCloser, error) {
	if c.stderr == nil {
		if c.StderrPath != "" {
			f, err := fifo.Open(c.StderrPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create stderr: %v", err)
			}
			c.stderr = f
		} else {
			c.stderr = nopCloser{ioutil.Discard}
		}
	}
	return c.stderr, nil
}

func (c *ExecCommand) Close() {
	stdout, err := c.Stdout()
	if err == nil {
		stdout.Close()
	}
	stderr, err := c.Stderr()
	if err == nil {
		stderr.Close()
	}
}

// ProcessState holds information about the state of a user process.
type ProcessState struct {
	Pid      int
	ExitCode int
	Signal   int
	Time     time.Time
}

// ExecutorVersion is the version of the executor
type ExecutorVersion struct {
	Version string
}

func (v *ExecutorVersion) GoString() string {
	return v.Version
}

// UniversalExecutor is an implementation of the Executor which launches and
// supervises processes. In addition to process supervision it provides resource
// and file system isolation
type UniversalExecutor struct {
	childCmd   exec.Cmd
	commandCfg *ExecCommand

	exitState     *ProcessState
	processExited chan interface{}

	// resConCtx is used to track and cleanup additional resources created by
	// the executor. Currently this is only used for cgroups.
	resConCtx resourceContainerContext

	totalCpuStats  *stats.CpuStats
	userCpuStats   *stats.CpuStats
	systemCpuStats *stats.CpuStats
	pidCollector   *pidCollector

	logger hclog.Logger
}

// NewExecutor returns an Executor
func NewExecutor(logger hclog.Logger) Executor {
	logger = logger.Named("executor")
	if err := shelpers.Init(); err != nil {
		logger.Error("unable to initialize stats", "error", err)
	}
	return &UniversalExecutor{
		logger:         logger,
		processExited:  make(chan interface{}),
		totalCpuStats:  stats.NewCpuStats(),
		userCpuStats:   stats.NewCpuStats(),
		systemCpuStats: stats.NewCpuStats(),
		pidCollector:   newPidCollector(logger),
	}
}

// Version returns the api version of the executor
func (e *UniversalExecutor) Version() (*ExecutorVersion, error) {
	return &ExecutorVersion{Version: ExecutorVersionLatest}, nil
}

// Launch launches the main process and returns its state. It also
// configures an applies isolation on certain platforms.
func (e *UniversalExecutor) Launch(command *ExecCommand) (*ProcessState, error) {
	e.logger.Info("launching command", "command", command.Cmd, "args", strings.Join(command.Args, " "))

	e.commandCfg = command

	// setting the user of the process
	if command.User != "" {
		e.logger.Debug("running command as user", "user", command.User)
		if err := e.runAs(command.User); err != nil {
			return nil, err
		}
	}

	// set the task dir as the working directory for the command
	e.childCmd.Dir = e.commandCfg.TaskDir

	// start command in separate process group
	if err := e.setNewProcessGroup(); err != nil {
		return nil, err
	}

	// Setup cgroups on linux
	if err := e.configureResourceContainer(os.Getpid()); err != nil {
		return nil, err
	}

	stdout, err := e.commandCfg.Stdout()
	if err != nil {
		return nil, err
	}
	stderr, err := e.commandCfg.Stderr()
	if err != nil {
		return nil, err
	}

	e.childCmd.Stdout = stdout
	e.childCmd.Stderr = stderr

	// Look up the binary path and make it executable
	absPath, err := lookupBin(command.TaskDir, command.Cmd)
	if err != nil {
		return nil, err
	}

	if err := makeExecutable(absPath); err != nil {
		return nil, err
	}

	path := absPath

	// Set the commands arguments
	e.childCmd.Path = path
	e.childCmd.Args = append([]string{e.childCmd.Path}, command.Args...)
	e.childCmd.Env = e.commandCfg.Env

	// Start the process
	if err := e.childCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command path=%q --- args=%q: %v", path, e.childCmd.Args, err)
	}

	go e.pidCollector.collectPids(e.processExited, getAllPids)
	go e.wait()
	return &ProcessState{Pid: e.childCmd.Process.Pid, ExitCode: -1, Time: time.Now()}, nil
}

// Exec a command inside a container for exec and java drivers.
func (e *UniversalExecutor) Exec(deadline time.Time, name string, args []string) ([]byte, int, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return ExecScript(ctx, e.childCmd.Dir, e.commandCfg.Env, e.childCmd.SysProcAttr, name, args)
}

// ExecScript executes cmd with args and returns the output, exit code, and
// error. Output is truncated to drivers/shared/structs.CheckBufSize
func ExecScript(ctx context.Context, dir string, env []string, attrs *syscall.SysProcAttr,
	name string, args []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	// Copy runtime environment from the main command
	cmd.SysProcAttr = attrs
	cmd.Dir = dir
	cmd.Env = env

	// Capture output
	buf, _ := circbuf.NewBuffer(int64(cstructs.CheckBufSize))
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			// Non-exit error, return it and let the caller treat
			// it as a critical failure
			return nil, 0, err
		}

		// Some kind of error happened; default to critical
		exitCode := 2
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			exitCode = status.ExitStatus()
		}

		// Don't return the exitError as the caller only needs the
		// output and code.
		return buf.Bytes(), exitCode, nil
	}
	return buf.Bytes(), 0, nil
}

// Wait waits until a process has exited and returns it's exitcode and errors
func (e *UniversalExecutor) Wait() (*ProcessState, error) {
	<-e.processExited
	return e.exitState, nil
}

func (e *UniversalExecutor) UpdateResources(resources *Resources) error {
	return nil
}

func (e *UniversalExecutor) wait() {
	defer close(e.processExited)
	pid := e.childCmd.Process.Pid
	err := e.childCmd.Wait()
	if err == nil {
		e.exitState = &ProcessState{Pid: pid, ExitCode: 0, Time: time.Now()}
		return
	}

	e.commandCfg.Close()

	exitCode := 1
	var signal int
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			exitCode = status.ExitStatus()
			if status.Signaled() {
				// bash(1) uses the lower 7 bits of a uint8
				// to indicate normal program failure (see
				// <sysexits.h>). If a process terminates due
				// to a signal, encode the signal number to
				// indicate which signal caused the process
				// to terminate.  Mirror this exit code
				// encoding scheme.
				const exitSignalBase = 128
				signal = int(status.Signal())
				exitCode = exitSignalBase + signal
			}
		}
	} else {
		e.logger.Warn("unexpected Cmd.Wait() error type", "error", err)
	}

	e.exitState = &ProcessState{Pid: pid, ExitCode: exitCode, Signal: signal, Time: time.Now()}
}

var (
	// finishedErr is the error message received when trying to kill and already
	// exited process.
	finishedErr = "os: process already finished"

	// noSuchProcessErr is the error message received when trying to kill a non
	// existing process (e.g. when killing a process group).
	noSuchProcessErr = "no such process"
)

// Exit cleans up the alloc directory, destroys resource container and kills the
// user process
func (e *UniversalExecutor) Shutdown(signal string, grace time.Duration) error {
	e.logger.Info("shutdown requested", "signal", signal, "grace_period_ms", grace.Round(time.Millisecond))
	var merr multierror.Error

	// If the executor did not launch a process, return.
	if e.commandCfg == nil {
		return nil
	}

	// If there is no process we can't shutdown
	if e.childCmd.Process == nil {
		e.logger.Warn("failed to shutdown", "error", "no process found")
		return fmt.Errorf("executor failed to shutdown error: no process found")
	}

	proc, err := os.FindProcess(e.childCmd.Process.Pid)
	if err != nil {
		err = fmt.Errorf("executor failed to find process: %v", err)
		e.logger.Warn("failed to shutdown", "error", err)
		return err
	}

	// If grace is 0 then skip shutdown logic
	if grace > 0 {
		// Default signal to SIGINT if not set
		if signal == "" {
			signal = "SIGINT"
		}

		sig, ok := signals.SignalLookup[signal]
		if !ok {
			err = fmt.Errorf("error unknown signal given for shutdown: %s", signal)
			e.logger.Warn("failed to shutdown", "error", err)
			return err
		}

		if err := e.shutdownProcess(sig, proc); err != nil {
			e.logger.Warn("failed to shutdown", "error", err)
			return err
		}

		select {
		case <-e.processExited:
		case <-time.After(grace):
			proc.Kill()
		}
	} else {
		proc.Kill()
	}

	// Prefer killing the process via the resource container.
	if !(e.commandCfg.ResourceLimits || e.commandCfg.BasicProcessCgroup) {
		if err := e.cleanupChildProcesses(proc); err != nil && err.Error() != finishedErr {
			merr.Errors = append(merr.Errors,
				fmt.Errorf("can't kill process with pid %d: %v", e.childCmd.Process.Pid, err))
		}
	}

	if e.commandCfg.ResourceLimits || e.commandCfg.BasicProcessCgroup {
		if err := e.resConCtx.executorCleanup(); err != nil {
			merr.Errors = append(merr.Errors, err)
		}
	}

	if err := merr.ErrorOrNil(); err != nil {
		e.logger.Warn("failed to shutdown", "error", err)
		return err
	}

	return nil
}

// Signal sends the passed signal to the task
func (e *UniversalExecutor) Signal(s os.Signal) error {
	if e.childCmd.Process == nil {
		return fmt.Errorf("Task not yet run")
	}

	e.logger.Debug("sending signal to PID", "signal", s, "pid", e.childCmd.Process.Pid)
	err := e.childCmd.Process.Signal(s)
	if err != nil {
		e.logger.Error("sending signal failed", "signal", s, "error", err)
		return err
	}

	return nil
}

func (e *UniversalExecutor) Stats() (*cstructs.TaskResourceUsage, error) {
	pidStats, err := e.pidCollector.pidStats()
	if err != nil {
		return nil, err
	}
	return aggregatedResourceUsage(e.systemCpuStats, pidStats), nil
}

// lookupBin looks for path to the binary to run by looking for the binary in
// the following locations, in-order: task/local/, task/, based on host $PATH.
// The return path is absolute.
func lookupBin(taskDir string, bin string) (string, error) {
	// Check the binary path first
	// This handles the case where the job spec sends a fully interpolated path to the binary
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	// Check in the local directory
	local := filepath.Join(taskDir, allocdir.TaskLocal, bin)
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}

	// Check at the root of the task's directory
	root := filepath.Join(taskDir, bin)
	if _, err := os.Stat(root); err == nil {
		return root, nil
	}

	// Check the $PATH
	if host, err := exec.LookPath(bin); err == nil {
		return host, nil
	}

	return "", fmt.Errorf("binary %q could not be found", bin)
}

// makeExecutable makes the given file executable for root,group,others.
func makeExecutable(binPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	fi, err := os.Stat(binPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("binary %q does not exist", binPath)
		}
		return fmt.Errorf("specified binary is invalid: %v", err)
	}

	// If it is not executable, make it so.
	perm := fi.Mode().Perm()
	req := os.FileMode(0555)
	if perm&req != req {
		if err := os.Chmod(binPath, perm|req); err != nil {
			return fmt.Errorf("error making %q executable: %s", binPath, err)
		}
	}
	return nil
}
