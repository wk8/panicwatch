package panicwatch_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"testing"

	goerrors "github.com/go-errors/errors"
	"github.com/grongor/panicwatch"
	"github.com/stretchr/testify/require"
)

const panicRegexTemplate = `goroutine 1 \[runn(?:ing|able)\]:
main\.executeCommand\({?0x[a-z0-9]+\??, 0x[a-z0-9]+\??}?\)
\t%[1]s/cmd/test/test\.go:\d+ \+0x[a-z0-9]+
main.main\(\)
\t%[1]s/cmd/test/test\.go:\d+ \+0x[a-z0-9]+
`

func TestPanicwatch(t *testing.T) {
	builder := strings.Builder{}

	for i := 0; i < 1500; i++ {
		builder.WriteString("some garbage here...")
		builder.WriteString("\n")
	}

	garbageString := builder.String()

	panicRegex := getPanicRegex()

	tests := []struct {
		command        string
		expectedStdout string
		expectedStderr string
		expectedPanic  string
		// expectedPanicType defaults to panicwatch.TypePanic if empty and expectedPanic is not empty
		expectedPanicType panicwatch.PanicType
		expectedExitCode  int
		// if true, the test won't attempt to check the stacktrace
		// this comes in handy for tests that involve several routines, any of which can cause the crash
		nonDeterministicStacktrace bool
		// if set, the test program logs certain steps to a log file, and those are the expected contents
		logFile string
	}{
		{
			command:        "no-panic",
			expectedStdout: "some stdout output\n",
			expectedStderr: "some stderr output\n",
		},
		{
			command:          "no-panic-error",
			expectedExitCode: 1,
			expectedStderr:   "blah blah something happened\n",
		},
		{
			command:          "panic",
			expectedExitCode: 2,
			expectedStdout:   "some output...\neverything looks good...\n",
			expectedPanic:    "wtf, unexpected panic!",
		},
		{
			command:          "panic-and-error",
			expectedExitCode: 2,
			expectedStdout:   "some output...\neverything looks good...\n",
			expectedStderr:   "well something goes bad ...\n",
			expectedPanic:    "... and panic!",
		},
		{
			command:          "panic-sync-split",
			expectedExitCode: 2,
			expectedPanic:    "i'm split in three lol",
		},
		{
			command:          "panic-with-garbage",
			expectedExitCode: 2,
			expectedStdout:   garbageString,
			expectedStderr:   "panic: blah blah\n\n" + garbageString,
			expectedPanic:    "and BAM!",
		},
		{
			command:          "only-last-panic-string-is-detected",
			expectedExitCode: 2,
			expectedStderr:   "panic: this is fake\n\n",
			expectedPanic:    "and this is not",
		},
		{
			command:                    "fatal",
			expectedExitCode:           2,
			expectedPanicType:          panicwatch.TypeFatalError,
			expectedPanic:              "concurrent map writes",
			nonDeterministicStacktrace: true,
		},
		{
			command:          "wait-for-watcher",
			expectedExitCode: 2,
			expectedPanic:    "panic right after starting panicwatch but some time after starting the program",
			logFile:          "[MAIN] starting\n[WATCHER] starting\n[MAIN] started\n",
		},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			assert := require.New(t)

			var (
				logFilePath  string
				envVariables []string
			)
			if test.logFile != "" {
				logFile, err := os.CreateTemp("", "panicwatch-test-log")
				assert.NoError(err)

				logFilePath = logFile.Name()
				envVariables = append(envVariables, fmt.Sprintf("_PANICWATCH_TEST_LOG_FILE=%s", logFilePath))
				defer func() { assert.NoError(os.Remove(logFilePath)) }()
			}

			cmd, stdout, stderr, resultFile := helperProcess(test.command, envVariables...)
			defer func() { assert.NoError(os.Remove(resultFile)) }()

			err := cmd.Run()

			if test.logFile != "" {
				logs, err := os.ReadFile(logFilePath)
				assert.NoError(err)

				assert.Equal(test.logFile, string(logs))
			}

			if test.expectedExitCode == 0 {
				assert.NoError(err, "unexpected exit code, stderr: "+stderr.String())
			} else {
				assert.Error(err)

				var exitErr *exec.ExitError

				assert.True(errors.As(err, &exitErr))
				assert.Equal(
					test.expectedExitCode,
					exitErr.ExitCode(),
					"unexpected exit code, stderr: "+stderr.String(),
				)
			}

			assert.Equal(test.expectedStdout, stdout.String())

			result := readResult(resultFile)

			assert.Equal(test.expectedPanic, result.Message)

			if test.expectedPanic == "" {
				assert.Equal(test.expectedStderr, stderr.String())

				return
			}

			assert.Regexp(panicRegex, result.Stack)

			actualStderr := stderr.String()

			if test.expectedStderr != "" {
				assert.True(strings.HasPrefix(actualStderr, test.expectedStderr))
				actualStderr = strings.TrimPrefix(actualStderr, test.expectedStderr)
			}

			expectedPanicType := test.expectedPanicType
			if expectedPanicType == "" {
				expectedPanicType = panicwatch.TypePanic
			}
			assert.Equal(expectedPanicType, result.Type)

			expectedPanicStart := fmt.Sprintf("%s: %s\n\n", expectedPanicType, test.expectedPanic)
			assert.True(strings.HasPrefix(actualStderr, expectedPanicStart))
			actualStderr = strings.TrimPrefix(actualStderr, test.expectedStderr)

			assert.Regexp(panicRegex, actualStderr)

			var resultAsErr *goerrors.Error

			assert.True(errors.As(result.AsError(), &resultAsErr))

			assert.Equal(test.expectedPanic, resultAsErr.Error())

			if !test.nonDeterministicStacktrace {
				builder := strings.Builder{}

				builder.WriteString("goroutine 1 [running]:\n")

				for _, frame := range resultAsErr.StackFrames() {
					if frame.Name == "main" {
						builder.WriteString(frame.Package + `.` + frame.Name + `()` + "\n")
					} else {
						builder.WriteString(frame.Package + `.` + frame.Name + `(0x0, 0x0)` + "\n")
					}

					builder.WriteString("\t" + frame.File + ":" + strconv.Itoa(frame.LineNumber) + ` +0x0` + "\n")
				}

				assert.Regexp(panicRegex, builder.String())
			}
		})
	}
}

// Each test uses this test method to run a separate process in order to test the functionality.
func helperProcess(command string, envVariables ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, string) {
	f, err := os.CreateTemp("", "result")
	if err != nil {
		panic(err)
	}

	err = f.Close()
	if err != nil {
		panic(err)
	}

	cmd := exec.Command("./test", command, f.Name()) //nolint:gosec // we control the inputs
	cmd.Stderr = new(bytes.Buffer)
	cmd.Stdout = new(bytes.Buffer)
	if len(envVariables) != 0 {
		cmd.Env = append(os.Environ(), envVariables...)
	}

	return cmd, cmd.Stdout.(*bytes.Buffer), cmd.Stderr.(*bytes.Buffer), f.Name()
}

func getPanicRegex() string {
	_, filename, _, _ := runtime.Caller(0)
	dir := path.Dir(filename)

	return fmt.Sprintf(panicRegexTemplate, dir)
}

func readResult(resultFile string) panicwatch.Panic {
	resultBytes, err := os.ReadFile(resultFile)
	if err != nil {
		panic(err)
	}

	if len(resultBytes) == 0 {
		return panicwatch.Panic{}
	}

	result := panicwatch.Panic{}

	err = json.Unmarshal(resultBytes, &result)
	if err != nil {
		panic(err)
	}

	return result
}
