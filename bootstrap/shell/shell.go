package shell

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buildkite/agent/v3/env"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/process"
	"github.com/buildkite/shellwords"
	"github.com/nightlyone/lockfile"
	"github.com/pkg/errors"
)

var (
	lockRetryDuration = time.Second
)

// Shell represents a virtual shell, handles logging, executing commands and
// provides hooks for capturing output and exit conditions.
//
// Provides a lowest-common denominator abstraction over macOS, Linux and Windows
type Shell struct {
	Logger

	// The running environment for the shell
	Env *env.Environment

	// Whether the shell is a PTY
	PTY bool

	// Where stdout is written, defaults to os.Stdout
	Writer io.Writer

	// Whether to run the shell in debug mode
	Debug bool

	// Current working directory that shell commands get executed in
	wd string

	// The context for the shell
	ctx context.Context

	// Currently running command
	cmd     *command
	cmdLock sync.Mutex
}

// New returns a new Shell
func New() (*Shell, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to find current working directory")
	}

	return &Shell{
		Logger: StderrLogger,
		Env:    env.FromSlice(os.Environ()),
		Writer: os.Stdout,
		wd:     wd,
		ctx:    context.Background(),
	}, nil
}

// New returns a new Shell with provided context.Context
func NewWithContext(ctx context.Context) (*Shell, error) {
	sh, err := New()
	if err != nil {
		return nil, err
	}

	sh.ctx = ctx
	return sh, nil
}

// Getwd returns the current working directory of the shell
func (s *Shell) Getwd() string {
	return s.wd
}

// Chdir changes the working directory of the shell
func (s *Shell) Chdir(path string) error {
	// If the path isn't absolute, prefix it with the current working directory.
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.wd, path)
	}

	s.Promptf("cd %s", shellwords.Quote(path))

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("Failed to change working: directory does not exist")
	}

	s.wd = path
	return nil
}

// AbsolutePath returns the absolute path to an executable based on the PATH and
// PATHEXT of the Shell
func (s *Shell) AbsolutePath(executable string) (string, error) {
	// Is the path already absolute?
	if path.IsAbs(executable) {
		return executable, nil
	}

	envPath, _ := s.Env.Get("PATH")
	fileExtensions, _ := s.Env.Get("PATHEXT") // For searching .exe, .bat, etc on Windows

	// Use our custom lookPath that takes a specific path
	absolutePath, err := LookPath(executable, envPath, fileExtensions)
	if err != nil {
		return "", err
	}

	// Since the path returned by LookPath is relative to the current working
	// directory, we need to get the absolute version of that.
	return filepath.Abs(absolutePath)
}

// Interrupt running command
func (s *Shell) Interrupt() {
	s.cmdLock.Lock()
	defer s.cmdLock.Unlock()

	if s.cmd != nil && s.cmd.proc != nil {
		s.cmd.proc.Interrupt()
	}
}

// Terminate running command
func (s *Shell) Terminate() {
	s.cmdLock.Lock()
	defer s.cmdLock.Unlock()

	if s.cmd != nil && s.cmd.proc != nil {
		s.cmd.proc.Terminate()
	}
}

// LockFile is a pid-based lock for cross-process locking
type LockFile interface {
	Unlock() error
}

// Create a cross-process file-based lock based on pid files
func (s *Shell) LockFile(path string, timeout time.Duration) (LockFile, error) {
	absolutePathToLock, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("Failed to find absolute path to lock \"%s\" (%v)", path, err)
	}

	lock, err := lockfile.New(absolutePathToLock)
	if err != nil {
		return nil, fmt.Errorf("Failed to create lock \"%s\" (%s)", absolutePathToLock, err)
	}

	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	for {
		// Keep trying the lock until we get it
		if err := lock.TryLock(); err != nil {
			s.Commentf("Could not acquire lock on \"%s\" (%s)", absolutePathToLock, err)
			s.Commentf("Trying again in %s...", lockRetryDuration)
			time.Sleep(lockRetryDuration)
		} else {
			break
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			// No value ready, moving on
		}
	}

	return &lock, err
}

// Run runs a command, write stdout and stderr to the logger and return an error
// if it fails
func (s *Shell) Run(command string, arg ...string) error {
	s.Promptf("%s", process.FormatCommand(command, arg))

	return s.RunWithoutPrompt(command, arg...)
}

