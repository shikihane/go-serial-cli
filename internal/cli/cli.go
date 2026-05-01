package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go-serial-cli/internal/serialcmd"
	"go-serial-cli/internal/session"
	workerpkg "go-serial-cli/internal/worker"
)

var ErrSessionPaused = errors.New("serial session is paused")

var ErrShellInputInterrupted = errors.New("shell input read interrupted")

const defaultTCPListenPort = "47017"

type App struct {
	deps AppDeps
}

type AppDeps struct {
	CommandName           string
	InstallSkill          func(source string, to string) error
	StartWorker           func(sessionName string) (int, error)
	StopProcess           func(pid int) error
	ReserveControlAddress func() (string, error)
	WaitForControl        func(address string) error
	CreateVirtualPorts    func(pairs []VirtualPortPair) error
	RemoveVirtualPorts    func(pairs []VirtualPortPair) error
	ClearSharePorts       func() error
	RunSetupC             func(args []string, output io.Writer) error
	AdminPause            func()
	BridgeTCP             func(opts serialcmd.TCPBridgeOptions) error
	RunSessionServer      func(opts serialcmd.SessionServerOptions) error
	RunShareBridge        func(opts serialcmd.ShareBridgeOptions) error
	Store                 session.Store
	ListPorts             func() ([]string, error)
	SendSerial            func(port string, baud int, data string) error
	AskSerial             func(opts serialcmd.AskOptions) error
	SendSession           func(address string, data string) error
	StreamSerial          func(opts serialcmd.StreamOptions) error
	StreamSession         func(address string, input io.Reader, output io.Writer) error
	IsProcessRunning      func(pid int) bool
	RetrySleep            func(delay time.Duration)
	Stdin                 io.Reader
	ShellInterrupts       <-chan os.Signal
	ConfigureShellConsole func() func()
	Version               VersionInfo
}

func New(deps AppDeps) *App {
	if deps.Version.IsZero() {
		deps.Version = DefaultVersionInfo()
	}
	return &App{deps: deps}
}

func (a *App) commandName() string {
	name := strings.TrimSpace(a.deps.CommandName)
	if name == "" {
		return "sio"
	}
	return name
}

func (a *App) usage(command string) error {
	return fmt.Errorf("usage: %s %s", a.commandName(), command)
}

func (a *App) pausedSessionError() error {
	return fmt.Errorf("%w; run %s resume first", ErrSessionPaused, a.commandName())
}

func (a *App) sharedControlUnavailable() error {
	return fmt.Errorf("shared session control channel is unavailable; run %s share <session> <virtual-port>... again", a.commandName())
}

func (a *App) Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		a.printHelp(out)
		return nil
	}

	switch args[0] {
	case "version", "-v", "--version":
		a.printVersion(out)
		return nil
	case "ports":
		return a.runPorts(out)
	case "open":
		return a.runOpen(args[1:], out)
	case "send":
		return a.runSend(args[1:], out)
	case "ask":
		return a.runAsk(args[1:], out)
	case "read":
		return a.runRead(args[1:], out)
	case "check":
		return a.runCheck(args[1:], out)
	case "clear":
		return a.runClear(args[1:], out)
	case "shell":
		return a.runShell(args[1:], out)
	case "tee":
		return a.runTee(args[1:], out)
	case "tcp":
		return a.runTCP(args[1:], out)
	case "share":
		return a.runShare(args[1:], out)
	case "pause":
		return a.runPause(args[1:], out)
	case "resume":
		return a.runResume(args[1:], out)
	case "status":
		return a.runStatus(args[1:], out)
	case "log":
		return a.runLog(args[1:], out)
	case "list":
		return a.runList(args[1:], out)
	case "stop":
		return a.runStop(args[1:], out)
	case "rm":
		return a.runRM(args[1:], out)
	case "skill":
		return a.runSkill(args[1:])
	case "tools":
		return a.runTools(args[1:], out)
	case "admin":
		return a.runAdmin(args[1:], out)
	case "worker":
		return a.runWorker(args[1:], out)
	case "help", "-h", "--help":
		a.printHelp(out)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a *App) runSkill(args []string) error {
	if len(args) < 1 || args[0] != "install" {
		return a.usage("skill install [source] [--to codex|claude|dir]")
	}

	source := ""
	to := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--to":
			if i+1 >= len(args) {
				return a.usage("skill install [source] [--to codex|claude|dir]")
			}
			to = args[i+1]
			i++
		default:
			if source != "" {
				return a.usage("skill install [source] [--to codex|claude|dir]")
			}
			source = args[i]
		}
	}
	if a.deps.InstallSkill == nil {
		return errors.New("skill installer is unavailable")
	}
	return a.deps.InstallSkill(source, to)
}

func DefaultDeps() (AppDeps, error) {
	store, err := session.DefaultStore()
	if err != nil {
		return AppDeps{}, err
	}
	return AppDeps{
		StartWorker:           startWorkerProcess,
		StopProcess:           stopProcess,
		ReserveControlAddress: reserveControlAddress,
		WaitForControl:        waitForControlAddress,
		CreateVirtualPorts:    createVirtualPorts,
		RemoveVirtualPorts:    removeVirtualPorts,
		ClearSharePorts:       clearSharePorts,
		RunSetupC:             runSetupCDirect,
		AdminPause:            pauseAdminWindow,
		BridgeTCP:             serialcmd.BridgeTCP,
		RunSessionServer:      serialcmd.RunSessionServer,
		RunShareBridge:        serialcmd.ShareBridge,
		Store:                 store,
		ListPorts:             serialcmd.Ports,
		SendSerial:            serialcmd.Send,
		AskSerial:             serialcmd.Ask,
		SendSession:           serialcmd.SendToSession,
		StreamSerial:          serialcmd.Stream,
		StreamSession:         serialcmd.StreamSession,
		IsProcessRunning:      isProcessRunning,
		RetrySleep:            time.Sleep,
		Stdin:                 os.Stdin,
		ConfigureShellConsole: configureShellConsole,
	}, nil
}

