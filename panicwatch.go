// Package panicwatch guarantees you that you will never miss a panic. Use it to reliably log any unhandled panics
// that may occur in your application. This is completely transparent to your application, and it doesn't affect
// it in any way. All signal handling and file descriptor manipulation (either from inside or outside) is still under
// your control.
package panicwatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/glycerine/rbuf"
	goerrors "github.com/go-errors/errors"
)

// Panic holds information about a panic parsed from stderr of your application.
type Panic struct {
	Type    PanicType
	Message string
	Stack   string
}

type PanicType string

const (
	TypePanic      PanicType = "panic"
	TypeFatalError PanicType = "fatal error"
)

// AsError returns the Panic as an instance of error interface. When the panic message and stack aren't malformed,
// it will return *goerrors.Error, otherwise it will fall back to a simple *errors.errorString,
// containing just the message.
func (p Panic) AsError() error {
	// hard-coding "panic" and not necessarily the panic's type is actually what's needed here,
	// as goerrors.ParsePanic does expect its input to start with "panic: "
	// it still works for other types of errors though
	parsedErr, err := goerrors.ParsePanic("panic: " + p.Message + "\n" + p.Stack)
	if err != nil {
		return errors.New(p.Message)
	}

	return parsedErr
}

// Config holds the configuration of panicwatch.
type Config struct {
	// BufferSize specifies the size of the read buffer between dup-ed stderr and the real one. Optional.
	BufferSize int
	// PanicDetectorBufferSize specifies the size of the buffer used to detect panic.
	// Too low value will cause the detection to fail. Optional.
	PanicDetectorBufferSize int
	// WaitForWatcherToStartFor is how long the main process will wait for the watcher child process to be up & running.
	// Optional. If not set, the main process will not wait for the watcher child process before proceeding.
	// Not supported on Windows.
	WaitForWatcherToStartFor time.Duration
	// OnPanic is a callback that will be called after your application dies, if a panic is detected. Required.
	OnPanic func(Panic)
	// OnWatcherErr is a callback that will be called when watcher process encounters an error. Optional.
	OnWatcherError func(error)
	// OnWatcherDied is a callback that will be called when watcher process dies.
	// It is recommended to set this callback to shut down your application gracefully. Optional.
	OnWatcherDied func(error)
}

const (
	CookieName  = "XkqVuiPZaKYxS3f2lHoYDTNfBPYNT24woDplRI4Z"
	CookieValue = "zQXfl15CShjg5yQzEqoGAIgFeyXhlr9JQABuYCXm"
)

// Start validates panicwatch config, replaces the stderr file descriptor with a new one and starts a watcher process.
// This watcher process will read the original stderr and tee it into the replaced file descriptor. When the application
// exits, the watcher process will check if there was a panic in the original stderr. If yes, it will call the OnPanic
// callback. If the watcher process encounters an error or dies, then appropriate callback is called if configured.
// It returns the process object for the watcher process
func Start(config Config) (*os.Process, error) {
	if err := config.validateAndSetDefaults(); err != nil {
		return nil, err
	}

	if os.Getenv(CookieName) == CookieValue {
		runMonitoringProcess(config)
		panic("this never should've been executed")
	}

	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	originalStderrFd, err := dup(int(os.Stderr.Fd()))
	if err != nil {
		return nil, err
	}

	err = redirectStderr(stderrW)
	if err != nil {
		return nil, err
	}

	originalStderr := os.NewFile(uintptr(originalStderrFd), os.Stderr.Name())

	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), CookieName+"="+CookieValue)
	cmd.Stdin = stderrR
	cmd.Stdout = originalStderr

	var startedPipeR *os.File
	if config.WaitForWatcherToStartFor != 0 {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.ExtraFiles = []*os.File{w}
		startedPipeR = r
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	if config.WaitForWatcherToStartFor != 0 {
		monitorStartedCh := make(chan error)

		go func() {
			buf := make([]byte, 1)
			var err error
			for read := 0; read == 0; {
				read, err = startedPipeR.Read(buf)
				if err != nil {
					monitorStartedCh <- err
					return
				}
			}
			close(monitorStartedCh)
		}()

		select {
		case err := <-monitorStartedCh:
			if err != nil {
				return nil, err
			}
		case <-time.After(config.WaitForWatcherToStartFor):
			return nil, errors.New("timed out waiting for monitor to start")
		}
	}

	go func() {
		err := cmd.Wait()
		_ = redirectStderr(originalStderr)

		if config.OnWatcherDied == nil {
			log.Fatalln("panicwatch: watcher process died")
		}

		config.OnWatcherDied(err)
	}()

	return cmd.Process, nil
}