// RunWithoutPrompt runs a command, write stdout and stderr to the logger and
// return an error if it fails. Notably it doesn't show a prompt.
func (s *Shell) RunWithoutPrompt(command string, arg ...string) error {
	cmd, err := s.buildCommand(command, arg...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return err
	}

	return s.executeCommand(cmd, s.Writer, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

// RunAndCapture runs a command and captures the output for processing. Stdout is captured, but
// stderr isn't. If the shell is in debug mode then the command will be eched and both stderr
// and stdout will be written to the logger. A PTY is never used for RunAndCapture.
func (s *Shell) RunAndCapture(command string, arg ...string) (string, error) {
	if s.Debug {
		s.Promptf("%s", process.FormatCommand(command, arg))
	}

	cmd, err := s.buildCommand(command, arg...)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer

	err = s.executeCommand(cmd, &b, executeFlags{
		Stdout: true,
		Stderr: false,
		PTY:    false,
	})
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(b.String()), nil
}

// RunScript is like Run, but the target is an interpreted script which has
// some extra checks to ensure it gets to the correct interpreter. Extra environment vars
// can also be passed the the script
func (s *Shell) RunScript(path string, extra *env.Environment) error {
	var command string
	var args []string

	// we apply a variety of "feature detection checks" to figure out how we should
	// best run the script

	var isBash = filepath.Ext(path) == "" || filepath.Ext(path) == ".sh"
	var isWindows = runtime.GOOS == "windows"
	var isPwsh = filepath.Ext(path) == ".ps1"

	switch {
	case isWindows && isBash:
		if s.Debug {
			s.Commentf("Attempting to run %s with Bash for Windows", path)
		}
		// Find Bash, either part of Cygwin or MSYS. Must be in the path
		bashPath, err := s.AbsolutePath("bash.exe")
		if err != nil {
			return fmt.Errorf("Error finding bash.exe, needed to run scripts: %v. "+
				"Is Git for Windows installed and correctly in your PATH variable?", err)
		}
		command = bashPath
		args = []string{"-c", filepath.ToSlash(path)}

	case isWindows && isPwsh:
		if s.Debug {
			s.Commentf("Attempting to run %s with Powershell", path)
		}
		command = "powershell.exe"
		args = []string{"-file", path}

	case !isWindows && isBash:
		command = "/bin/sh"
		args = []string{"-c", path}

	default:
		command = path
		args = []string{}
	}

	cmd, err := s.buildCommand(command, args...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return err
	}

	// Combine the two slices of env, let the latter overwrite the former
	currentEnv := env.FromSlice(cmd.Env)
	customEnv := currentEnv.Merge(extra)
	cmd.Env = customEnv.ToSlice()

	return s.executeCommand(cmd, s.Writer, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

type command struct {
	process.Config
	proc   *process.Process
	cancel context.CancelFunc
}

// buildCommand returns a command that can later be executed
func (s *Shell) buildCommand(name string, arg ...string) (*command, error) {
	// Always use absolute path as Windows has a hard time
	// finding executables in its path
	absPath, err := s.AbsolutePath(name)
	if err != nil {
		return nil, err
	}

	cfg := process.Config{
		Path: absPath,
		Args: arg,
		Env:  s.Env.ToSlice(),
		Dir:  s.wd,
	}

	// Create a sub-context so that shell.Cancel() can interrupt
	// a running command
	subctx, cancel := context.WithCancel(s.ctx)
	cfg.Context = subctx

	// Add env that commands expect a shell to set
	cfg.Env = append(cfg.Env,
		`PWD=`+s.wd,
	)

	return &command{Config: cfg, cancel: cancel}, nil
}

type executeFlags struct {
	// Whether to capture stdout
	Stdout bool

	// Whether to capture stderr
	Stderr bool

	// Run the command in a PTY
	PTY bool
}

func (s *Shell) executeCommand(cmd *command, w io.Writer, flags executeFlags) error {
	s.cmdLock.Lock()
	s.cmd = cmd
	s.cmdLock.Unlock()

	cmdStr := process.FormatCommand(cmd.Path, cmd.Args)

	if s.Debug {
		t := time.Now()
		defer func() {
			s.Commentf("↳ Command completed in %v", time.Now().Sub(t))
		}()
	}

	// Must cancel the context regardless of outcome
	defer func() {
		cmd.cancel()
	}()

	cfg := cmd.Config

	// Modify process config based on execution flags
	if flags.PTY {
		cfg.PTY = true
		cfg.Stdout = w
	} else {
		// Show stdout if requested or via debug
		if flags.Stdout {
			cfg.Stdout = w
		} else if s.Debug {
			stdOutStreamer := NewLoggerStreamer(s.Logger)
			defer stdOutStreamer.Close()
			cfg.Stdout = stdOutStreamer
		}

		// Show stderr if requested or via debug
		if flags.Stderr {
			cfg.Stderr = w
		} else if s.Debug {
			stdErrStreamer := NewLoggerStreamer(s.Logger)
			defer stdErrStreamer.Close()
			cfg.Stderr = stdErrStreamer
		}
	}

	p := process.New(logger.Discard, cfg)

	s.cmdLock.Lock()
	s.cmd.proc = p
	s.cmdLock.Unlock()

	if err := p.Run(); err != nil {
		return errors.Wrapf(err, "Error running `%s`", cmdStr)
	}

	return p.WaitResult()
}

// GetExitCode extracts an exit code from an error where the platform supports it,
// otherwise returns 0 for no error and 1 for an error
func GetExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch cause := errors.Cause(err).(type) {
	case *ExitError:
		return cause.Code

	case *exec.ExitError:
		// The program has exited with an exit code != 0
		// There is no platform independent way to retrieve
		// the exit code, but the following will work on Unix/macOS
		if status, ok := cause.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return 1
}

// IsExitSignaled returns true if the error is an ExitError that was
// caused by receiving a signal
func IsExitSignaled(err error) bool {
	if err == nil {
		return false
	}
	if exitErr, ok := errors.Cause(err).(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.Signaled()
		}
	}
	return false
}

func IsExitError(err error) bool {
	switch errors.Cause(err).(type) {
	case *ExitError:
		return true
	case *exec.ExitError:
		return true
	}
	return false
}

// ExitError is an error that carries a shell exit code
type ExitError struct {
	Code    int
	Message string
}

// Error returns the string message and fulfils the error interface
func (ee *ExitError) Error() string {
	return ee.Message
}