func (a *App) runAdmin(args []string, out io.Writer) error {
	runSetupC := a.deps.RunSetupC
	if runSetupC == nil {
		runSetupC = runSetupCDirect
	}
	pause := a.deps.AdminPause
	if pause == nil {
		pause = pauseAdminWindow
	}

	switch {
	case len(args) >= 2 && args[0] == "setupc":
		setupcArgs := append([]string(nil), args[1:]...)
		err := runSetupCOperationsInAdminWindow([]setupCOperation{{Description: "run setupc.exe", Args: setupcArgs}}, out, runSetupC)
		pause()
		return err
	case len(args) >= 2 && args[0] == "setupc-batch":
		encoded := args[1]
		logPath := adminLogPath(args[2:])
		if logPath != "" {
			var file *os.File
			file, err := os.Create(logPath)
			if err != nil {
				return err
			}
			defer file.Close()
			out = io.MultiWriter(out, file)
		}
		ops, err := decodeSetupCOperations(encoded)
		if err != nil {
			_, _ = fmt.Fprintf(out, "FAILED: decode setupc batch: %v\n\n", err)
			pause()
			return err
		}
		err = runSetupCOperationsInAdminWindow(ops, out, runSetupC)
		pause()
		return err
	case len(args) >= 2 && args[0] == "pnp-remove":
		parents, logPath := splitAdminLogArg(args[1:])
		if logPath != "" {
			var file *os.File
			file, err := os.Create(logPath)
			if err != nil {
				return err
			}
			defer file.Close()
			out = io.MultiWriter(out, file)
		}
		err := runPnpRemoveParents(parents, out)
		if err != nil {
			_, _ = fmt.Fprintf(out, "\nFAILED: %v\n\n", err)
			pause()
		}
		return err
	default:
		return a.usage("admin setupc <args...>")
	}
}

func adminLogPath(args []string) string {
	_, logPath := splitAdminLogArg(args)
	return logPath
}

func splitAdminLogArg(args []string) ([]string, string) {
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--log" && i+1 < len(args) {
			return append(clean, args[i+2:]...), args[i+1]
		}
		clean = append(clean, args[i])
	}
	return clean, ""
}

func runSetupCOperationsInAdminWindow(ops []setupCOperation, out io.Writer, runSetupC setupCRunner) error {
	var runErr error
	for _, op := range ops {
		if op.Description != "" {
			_, _ = fmt.Fprintf(out, "%s\n", op.Description)
		}
		if err := releaseStaleCOMNameReservationsForOperation(op, out); err != nil {
			runErr = err
			_, _ = fmt.Fprintf(out, "\nFAILED: %v\n\n", err)
			break
		}
		_, _ = fmt.Fprintf(out, "Running com0com setupc.exe %s\n\n", strings.Join(op.Args, " "))
		err := runSetupC(op.Args, out)
		if err != nil {
			runErr = err
			_, _ = fmt.Fprintf(out, "\nFAILED: %v\n\n", err)
			break
		}
		_, _ = fmt.Fprintln(out, "\nOK")
		_, _ = fmt.Fprintln(out)
	}
	if runErr != nil {
		return runErr
	}
	_, _ = fmt.Fprintln(out, "SUCCESS: setupc.exe completed.")
	_, _ = fmt.Fprintln(out)
	return nil
}

func (a *App) runTools(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "extract" {
		return a.usage("tools extract <dir>")
	}
	return errors.New("no third-party tools are bundled; install com0com externally and make setupc.exe discoverable on PATH")
}

func (a *App) runPorts(out io.Writer) error {
	if a.deps.ListPorts == nil {
		_, _ = fmt.Fprintln(out, "port listing is unavailable")
		return nil
	}
	ports, err := a.deps.ListPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		_, _ = fmt.Fprintln(out, "no serial ports found")
		return nil
	}
	for _, port := range ports {
		_, _ = fmt.Fprintln(out, port)
	}
	return nil
}

func (a *App) runOpen(args []string, out io.Writer) error {
	if len(args) < 2 {
		return a.usage("open <session> <port> [-b baud]")
	}
	name := args[0]
	port := args[1]
	baud := 115200
	if err := session.ValidateName(name); err != nil {
		return err
	}
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-b":
			if i+1 >= len(args) {
				return a.usage("open <session> <port> [-b baud]")
			}
			parsed, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fmt.Errorf("invalid baud rate %q", args[i+1])
			}
			baud = parsed
			i++
		default:
			return a.usage("open <session> <port> [-b baud]")
		}
	}
	if port == "" {
		return a.usage("open <session> <port> [-b baud]")
	}
	if baud <= 0 {
		return errors.New("baud rate must be positive")
	}
	if a.deps.Store.Dir != "" {
		loadedState, loadErr := a.deps.Store.Load(name)
		hadPreviousState := loadErr == nil
		if hadPreviousState {
			if err := a.stopLiveResources(loadedState); err != nil {
				return err
			}
		}
		state := session.State{Name: name, Port: port, Baud: baud, Status: session.StatusConfigured}
		if a.deps.ReserveControlAddress != nil {
			controlAddress, err := a.deps.ReserveControlAddress()
			if err != nil {
				return err
			}
			state.ControlAddress = controlAddress
		}
		if err := a.deps.Store.Save(state); err != nil {
			return err
		}
		if err := a.startSessionWorker(name); err != nil {
			if hadPreviousState {
				if saveErr := a.deps.Store.Save(loadedState); saveErr != nil {
					return errors.Join(err, saveErr)
				}
			} else if removeErr := a.deps.Store.Remove(name); removeErr != nil {
				return errors.Join(err, removeErr)
			}
			return err
		}
	}
	_, _ = fmt.Fprintf(out, "session %s: %s at %d baud\n", name, port, baud)
	return nil
}

