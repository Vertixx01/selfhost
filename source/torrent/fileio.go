package torrent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
)

// FileIOEnv selects anacrolix's storage file IO. Its default, mmap, maps every
// downloaded file and never unmaps it: on Windows the file can't be moved or
// deleted, and on Linux a deleted multi-GB episode keeps using disk until the
// process exits. "classic" uses plain reads/writes that close their handles.
//
// anacrolix reads the variable in a package init(), before main runs, so it
// can't be set from Go code; EnsureClassicFileIO re-launches the process with
// it set instead. (The Docker image sets it directly and never re-launches.)
const FileIOEnv = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// EnsureClassicFileIO returns immediately if FileIOEnv=classic. Otherwise it
// runs the current executable again with the variable set, forwards
// SIGINT/SIGTERM to it, and exits with its exit code — it never returns.
func EnsureClassicFileIO() {
	switch v := os.Getenv(FileIOEnv); v {
	case "classic":
		return
	case "":
	default:
		fmt.Fprintf(os.Stderr, "%s=%s is not supported; torClient needs %s=classic\n", FileIOEnv, v, FileIOEnv)
		os.Exit(2)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find own executable to re-launch with %s=classic: %v\n", FileIOEnv, err)
		os.Exit(2)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), FileIOEnv+"=classic")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "re-launch with %s=classic: %v\n", FileIOEnv, err)
		os.Exit(2)
	}
	go func() {
		for sig := range sigs {
			// On Windows the console already delivers Ctrl+C to the child,
			// and Process.Signal can't send it anyway.
			if runtime.GOOS != "windows" {
				cmd.Process.Signal(sig)
			}
		}
	}()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