func (c *Config) validateAndSetDefaults() error {
	if c.OnPanic == nil {
		return errors.New("OnPanic callback must be set")
	}

	if c.BufferSize < 0 {
		return errors.New("BufferSize can't be less than zero")
	}

	if c.BufferSize == 0 {
		c.BufferSize = 1e5
	}

	if c.PanicDetectorBufferSize < 0 {
		return errors.New("PanicDetectorBufferSize can't be less than zero")
	}

	if c.WaitForWatcherToStartFor != 0 && runtime.GOOS == "windows" {
		return errors.New("WaitForWatcherToStartFor not supported on windows")
	}

	if c.PanicDetectorBufferSize == 0 {
		c.PanicDetectorBufferSize = 1e5
	}

	return nil
}

//nolint:gochecknoglobals
var panicTypes = []string{
	string(TypePanic),
	string(TypeFatalError),
}

const panicHeaderSuffix = ": "

func runMonitoringProcess(config Config) {
	signal.Ignore()

	if config.WaitForWatcherToStartFor != 0 {
		startedPipeR := os.NewFile(3, "startedPipeR")
		if _, err := startedPipeR.Write([]byte("1")); err != nil {
			panic(fmt.Sprintf("unable to notify main process that watcher has started: %v", err))
		}
	}

	readBuffer := make([]byte, config.BufferSize)
	buffer := rbuf.NewFixedSizeRingBuf(config.PanicDetectorBufferSize)
	reader := io.TeeReader(os.Stdin, os.Stdout)

	for {
		n, err := reader.Read(readBuffer)
		if n > 0 {
			_, _ = buffer.WriteAndMaybeOverwriteOldestData(readBuffer[:n])
		}

		if errors.Is(err, io.EOF) {
			bufferBytes := buffer.Bytes()

			index := findLastPanicStartIndex(bufferBytes)
			if index != -1 {
				if parsed := parsePanic(bufferBytes[index:]); parsed != nil {
					config.OnPanic(*parsed)
				}
			}

			os.Exit(0)
		}

		if err != nil {
			if config.OnWatcherError != nil {
				config.OnWatcherError(err)
			}

			os.Exit(1)
		}
	}
}

func findLastPanicStartIndex(b []byte) int {
	panicHeaders := make([][]byte, len(panicTypes))
	for i, panicType := range panicTypes {
		panicHeaders[i] = []byte(panicType + panicHeaderSuffix)
	}

	for {
		index := bytes.LastIndexByte(b, '\n')

		for _, panicHeader := range panicHeaders {
			if bytes.HasPrefix(b[index+1:], panicHeader) {
				return index + 1
			}
		}

		if index == -1 {
			return -1
		}

		b = b[:index]
	}
}

func parsePanic(raw []byte) *Panic {
	panicRegex := regexp.MustCompile(`(?sm)(` + strings.Join(panicTypes, "|") + ")" +
		panicHeaderSuffix + `(.*?$)?\n+(.*)\z`)

	matches := panicRegex.FindSubmatch(raw)
	if matches != nil {
		return &Panic{
			Type:    PanicType(matches[1]),
			Message: string(matches[2]),
			Stack:   string(matches[3]),
		}
	}

	return nil
}