func (a *App) startSessionWorker(name string) error {
	if a.deps.StartWorker == nil {
		return nil
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	pid, err := a.deps.StartWorker(name)
	if err != nil {
		return err
	}
	state, err = a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	state.WorkerPID = pid
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	if state.ControlAddress == "" || a.deps.WaitForControl == nil {
		return nil
	}
	if err := a.deps.WaitForControl(state.ControlAddress); err != nil {
		startupErr := a.workerStartupErrorFromLog(name, pid, "session")
		if startupErr == nil {
			startupErr = err
		}
		if a.deps.StopProcess != nil && (a.deps.IsProcessRunning == nil || a.deps.IsProcessRunning(pid)) {
			_ = a.deps.StopProcess(pid)
		}
		state.WorkerPID = 0
		state.ControlAddress = ""
		if saveErr := a.deps.Store.Save(state); saveErr != nil {
			return errors.Join(startupErr, saveErr)
		}
		return startupErr
	}
	return nil
}

func (a *App) runSend(args []string, out io.Writer) error {
	if len(args) != 2 {
		return a.usage("send <session> <data>")
	}
	name := args[0]
	if a.deps.Store.Dir != "" && (a.deps.SendSerial != nil || a.deps.SendSession != nil) {
		state, err := a.deps.Store.Load(name)
		if err != nil {
			return err
		}
		if state.Paused {
			return a.pausedSessionError()
		}
		if address, ok, err := sessionDialAddress(state); err != nil {
			return err
		} else if ok && a.deps.SendSession != nil {
			if err := a.deps.SendSession(address, args[1]); err != nil {
				return err
			}
		} else if state.Status == session.StatusSharing {
			return a.sharedControlUnavailable()
		} else if a.deps.SendSerial != nil {
			if err := a.deps.SendSerial(state.Port, state.Baud, args[1]); err != nil {
				return err
			}
		} else {
			return errors.New("serial sender is unavailable")
		}
	}
	_, _ = fmt.Fprintf(out, "sent %d bytes to %s\n", len(args[1]), name)
	return nil
}

func (a *App) runAsk(args []string, out io.Writer) error {
	if len(args) < 2 {
		return a.usage("ask <session> <data> [-t seconds] [-l lines]")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeoutSeconds := fs.Float64("t", 0.5, "read response window in seconds")
	maxLines := fs.Int("l", 50, "print the last N response lines; 0 means unlimited")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return a.usage("ask <session> <data> [-t seconds] [-l lines]")
	}
	if *timeoutSeconds <= 0 {
		return errors.New("ask timeout must be positive")
	}
	if *maxLines < 0 {
		return errors.New("ask line limit must not be negative")
	}
	if a.deps.Store.Dir == "" {
		if a.deps.AskSerial == nil {
			_, _ = fmt.Fprintln(out, "serial ask is unavailable")
			return nil
		}
		return a.deps.AskSerial(serialcmd.AskOptions{
			Data:     args[1],
			Timeout:  time.Duration(*timeoutSeconds * float64(time.Second)),
			MaxLines: *maxLines,
			Output:   out,
		})
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	if state.Paused {
		return a.pausedSessionError()
	}
	if address, ok, err := sessionDialAddress(state); err != nil {
		return err
	} else if ok {
		if a.deps.SendSession == nil {
			return errors.New("session sender is unavailable")
		}
		return a.askViaSessionWorker(state, address, args[1], time.Duration(*timeoutSeconds*float64(time.Second)), *maxLines, out)
	}
	if state.Status == session.StatusSharing {
		return a.sharedControlUnavailable()
	}
	if a.deps.AskSerial == nil {
		return errors.New("serial ask is unavailable")
	}
	return a.deps.AskSerial(serialcmd.AskOptions{
		Port:      state.Port,
		Baud:      state.Baud,
		Data:      args[1],
		Timeout:   time.Duration(*timeoutSeconds * float64(time.Second)),
		MaxLines:  *maxLines,
		Output:    out,
		CachePath: a.deps.Store.CachePath(name),
	})
}

func (a *App) askViaSessionWorker(state session.State, address string, data string, timeout time.Duration, maxLines int, out io.Writer) error {
	cachePath := a.deps.Store.CachePath(state.Name)
	start := cacheSize(cachePath)
	if err := a.deps.SendSession(address, data); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	next := start
	var response bytes.Buffer
	for {
		end, err := copyCacheBytes(cachePath, &response, next)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next = end
		if time.Now().After(deadline) {
			_, err := out.Write(lastLines(response.Bytes(), maxLines))
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cacheSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func copyCacheBytes(path string, out io.Writer, start int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return start, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return start, err
	}
	written, err := io.Copy(out, f)
	return start + written, err
}

func lastLines(data []byte, maxLines int) []byte {
	if maxLines <= 0 {
		return data
	}
	lines := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] != '\n' {
			continue
		}
		lines++
		if lines > maxLines {
			return data[i+1:]
		}
	}
	return data
}

func teeOutput(out io.Writer, path string) (io.Writer, func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, func() {}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, func() {}, err
	}
	return io.MultiWriter(out, file), func() {
		_ = file.Close()
	}, nil
}

func (a *App) runShell(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("shell <session>")
	}
	input := a.deps.Stdin
	usingDefaultStdin := input == nil || input == os.Stdin
	if input == nil {
		input = os.Stdin
	}
	if usingDefaultStdin || a.deps.ShellInterrupts != nil {
		var restoreConsole func()
		if usingDefaultStdin && a.deps.ConfigureShellConsole != nil {
			restoreConsole = a.deps.ConfigureShellConsole()
		}
		wrappedInput, cleanup := shellInputWithInterrupts(input, a.deps.ShellInterrupts, out)
		defer cleanup()
		if restoreConsole != nil {
			defer restoreConsole()
		}
		input = wrappedInput
	}
	return a.runStream(args[0], serialcmd.StreamOptions{Input: input}, out)
}

const shellInterruptExitWindow = 2 * time.Second
const shellInterruptDuplicateWindow = 50 * time.Millisecond

func shellInputWithInterrupts(input io.Reader, interrupts <-chan os.Signal, echo io.Writer) (io.Reader, func()) {
	reader, writer := io.Pipe()
	stop := make(chan struct{})
	var notifyCh chan os.Signal
	signalCh := interrupts
	if signalCh == nil {
		notifyCh = make(chan os.Signal, 1)
		signal.Notify(notifyCh, os.Interrupt)
		signalCh = notifyCh
	}

	go copyShellInput(writer, input, stop, echo)

	go func() {
		var lastInterrupt time.Time
		for {
			select {
			case <-stop:
				return
			case _, ok := <-signalCh:
				if !ok {
					return
				}
				now := time.Now()
				if !lastInterrupt.IsZero() && now.Sub(lastInterrupt) <= shellInterruptExitWindow {
					if now.Sub(lastInterrupt) <= shellInterruptDuplicateWindow {
						continue
					}
					_ = writer.Close()
					return
				}
				lastInterrupt = now
				go func() {
					_, _ = writer.Write([]byte{0x03})
				}()
			}
		}
	}()

	return reader, func() {
		close(stop)
		if notifyCh != nil {
			signal.Stop(notifyCh)
		}
		_ = writer.Close()
	}
}

func copyShellInput(writer *io.PipeWriter, input io.Reader, stop <-chan struct{}, echo io.Writer) {
	state := &shellInputState{}
	buf := make([]byte, 4096)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			if writeErr := state.write(writer, echo, buf[:n]); writeErr != nil {
				return
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			_ = writer.Close()
			return
		}
		if isShellInputInterrupted(err) {
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		_ = writer.CloseWithError(err)
		return
	}
}

type shellInputState struct {
	lastCtrlC time.Time
}

func (s *shellInputState) write(writer *io.PipeWriter, echo io.Writer, data []byte) error {
	for _, ch := range data {
		if ch == 0x03 {
			now := time.Now()
			if !s.lastCtrlC.IsZero() && now.Sub(s.lastCtrlC) <= shellInterruptExitWindow {
				if now.Sub(s.lastCtrlC) <= shellInterruptDuplicateWindow {
					continue
				}
				return writer.Close()
			}
			s.lastCtrlC = now
		}
		if ch == '\r' {
			if echo != nil {
				_, _ = echo.Write([]byte{'\r', '\n'})
			}
			ch = '\n'
		} else if echo != nil && ch != 0x03 {
			_, _ = echo.Write([]byte{ch})
		}
		if _, err := writer.Write([]byte{ch}); err != nil {
			return err
		}
	}
	return nil
}

func isShellInputInterrupted(err error) bool {
	return errors.Is(err, ErrShellInputInterrupted) || isPlatformShellInputInterrupted(err)
}

func (a *App) runTee(args []string, out io.Writer) error {
	if len(args) != 2 {
		return a.usage("tee <session> <file>")
	}
	return a.runStream(args[0], serialcmd.StreamOptions{TeePath: args[1]}, out)
}

func (a *App) runStream(name string, opts serialcmd.StreamOptions, out io.Writer) error {
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir == "" {
		if a.deps.StreamSerial == nil {
			_, _ = fmt.Fprintln(out, "serial streaming is unavailable")
			return nil
		}
		opts.Output = out
		return a.deps.StreamSerial(opts)
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	if state.Paused {
		return a.pausedSessionError()
	}
	if address, ok, err := sessionDialAddress(state); err != nil {
		return err
	} else if ok && a.deps.StreamSession != nil {
		output := out
		var closeOutput func()
		if opts.TeePath != "" {
			var err error
			output, closeOutput, err = teeOutput(out, opts.TeePath)
			if err != nil {
				return err
			}
			defer closeOutput()
		}
		return a.deps.StreamSession(address, opts.Input, output)
	} else if state.Status == session.StatusSharing {
		return a.sharedControlUnavailable()
	}
	if a.deps.StreamSerial == nil {
		return errors.New("serial streaming is unavailable")
	}
	opts.Port = state.Port
	opts.Baud = state.Baud
	opts.Output = out
	opts.CachePath = a.deps.Store.CachePath(name)
	return a.deps.StreamSerial(opts)
}

func (a *App) runRead(args []string, out io.Writer) error {
	if len(args) == 0 {
		return a.usage("read <session> [-n count] [--to file]")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.Int("n", 0, "read last n bytes")
	to := fs.String("to", "", "write cached data to file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return a.usage("read <session> [-n count] [--to file]")
	}
	if *n < 0 {
		return errors.New("read count must not be negative")
	}
	if a.deps.Store.Dir != "" {
		cachePath := a.deps.Store.CachePath(name)
		if *to != "" {
			written, err := copyCacheToFile(cachePath, *to, int64(*n))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					_, _ = fmt.Fprintln(out, "no cached data")
					return nil
				}
				return err
			}
			_, _ = fmt.Fprintf(out, "wrote %d bytes to %s\n", written, *to)
			return nil
		}
		data, err := os.ReadFile(cachePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintln(out, "no cached data")
				return nil
			}
			return err
		}
		if *n > 0 && len(data) > *n {
			data = data[len(data)-*n:]
		}
		_, _ = out.Write(data)
		return nil
	}
	if *n == 0 {
		_, _ = fmt.Fprintln(out, "no cached data")
		return nil
	}
	_, _ = fmt.Fprintln(out, "no cached data, window="+strconv.Itoa(*n))
	return nil
}

func copyCacheToFile(cachePath string, destPath string, lastBytes int64) (int64, error) {
	if destPath == "" {
		return 0, errors.New("destination file is required")
	}
	in, err := os.Open(cachePath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if lastBytes > 0 && info.Size() > lastBytes {
		if _, err := in.Seek(info.Size()-lastBytes, io.SeekStart); err != nil {
			return 0, err
		}
	}
	dir := filepath.Dir(destPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

func (a *App) runCheck(args []string, out io.Writer) error {
	if len(args) == 0 {
		return a.usage("check <session> [-n count] [--from offset] [--rewind count] [--to file]")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.Int64("n", 0, "read at most n bytes")
	from := fs.Int64("from", -1, "read from absolute cache offset")
	rewind := fs.Int64("rewind", 0, "move cursor back before reading")
	to := fs.String("to", "", "write checked data to file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return a.usage("check <session> [-n count] [--from offset] [--rewind count] [--to file]")
	}
	if *n < 0 {
		return errors.New("check count must not be negative")
	}
	if *from < -1 {
		return errors.New("check offset must not be negative")
	}
	if *rewind < 0 {
		return errors.New("check rewind must not be negative")
	}
	if a.deps.Store.Dir == "" {
		_, _ = fmt.Fprintln(out, "no cached data")
		return nil
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	start := state.CheckOffset
	if *from >= 0 {
		start = *from
	} else if *rewind > 0 {
		start -= *rewind
		if start < 0 {
			start = 0
		}
	}
	end, written, err := copyCacheRange(a.deps.Store.CachePath(name), out, *to, start, *n)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintln(out, "no cached data")
			return nil
		}
		return err
	}
	state.CheckOffset = end
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	if *to != "" {
		_, _ = fmt.Fprintf(out, "wrote %d bytes to %s\n", written, *to)
	}
	return nil
}

func copyCacheRange(cachePath string, terminalOut io.Writer, destPath string, start int64, maxBytes int64) (int64, int64, error) {
	in, err := os.Open(cachePath)
	if err != nil {
		return 0, 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := info.Size()
	if start > size {
		start = size
	}
	if start < 0 {
		start = 0
	}
	end := size
	if maxBytes > 0 && start+maxBytes < end {
		end = start + maxBytes
	}
	if _, err := in.Seek(start, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var out io.Writer = terminalOut
	var closeOut func() error
	if destPath != "" {
		file, err := createOutputFile(destPath)
		if err != nil {
			return 0, 0, err
		}
		out = file
		closeOut = file.Close
	}
	written, err := io.CopyN(out, in, end-start)
	if closeOut != nil {
		if closeErr := closeOut(); err == nil {
			err = closeErr
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, written, err
	}
	return start + written, written, nil
}

func createOutputFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("destination file is required")
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

func (a *App) runClear(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("clear <session>|--share")
	}
	if args[0] == "--share" {
		return a.runClearShare(out)
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir != "" {
		if err := os.MkdirAll(a.deps.Store.SessionDir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.deps.Store.CachePath(name), nil, 0o644); err != nil {
			return err
		}
		state, err := a.deps.Store.Load(name)
		if err == nil {
			state.CheckOffset = 0
			if err := a.deps.Store.Save(state); err != nil {
				return err
			}
		}
	}
	_, _ = fmt.Fprintf(out, "cache cleared for %s\n", name)
	return nil
}

func (a *App) runClearShare(out io.Writer) error {
	var clearErr error
	_, _ = fmt.Fprintln(out, "clearing shared virtual ports")
	if a.deps.Store.Dir != "" {
		states, err := a.deps.Store.List()
		if err != nil {
			return err
		}
		for _, state := range states {
			if len(state.VirtualPorts) == 0 && len(state.HubPorts) == 0 && state.Status != session.StatusSharing {
				continue
			}
			if err := a.stopProcessesOnly(state); err != nil {
				clearErr = errors.Join(clearErr, fmt.Errorf("%s: %w", state.Name, err))
			}
			if err := a.deps.Store.Stop(state.Name); err != nil {
				clearErr = errors.Join(clearErr, fmt.Errorf("%s: %w", state.Name, err))
			}
		}
	}
	if a.deps.ClearSharePorts != nil {
		if err := a.deps.ClearSharePorts(); err != nil {
			clearErr = errors.Join(clearErr, err)
		}
	}
	if clearErr != nil {
		_, _ = fmt.Fprintf(out, "shared virtual port cleanup failed: %v\n", clearErr)
		return clearErr
	}
	_, _ = fmt.Fprintln(out, "shared virtual ports cleared")
	return nil
}

func (a *App) runTCP(args []string, out io.Writer) error {
	if len(args) != 2 {
		return a.usage("tcp <session> <listen-address>")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	listenAddress := normalizeTCPListenAddress(args[1])
	if err := validateTCPListenAddress(listenAddress); err != nil {
		return err
	}
	if a.deps.Store.Dir != "" {
		state, err := a.deps.Store.Load(name)
		if err != nil {
			return err
		}
		previousState := state
		preserveShareResources := state.Status == session.StatusSharing && (len(state.VirtualPorts) > 0 || len(state.HubPorts) > 0 || state.HubPID != 0)
		if preserveShareResources {
			if err := a.stopProcessesOnly(state); err != nil {
				return err
			}
			state.WorkerPID = 0
			state.HubPID = 0
		} else {
			if err := a.stopLiveResources(state); err != nil {
				return err
			}
			state.VirtualPorts = nil
			state.HubPorts = nil
			state.HubPID = 0
			state.Status = session.StatusTCP
		}
		state.Paused = false
		state.TCPAddress = listenAddress
		state.ControlAddress = ""
		state.WorkerPID = 0
		if err := a.deps.Store.Save(state); err != nil {
			return err
		}
		if a.deps.StartWorker != nil {
			pid, err := a.deps.StartWorker(name)
			if err != nil {
				return err
			}
			state, err = a.deps.Store.Load(name)
			if err != nil {
				return err
			}
			state.WorkerPID = pid
			if err := a.deps.Store.Save(state); err != nil {
				return err
			}
			if a.deps.IsProcessRunning != nil {
				startMode := "tcp"
				if preserveShareResources {
					startMode = "share"
				}
				if err := a.waitForWorkerListenStartup(name, pid, startMode); err != nil {
					if a.deps.StopProcess != nil {
						_ = a.deps.StopProcess(pid)
					}
					previousState.WorkerPID = 0
					previousState.HubPID = 0
					if saveErr := a.deps.Store.Save(previousState); saveErr != nil {
						return errors.Join(err, saveErr)
					}
					return err
				}
			}
		}
	}
	_, _ = fmt.Fprintf(out, "tcp forwarding %s at %s\n", name, listenAddress)
	return nil
}

func (a *App) runShare(args []string, out io.Writer) error {
	if len(args) < 2 {
		return a.usage("share <session> <virtual-port> [virtual-port...]")
	}
	name := args[0]
	virtualPorts := append([]string(nil), args[1:]...)
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir != "" {
		state, err := a.deps.Store.Load(name)
		if err != nil {
			return err
		}
		if err := a.stopLiveResources(state); err != nil {
			return err
		}
		state.Status = session.StatusSharing
		state.Paused = false
		state.VirtualPorts = virtualPorts
		state.HubPorts = hubPortsFor(virtualPorts)
		state.ControlAddress = ""
		if len(virtualPorts) > 0 && a.deps.ReserveControlAddress != nil {
			controlAddress, err := a.deps.ReserveControlAddress()
			if err != nil {
				return err
			}
			state.ControlAddress = controlAddress
		}
		state.WorkerPID = 0
		state.HubPID = 0
		if err := a.deps.Store.Save(state); err != nil {
			return err
		}
		if a.deps.CreateVirtualPorts != nil {
			if err := a.deps.CreateVirtualPorts(portPairs(state.VirtualPorts, state.HubPorts)); err != nil {
				return err
			}
		}
		if a.deps.StartWorker != nil {
			pid, err := a.deps.StartWorker(name)
			if err != nil {
				return err
			}
			state, err = a.deps.Store.Load(name)
			if err != nil {
				return err
			}
			state.WorkerPID = pid
			if err := a.deps.Store.Save(state); err != nil {
				return err
			}
			if state.ControlAddress != "" && a.deps.WaitForControl != nil {
				if err := a.deps.WaitForControl(state.ControlAddress); err != nil {
					startupErr := a.workerStartupErrorFromLog(name, pid, "share")
					if startupErr == nil {
						startupErr = err
					}
					if a.deps.StopProcess != nil && (a.deps.IsProcessRunning == nil || a.deps.IsProcessRunning(pid)) {
						_ = a.deps.StopProcess(pid)
					}
					state.WorkerPID = 0
					if saveErr := a.deps.Store.Save(state); saveErr != nil {
						return errors.Join(startupErr, saveErr)
					}
					return startupErr
				}
			}
		}
	}
	_, _ = fmt.Fprintf(out, "sharing %s via %s\n", name, strings.Join(virtualPorts, ","))
	return nil
}

func (a *App) stopProcessesOnly(state session.State) error {
	var stopErr error
	if a.deps.StopProcess == nil {
		return nil
	}
	seen := map[int]bool{}
	for _, pid := range []int{state.WorkerPID, state.HubPID} {
		if pid == 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		if a.deps.IsProcessRunning != nil && !a.deps.IsProcessRunning(pid) {
			continue
		}
		if err := a.deps.StopProcess(pid); err != nil {
			stopErr = errors.Join(stopErr, err)
			continue
		}
		if err := a.waitForProcessExit(pid); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
	}
	return stopErr
}

func (a *App) stopLiveResources(state session.State) error {
	var stopErr error
	if a.deps.StopProcess != nil {
		seen := map[int]bool{}
		for _, pid := range []int{state.WorkerPID, state.HubPID} {
			if pid == 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			if a.deps.IsProcessRunning != nil && !a.deps.IsProcessRunning(pid) {
				continue
			}
			if err := a.deps.StopProcess(pid); err != nil {
				stopErr = errors.Join(stopErr, err)
				continue
			}
			if err := a.waitForProcessExit(pid); err != nil {
				stopErr = errors.Join(stopErr, err)
			}
		}
	}
	if a.deps.RemoveVirtualPorts != nil && len(state.VirtualPorts) > 0 {
		if err := a.deps.RemoveVirtualPorts(portPairs(state.VirtualPorts, state.HubPorts)); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
	}
	return stopErr
}

func (a *App) runWorker(args []string, out io.Writer) error {
	if len(args) != 2 {
		return a.usage("worker share|tcp <session>")
	}
	switch args[0] {
	case "run":
		return a.runWorkerAuto(args[1], out)
	case "share":
		return a.runWorkerShare(args[1], out)
	case "tcp":
		return a.runWorkerTCP(args[1], out)
	default:
		return a.usage("worker share|tcp <session>")
	}
}

func (a *App) runWorkerAuto(name string, out io.Writer) error {
	if a.deps.Store.Dir == "" {
		return errors.New("session store is unavailable")
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	if len(state.VirtualPorts) == 0 {
		if state.TCPAddress != "" {
			return a.runWorkerTCP(name, out)
		}
		return a.runWorkerSession(name, out)
	}
	return a.runWorkerShare(name, out)
}

func (a *App) runWorkerSession(name string, out io.Writer) (retErr error) {
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir == "" {
		return errors.New("session store is unavailable")
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	logPath := a.deps.Store.WorkerLogPath(name)
	appendWorkerLog := func(line string) {
		_ = session.AppendLog(logPath, line)
	}
	appendWorkerLog(fmt.Sprintf("worker start mode=session pid=%d control=%s", os.Getpid(), state.ControlAddress))
	defer func() {
		if retErr != nil {
			appendWorkerLog("worker error " + retErr.Error())
		}
		appendWorkerLog("worker exit")
	}()
	if state.Paused {
		return a.pausedSessionError()
	}
	if state.ControlAddress == "" {
		return errors.New("session control address is required")
	}
	if a.deps.RunSessionServer == nil {
		return errors.New("session server is unavailable")
	}
	state.Status = session.StatusConfigured
	state.WorkerPID = os.Getpid()
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	return a.runWorkerWithRetry(name, appendWorkerLog, func() error {
		return a.deps.RunSessionServer(serialcmd.SessionServerOptions{
			ControlAddress: state.ControlAddress,
			Port:           state.Port,
			Baud:           state.Baud,
			CachePath:      a.deps.Store.CachePath(name),
		})
	})
}

func (a *App) runWorkerShare(name string, out io.Writer) (retErr error) {
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir == "" {
		return errors.New("session store is unavailable")
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	logPath := a.deps.Store.WorkerLogPath(name)
	appendWorkerLog := func(line string) {
		_ = session.AppendLog(logPath, line)
	}
	appendWorkerLog(fmt.Sprintf("worker start mode=share pid=%d", os.Getpid()))
	defer func() {
		if retErr != nil {
			appendWorkerLog("worker error " + retErr.Error())
		}
		appendWorkerLog("worker exit")
	}()
	if state.Paused {
		return a.pausedSessionError()
	}
	state.Status = session.StatusSharing
	state.WorkerPID = os.Getpid()
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	if len(state.HubPorts) > 0 {
		if a.deps.RunShareBridge == nil {
			return errors.New("share bridge is unavailable")
		}
		controlAddress := state.ControlAddress
		if controlAddress == "" {
			controlAddress = state.TCPAddress
		}
		state.HubPID = 0
		if err := a.deps.Store.Save(state); err != nil {
			return err
		}
		return a.runWorkerWithRetry(name, appendWorkerLog, func() error {
			return a.deps.RunShareBridge(serialcmd.ShareBridgeOptions{
				PhysicalPort:   state.Port,
				HubPorts:       append([]string(nil), state.HubPorts...),
				Baud:           state.Baud,
				CachePath:      a.deps.Store.CachePath(name),
				ControlAddress: controlAddress,
				TCPAddress:     state.TCPAddress,
				OnListening: func(address string) {
					appendWorkerLog("worker ready listen=" + address)
				},
			})
		})
	}
	if a.deps.StreamSerial == nil {
		return errors.New("serial streaming is unavailable")
	}
	return a.runWorkerWithRetry(name, appendWorkerLog, func() error {
		return a.deps.StreamSerial(serialcmd.StreamOptions{
			Port:      state.Port,
			Baud:      state.Baud,
			Output:    io.Discard,
			CachePath: a.deps.Store.CachePath(name),
		})
	})
}

func (a *App) runWorkerTCP(name string, out io.Writer) (retErr error) {
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir == "" {
		return errors.New("session store is unavailable")
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	logPath := a.deps.Store.WorkerLogPath(name)
	appendWorkerLog := func(line string) {
		_ = session.AppendLog(logPath, line)
	}
	appendWorkerLog(fmt.Sprintf("worker start mode=tcp pid=%d listen=%s", os.Getpid(), state.TCPAddress))
	defer func() {
		if retErr != nil {
			appendWorkerLog("worker error " + retErr.Error())
		}
		appendWorkerLog("worker exit")
	}()
	if state.Paused {
		return a.pausedSessionError()
	}
	if state.TCPAddress == "" {
		return errors.New("tcp listen address is required")
	}
	if err := validateTCPListenAddress(state.TCPAddress); err != nil {
		return err
	}
	if a.deps.BridgeTCP == nil {
		return errors.New("tcp bridge is unavailable")
	}
	state.Status = session.StatusTCP
	state.WorkerPID = os.Getpid()
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	return a.runWorkerWithRetry(name, appendWorkerLog, func() error {
		return a.deps.BridgeTCP(serialcmd.TCPBridgeOptions{
			ListenAddress: state.TCPAddress,
			Port:          state.Port,
			Baud:          state.Baud,
			CachePath:     a.deps.Store.CachePath(name),
			OnListening: func(address string) {
				appendWorkerLog("worker ready listen=" + address)
			},
		})
	})
}

func validateTCPListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid tcp listen address %q: %w", address, err)
	}
	if host == "" && !strings.HasPrefix(address, ":") {
		return fmt.Errorf("invalid tcp listen address %q: missing host separator", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("invalid tcp listen address %q: port must be 1-65535", address)
	}
	if _, err := net.ResolveTCPAddr("tcp", address); err != nil {
		return fmt.Errorf("invalid tcp listen address %q: %w", address, err)
	}
	return nil
}

func normalizeTCPListenAddress(address string) string {
	if strings.Contains(address, ":") || strings.ContainsAny(address, " \t\r\n") {
		return address
	}
	return net.JoinHostPort(address, defaultTCPListenPort)
}

func sessionDialAddress(state session.State) (string, bool, error) {
	if state.ControlAddress != "" {
		return state.ControlAddress, true, nil
	}
	if state.TCPAddress == "" {
		return "", false, nil
	}
	address, err := tcpListenAddressToDialAddress(state.TCPAddress)
	if err != nil {
		return "", false, err
	}
	return address, true, nil
}

func tcpListenAddressToDialAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func (a *App) waitForWorkerListenStartup(name string, pid int, mode string) error {
	startMarker := fmt.Sprintf("worker start mode=%s pid=%d", mode, pid)
	deadline := time.Now().Add(2 * time.Second)
	logPath := a.deps.Store.WorkerLogPath(name)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(logPath)
		if err == nil {
			started := false
			for _, line := range strings.Split(string(content), "\n") {
				if strings.Contains(line, startMarker) {
					started = true
					continue
				}
				if !started {
					continue
				}
				if strings.Contains(line, " worker ready listen=") {
					return nil
				}
				if idx := strings.Index(line, " worker retry error="); idx >= 0 {
					return errors.New(workerRetryErrorText(line[idx+len(" worker retry error="):]))
				}
				if idx := strings.Index(line, " worker error "); idx >= 0 {
					return errors.New(strings.TrimSpace(line[idx+len(" worker error "):]))
				}
				if strings.Contains(line, " worker exit") && !a.deps.IsProcessRunning(pid) {
					return fmt.Errorf("tcp worker %d exited before listening", pid)
				}
			}
		}
		if !a.deps.IsProcessRunning(pid) {
			return fmt.Errorf("tcp worker %d exited before listening", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("wait for %s worker %d to listen timed out", mode, pid)
}

func (a *App) workerStartupErrorFromLog(name string, pid int, mode string) error {
	startMarker := fmt.Sprintf("worker start mode=%s pid=%d", mode, pid)
	return workerStartupErrorFromLogPath(a.deps.Store.WorkerLogPath(name), startMarker)
}

func workerStartupErrorFromLogPath(logPath string, startMarker string) error {
	content, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	started := false
	for _, line := range strings.Split(string(content), "\n") {
		if strings.Contains(line, startMarker) {
			started = true
			continue
		}
		if !started {
			continue
		}
		if idx := strings.Index(line, " worker error "); idx >= 0 {
			return errors.New(strings.TrimSpace(line[idx+len(" worker error "):]))
		}
		if idx := strings.Index(line, " worker retry error="); idx >= 0 {
			return errors.New(workerRetryErrorText(line[idx+len(" worker retry error="):]))
		}
	}
	return nil
}

func workerModeForState(state session.State) string {
	if state.TCPAddress != "" {
		return "tcp"
	}
	if len(state.VirtualPorts) > 0 || state.Status == session.StatusSharing {
		return "share"
	}
	return "session"
}

func workerRetryErrorText(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		if end := strings.Index(raw[1:], `"`); end >= 0 {
			quoted := raw[:end+2]
			if unquoted, err := strconv.Unquote(quoted); err == nil {
				return unquoted
			}
			return strings.Trim(quoted, `"`)
		}
	}
	if idx := strings.Index(raw, " delay="); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.Trim(raw, `"`)
}

func (a *App) runWorkerWithRetry(name string, appendWorkerLog func(string), fn func() error) error {
	if a.deps.RetrySleep == nil {
		return fn()
	}
	return workerpkg.RunWithRetry(fn, workerpkg.RetryOptions{
		Policy: workerpkg.RetryPolicy{Initial: 250 * time.Millisecond, Max: 5 * time.Second},
		ShouldStop: func() bool {
			state, err := a.deps.Store.Load(name)
			return err != nil || state.Status == session.StatusStopped
		},
		OnRetry: func(err error, delay time.Duration) {
			appendWorkerLog(fmt.Sprintf("worker retry error=%q delay=%s", err.Error(), delay))
		},
		Sleep: a.deps.RetrySleep,
	})
}

func (a *App) runPause(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("pause <session>")
	}
	return a.setPaused(args[0], true, out)
}

func (a *App) runResume(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("resume <session>")
	}
	return a.setPaused(args[0], false, out)
}

func (a *App) setPaused(name string, paused bool, out io.Writer) error {
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir == "" {
		if paused {
			_, _ = fmt.Fprintf(out, "serial control paused for %s\n", name)
		} else {
			_, _ = fmt.Fprintf(out, "serial control resumed for %s\n", name)
		}
		return nil
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	state.Paused = paused
	if paused {
		state.Status = session.StatusPaused
	} else if len(state.VirtualPorts) > 0 {
		state.Status = session.StatusSharing
	} else {
		state.Status = session.StatusConfigured
	}
	if err := a.deps.Store.Save(state); err != nil {
		return err
	}
	if paused {
		_, _ = fmt.Fprintf(out, "serial control paused for %s\n", name)
	} else {
		_, _ = fmt.Fprintf(out, "serial control resumed for %s\n", name)
	}
	return nil
}

func (a *App) runStatus(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("status <session>")
	}
	name := args[0]
	if a.deps.Store.Dir == "" {
		_, _ = fmt.Fprintf(out, "no serial session named %s\n", name)
		return nil
	}
	state, err := a.deps.Store.Load(name)
	if err != nil {
		return err
	}
	printState(out, state, a.deps.Store, a.deps.IsProcessRunning)
	return nil
}

func (a *App) runLog(args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return a.usage("log <session> [--worker]")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	logKind := "worker"
	if len(args) == 2 {
		switch args[1] {
		case "--worker":
			logKind = "worker"
		default:
			return a.usage("log <session> [--worker]")
		}
	}
	if a.deps.Store.Dir == "" {
		_, _ = fmt.Fprintf(out, "no serial session named %s\n", name)
		return nil
	}
	if _, err := a.deps.Store.Load(name); err != nil {
		return err
	}
	logPath := a.deps.Store.WorkerLogPath(name)
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(out, "no %s log for %s at %s\n", logKind, name, logPath)
			return nil
		}
		return err
	}
	defer f.Close()
	_, err = io.Copy(out, f)
	return err
}

func (a *App) runList(args []string, out io.Writer) error {
	if len(args) != 0 {
		return a.usage("list")
	}
	if a.deps.Store.Dir == "" {
		_, _ = fmt.Fprintln(out, "no sessions")
		return nil
	}
	states, err := a.deps.Store.List()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		_, _ = fmt.Fprintln(out, "no sessions")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSTATUS\tPHYSICAL\tBAUD\tSHARED\tWORKER")
	for _, state := range states {
		worker := processState(state.WorkerPID, a.deps.IsProcessRunning)
		if state.WorkerPID != 0 {
			worker = fmt.Sprintf("%d/%s", state.WorkerPID, worker)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			state.Name,
			state.Status,
			state.Port,
			state.Baud,
			strings.Join(state.VirtualPorts, ","),
			worker,
		)
	}
	return w.Flush()
}

func (a *App) runStop(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("stop <session>")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir != "" {
		state, err := a.deps.Store.Load(name)
		if err != nil {
			return err
		}
		stopErr := a.stopLiveResources(state)
		if stopErr != nil {
			state.Status = session.StatusStopped
			state.Paused = false
			state.TCPAddress = ""
			state.ControlAddress = ""
			state.CheckOffset = 0
			state.WorkerPID = 0
			state.HubPID = 0
			if err := a.deps.Store.Save(state); err != nil {
				return errors.Join(stopErr, err)
			}
			_, _ = fmt.Fprintf(out, "warning: cleanup for %s was incomplete: %v\n", name, stopErr)
		} else if err := a.deps.Store.Stop(name); err != nil {
			return errors.Join(stopErr, err)
		}
	}
	_, _ = fmt.Fprintf(out, "stopped %s\n", name)
	return nil
}

func (a *App) runRM(args []string, out io.Writer) error {
	if len(args) != 1 {
		return a.usage("rm <session>")
	}
	name := args[0]
	if err := session.ValidateName(name); err != nil {
		return err
	}
	if a.deps.Store.Dir != "" {
		state, err := a.deps.Store.Load(name)
		if err != nil {
			return err
		}
		stopErr := a.stopLiveResources(state)
		if stopErr != nil {
			state.Status = session.StatusStopped
			state.Paused = false
			state.TCPAddress = ""
			state.ControlAddress = ""
			state.CheckOffset = 0
			state.WorkerPID = 0
			state.HubPID = 0
			if err := a.deps.Store.Save(state); err != nil {
				return errors.Join(stopErr, err)
			}
			_, _ = fmt.Fprintf(out, "warning: cleanup for %s was incomplete: %v\n", name, stopErr)
			_, _ = fmt.Fprintf(out, "%s not removed; run %s clear --share or retry %s rm %s after closing ports\n", name, a.commandName(), a.commandName(), name)
			return nil
		}
		if err := a.deps.Store.Remove(name); err != nil {
			return errors.Join(stopErr, err)
		}
	}
	_, _ = fmt.Fprintf(out, "removed %s\n", name)
	return nil
}

func (a *App) waitForProcessExit(pid int) error {
	if a.deps.IsProcessRunning == nil || a.deps.RetrySleep == nil {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !a.deps.IsProcessRunning(pid) {
			return nil
		}
		a.deps.RetrySleep(50 * time.Millisecond)
	}
	if !a.deps.IsProcessRunning(pid) {
		return nil
	}
	return fmt.Errorf("stop process %d: process did not exit", pid)
}

func (a *App) printHelp(out io.Writer) {
	info := a.deps.Version.Normalize()
	name := a.commandName()
	_, _ = fmt.Fprintf(out, "%s - serial CLI\n", name)
	_, _ = fmt.Fprintf(out, "version: %s commit=%s built_at=%s go=%s\n", info.Version, info.Commit, info.BuiltAt, info.GoVersion)
	_, _ = fmt.Fprintf(out, `

Usage:
  %[1]s version
  %[1]s -v
  %[1]s ports
  %[1]s open <session> <port> [-b baud]
  %[1]s send <session> <data>
  %[1]s ask <session> <data> [-t seconds] [-l lines]
  %[1]s read <session> [-n count] [--to file]
  %[1]s check <session> [-n count] [--from offset] [--rewind count] [--to file]
  %[1]s clear <session>
  %[1]s clear --share
  %[1]s shell <session>
  %[1]s tee <session> <file>
  %[1]s share <session> <virtual-port> [virtual-port...]
  %[1]s tcp <session> <listen-address>
  %[1]s pause <session>
  %[1]s resume <session>
  %[1]s status <session>
  %[1]s log <session> [--worker]
  %[1]s stop <session>
  %[1]s rm <session>
  %[1]s list
  %[1]s skill install [source] [--to codex|claude|dir]`, name)
}

func (a *App) printVersion(out io.Writer) {
	info := a.deps.Version.Normalize()
	_, _ = fmt.Fprintf(out, "version: %s\n", info.Version)
	_, _ = fmt.Fprintf(out, "commit: %s\n", info.Commit)
	_, _ = fmt.Fprintf(out, "built_at: %s\n", info.BuiltAt)
	_, _ = fmt.Fprintf(out, "go: %s\n", info.GoVersion)
}

func printState(out io.Writer, state session.State, store session.Store, isRunning func(int) bool) {
	_, _ = fmt.Fprintf(out, "name: %s\n", state.Name)
	_, _ = fmt.Fprintf(out, "status: %s\n", state.Status)
	_, _ = fmt.Fprintf(out, "physical: %s\n", state.Port)
	_, _ = fmt.Fprintf(out, "baud: %d\n", state.Baud)
	if len(state.VirtualPorts) > 0 {
		_, _ = fmt.Fprintf(out, "shared: %s\n", strings.Join(state.VirtualPorts, ","))
	}
	if state.WorkerPID != 0 {
		_, _ = fmt.Fprintf(out, "worker_pid: %d\n", state.WorkerPID)
	}
	_, _ = fmt.Fprintf(out, "worker_state: %s\n", processState(state.WorkerPID, isRunning))
	if store.Dir != "" && state.WorkerPID != 0 {
		mode := workerModeForState(state)
		startMarker := fmt.Sprintf("worker start mode=%s pid=%d", mode, state.WorkerPID)
		if err := workerStartupErrorFromLogPath(store.WorkerLogPath(state.Name), startMarker); err != nil {
			_, _ = fmt.Fprintf(out, "worker_error: %s\n", err)
		}
	}
	if state.TCPAddress != "" {
		_, _ = fmt.Fprintf(out, "tcp: %s\n", state.TCPAddress)
	}
	if store.Dir != "" {
		_, _ = fmt.Fprintf(out, "worker_log: %s\n", store.WorkerLogPath(state.Name))
	}
}

func processState(pid int, isRunning func(int) bool) string {
	if pid == 0 {
		return "stopped"
	}
	if isRunning != nil && !isRunning(pid) {
		return "stale"
	}
	return "running"
}

func hubPortsFor(virtualPorts []string) []string {
	hubPorts := make([]string, 0, len(virtualPorts))
	for _, port := range virtualPorts {
		suffix := strings.TrimPrefix(strings.ToUpper(port), "COM")
		if suffix == "" || suffix == strings.ToUpper(port) {
			suffix = strconv.Itoa(len(hubPorts))
		}
		hubPorts = append(hubPorts, "CNCB"+suffix)
	}
	return hubPorts
}
