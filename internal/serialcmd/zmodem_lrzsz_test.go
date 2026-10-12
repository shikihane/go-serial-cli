package serialcmd

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// lrzsz interop. The tests run lrzsz sz/rz as the peer of the Go side,
// with the process stdin/stdout as the link. lrzsz is looked up in this
// order: SIO_LRZSZ (a directory holding sz and rz), sz/rz on PATH, and on
// Windows inside WSL (on the WSL PATH or under ~/.cache/sio-lrzsz, where
// "apt-get download lrzsz && dpkg -x" leaves it without root). Without
// lrzsz the tests skip.

type lrzsz struct {
	// wsl runs the tools through wsl.exe; dir is then a WSL path.
	wsl bool
	dir string
}

var (
	lrzszOnce  sync.Once
	lrzszFound *lrzsz
)

func findLrzsz() *lrzsz {
	lrzszOnce.Do(func() {
		if dir := os.Getenv("SIO_LRZSZ"); dir != "" {
			if runtime.GOOS == "windows" && strings.HasPrefix(dir, "/") {
				lrzszFound = &lrzsz{wsl: true, dir: dir}
			} else {
				lrzszFound = &lrzsz{dir: dir}
			}
			return
		}
		if runtime.GOOS != "windows" {
			if p, err := exec.LookPath("sz"); err == nil {
				if _, err := exec.LookPath("rz"); err == nil {
					lrzszFound = &lrzsz{dir: filepath.Dir(p)}
				}
			}
			return
		}
		if _, err := exec.LookPath("wsl.exe"); err != nil {
			return
		}
		cmd := exec.Command("wsl.exe", "-e", "sh", "-c",
			`for d in "$(dirname "$(command -v sz 2>/dev/null)" 2>/dev/null)" "$HOME/.cache/sio-lrzsz/root/usr/bin"; do if [ -x "$d/sz" ] && [ -x "$d/rz" ] && "$d/sz" --version 2>&1 | grep -q lrzsz; then echo "LRZSZ=$d"; exit 0; fi; done`)
		out, err := runWithTimeout(cmd, 30*time.Second)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if dir, ok := strings.CutPrefix(strings.TrimSpace(line), "LRZSZ="); ok {
				lrzszFound = &lrzsz{wsl: true, dir: dir}
			}
		}
	})
	return lrzszFound
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		return nil, os.ErrDeadlineExceeded
	}
}

func needLrzsz(t *testing.T) *lrzsz {
	t.Helper()
	if testing.Short() {
		t.Skip("lrzsz interop is skipped in -short mode")
	}
	l := findLrzsz()
	if l == nil {
		t.Skip("lrzsz not found (set SIO_LRZSZ, put sz/rz on PATH, or extract lrzsz under ~/.cache/sio-lrzsz in WSL)")
	}
	return l
}

// lrzszProc is a running sz or rz whose stdin/stdout form the link.
type lrzszProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr bytes.Buffer
	done   chan struct{}
	err    error
}

func (p *lrzszProc) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *lrzszProc) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (l *lrzsz) start(t *testing.T, workDir string, tool string, args ...string) *lrzszProc {
	t.Helper()
	var cmd *exec.Cmd
	if l.wsl {
		full := append([]string{"--cd", workDir, "-e", l.dir + "/" + tool}, args...)
		cmd = exec.Command("wsl.exe", full...)
	} else {
		cmd = exec.Command(filepath.Join(l.dir, tool), args...)
		cmd.Dir = workDir
	}
	p := &lrzszProc{cmd: cmd, done: make(chan struct{})}
	var err error
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if p.stdout, err = cmd.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", tool, err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return p
}

func (p *lrzszProc) wait(t *testing.T) error {
	t.Helper()
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return p.err
	case <-time.After(30 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("lrzsz did not exit; stderr:\n%s", p.stderr.String())
		return nil
	}
}

func runGoAgainst(t *testing.T, p *lrzszProc, opts ZModemOptions) (ZModemResult, string, error) {
	t.Helper()
	var res ZModemResult
	var out bytes.Buffer
	opts.Link = p
	opts.Result = &res
	opts.Output = &out
	if opts.HandshakeWait == 0 {
		opts.HandshakeWait = 20 * time.Second
	}
	errCh := make(chan error, 1)
	go func() { errCh <- TransferZModem(opts) }()
	select {
	case err := <-errCh:
		return res, out.String(), err
	case <-time.After(90 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("go side hung; output:\n%s\nlrzsz stderr:\n%s", out.String(), p.stderr.String())
	}
	return res, "", nil
}

func TestLrzszReceivesFromGoSender(t *testing.T) {
	l := needLrzsz(t)
	cases := []struct {
		name   string
		rzArgs []string
		opts   ZModemOptions
	}{
		{"plain", nil, ZModemOptions{}},
		{"escape-all", []string{"-e"}, ZModemOptions{}},
		{"go-escapes-ctl", nil, ZModemOptions{EscapeCtl: true}},
		{"window-4k", nil, ZModemOptions{Window: 4096}},
		{"small-subpackets", nil, ZModemOptions{MaxSubpacket: 256}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			files := map[string][]byte{
				"a.bin": zPattern(200_000, 7),
				"b.txt": append(zText(5000), []byte("@\r\x18\x10\x11\x13\x91\x93\x00\xff")...),
			}
			var send []ZSendFile
			for _, n := range []string{"a.bin", "b.txt"} {
				send = append(send, ZSendFile{Path: writeTemp(t, src, n, files[n])})
			}
			p := l.start(t, dst, "rz", append([]string{"-b", "-q"}, tc.rzArgs...)...)
			opts := tc.opts
			opts.Files = send
			res, out, err := runGoAgainst(t, p, opts)
			perr := p.wait(t)
			if err != nil || perr != nil {
				t.Fatalf("go err=%v rz err=%v\n%s\nrz stderr:\n%s", err, perr, out, p.stderr.String())
			}
			for n, data := range files {
				mustEqualFile(t, filepath.Join(dst, n), data)
			}
			t.Logf("rz %v: outcome=%s peer-buffer=%d window=%d crc32=%v escctl=%v fdx=%v", tc.rzArgs, res.Outcome, res.PeerBuffer, res.Window, res.CRC32, res.EscapeCtl, res.FullDuplex)
		})
	}
}

func TestGoReceiverFromLrzszSender(t *testing.T) {
	l := needLrzsz(t)
	cases := []struct {
		name   string
		szArgs []string
		opts   ZModemOptions
	}{
		{"plain", nil, ZModemOptions{}},
		{"8k-subpackets", []string{"-8"}, ZModemOptions{}},
		{"escape-all", []string{"-e"}, ZModemOptions{}},
		{"window-4k", []string{"-w", "4096"}, ZModemOptions{}},
		{"crc16", []string{"-o"}, ZModemOptions{}},
		{"go-asks-escape", nil, ZModemOptions{EscapeCtl: true}},
		{"go-advertises-buffer", nil, ZModemOptions{RecvBuffer: 8192}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			files := map[string][]byte{
				"a.bin": zPattern(300_000, 9),
				"b.txt": append(zText(3000), []byte("@\r\x18\x10\x11\x13\x91\x93\x00\xff")...),
			}
			for n, data := range files {
				writeTemp(t, src, n, data)
			}
			p := l.start(t, src, "sz", append(append([]string{"-b", "-q"}, tc.szArgs...), "a.bin", "b.txt")...)
			opts := tc.opts
			opts.Receive = true
			opts.DestDir = dst
			res, out, err := runGoAgainst(t, p, opts)
			perr := p.wait(t)
			if err != nil || perr != nil {
				t.Fatalf("go err=%v sz err=%v\n%s\nsz stderr:\n%s", err, perr, out, p.stderr.String())
			}
			for n, data := range files {
				mustEqualFile(t, filepath.Join(dst, n), data)
			}
			if res.Outcome != OutcomeOK || len(res.Files) != 2 {
				t.Fatalf("result = %+v\n%s", res, out)
			}
		})
	}
}

// sz -r resumes into the Go receiver's .zpart after the prefix check.
func TestGoReceiverResumesFromLrzszSender(t *testing.T) {
	l := needLrzsz(t)
	src, dst := t.TempDir(), t.TempDir()
	data := zPattern(400_000, 22)
	writeTemp(t, src, "r.bin", data)
	writeTemp(t, dst, "r.bin.zpart", data[:150_000])
	p := l.start(t, src, "sz", "-b", "-q", "-r", "r.bin")
	res, out, err := runGoAgainst(t, p, ZModemOptions{Receive: true, DestDir: dst})
	perr := p.wait(t)
	if err != nil || perr != nil {
		t.Fatalf("go err=%v sz err=%v\n%s\n%s", err, perr, out, p.stderr.String())
	}
	mustEqualFile(t, filepath.Join(dst, "r.bin"), data)
	if f := res.Files[0]; f.Status != OutcomeResumed || f.ResumeFrom != 150_000 {
		t.Fatalf("file = %+v", f)
	}
}

// lrzsz rz -r resumes a partial file from the Go sender, which answers
// ZRPOS at any offset. lrzsz rounds the resume point down to 1K, and it
// does not resume when ZF1 is ZMCRC (the Go default): it then receives the
// whole file again. Both are lrzsz choices on its side.
func TestLrzszResumesFromGoSender(t *testing.T) {
	l := needLrzsz(t)
	for _, tc := range []struct {
		policy ConflictPolicy
		resume bool
	}{
		{ConflictOverwrite, true},
		{zConflictNone, true},
		{ConflictSkipIdenticalElseRename, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			dst := t.TempDir()
			data := zPattern(300_000, 23)
			writeTemp(t, dst, "r.bin", data[:100_000])
			p := l.start(t, dst, "rz", "-b", "-q", "-r")
			res, out, err := runGoAgainst(t, p, ZModemOptions{Files: []ZSendFile{{Name: "r.bin", Data: data}}, Conflict: tc.policy})
			perr := p.wait(t)
			if err != nil || perr != nil {
				t.Fatalf("go err=%v rz err=%v\n%s\n%s", err, perr, out, p.stderr.String())
			}
			mustEqualFile(t, filepath.Join(dst, "r.bin"), data)
			from := res.Files[0].ResumeFrom
			if tc.resume && (from <= 0 || from > 100_000) {
				t.Fatalf("lrzsz resumed from %d, want (0, 100000]", from)
			}
			if !tc.resume && from != 0 {
				t.Fatalf("lrzsz resumed from %d under ZMCRC", from)
			}
		})
	}
}

// Without --conflict the Go receiver obeys the management option lrzsz sz
// puts in ZF1; an explicit policy wins over it.
func TestGoReceiverObeysLrzszManagementOptions(t *testing.T) {
	l := needLrzsz(t)
	data := zPattern(20_000, 24)
	other := zPattern(20_000, 25)
	cases := []struct {
		name   string
		args   []string
		policy ConflictPolicy
		status string
		files  map[string][]byte
	}{
		{"protect", []string{"-p"}, "", OutcomeRefused, map[string][]byte{"m.bin": other}},
		{"overwrite", []string{"-y"}, "", OutcomeOK, map[string][]byte{"m.bin": data}},
		{"rename", []string{"-E"}, "", OutcomeOK, map[string][]byte{"m.bin": other, "m_1.bin": data}},
		{"append", []string{"-+"}, "", OutcomeOK, map[string][]byte{"m.bin": append(append([]byte(nil), other...), data...)}},
		{"explicit-skip-beats-overwrite", []string{"-y"}, ConflictSkip, OutcomeRefused, map[string][]byte{"m.bin": other}},
		{"explicit-overwrite-beats-protect", []string{"-p"}, ConflictOverwrite, OutcomeOK, map[string][]byte{"m.bin": data}},
		{"no-option-uses-local-policy", nil, ConflictSkip, OutcomeRefused, map[string][]byte{"m.bin": other}},
		// No ZCRESUM and no ZMCRC: no ZCRC is sent; the default renames.
		{"no-option-default-renames", nil, "", OutcomeOK, map[string][]byte{"m.bin": other, "m_1.bin": data}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			writeTemp(t, src, "m.bin", data)
			writeTemp(t, dst, "m.bin", other)
			p := l.start(t, src, "sz", append(append([]string{"-b", "-q"}, tc.args...), "m.bin")...)
			res, out, err := runGoAgainst(t, p, ZModemOptions{Receive: true, DestDir: dst, Conflict: tc.policy})
			perr := p.wait(t)
			if err != nil && !(tc.status == OutcomeRefused && res.Outcome == OutcomeRefused) {
				t.Fatalf("go err=%v sz err=%v\n%s\n%s", err, perr, out, p.stderr.String())
			}
			if got := res.Files[0].Status; got != tc.status {
				t.Fatalf("status = %s (%s), want %s", got, res.Files[0].Reason, tc.status)
			}
			for name, want := range tc.files {
				mustEqualFile(t, filepath.Join(dst, name), want)
			}
		})
	}
}

// The Go sender's default ZMCRC: lrzsz rz receives the file normally (it
// treats ZMCRC as a plain transfer) and the sender answers its requests.
func TestLrzszIdenticalFileWithGoSender(t *testing.T) {
	l := needLrzsz(t)
	dst := t.TempDir()
	data := zPattern(10_000, 26)
	p := l.start(t, dst, "rz", "-b", "-q")
	_, out, err := runGoAgainst(t, p, ZModemOptions{Files: []ZSendFile{{Name: "i.bin", Data: data}}})
	perr := p.wait(t)
	if err != nil || perr != nil {
		t.Fatalf("go err=%v rz err=%v\n%s\n%s", err, perr, out, p.stderr.String())
	}
	mustEqualFile(t, filepath.Join(dst, "i.bin"), data)
}

func (p *lrzszProc) Close() error {
	_ = p.stdin.Close()
	return nil
}

// The production path: lrzsz plays the device behind a session worker, and
// the Go side dials the worker, claims the port and transfers.
func TestLrzszThroughSessionWorkerClaim(t *testing.T) {
	l := needLrzsz(t)
	for _, dir := range []string{"device-sends", "device-receives"} {
		t.Run(dir, func(t *testing.T) {
			devDir, hostDir := t.TempDir(), t.TempDir()
			data := zPattern(500_000, 40)
			var p *lrzszProc
			if dir == "device-sends" {
				writeTemp(t, devDir, "dev.bin", data)
				p = l.start(t, devDir, "sz", "-b", "-q", "dev.bin")
			} else {
				p = l.start(t, devDir, "rz", "-b", "-q")
			}
			address := freeTCPAddress(t)
			stop := make(chan struct{})
			errCh := make(chan error, 1)
			go func() {
				errCh <- RunSessionServer(SessionServerOptions{
					ControlAddress: address, Port: "COM7", Baud: 3000000, Stop: stop,
					OpenPort: func(string, int) (SerialPort, error) { return p, nil },
				})
			}()
			t.Cleanup(func() { close(stop); <-errCh })
			waitForTCPServer(t, address)

			var res ZModemResult
			var out bytes.Buffer
			opts := ZModemOptions{Address: address, Result: &res, Output: &out, HandshakeWait: 20 * time.Second}
			if dir == "device-sends" {
				opts.Receive = true
				opts.DestDir = hostDir
			} else {
				opts.Files = []ZSendFile{{Path: writeTemp(t, hostDir, "host.bin", data)}}
			}
			if err := TransferZModem(opts); err != nil {
				t.Fatalf("transfer: %v\n%s\nlrzsz stderr:\n%s", err, out.String(), p.stderr.String())
			}
			if err := p.wait(t); err != nil {
				t.Fatalf("lrzsz exit: %v\n%s", err, p.stderr.String())
			}
			if dir == "device-sends" {
				mustEqualFile(t, filepath.Join(hostDir, "dev.bin"), data)
			} else {
				mustEqualFile(t, filepath.Join(devDir, "host.bin"), data)
			}
			if res.Outcome != OutcomeOK {
				t.Fatalf("outcome = %s", res.Outcome)
			}
		})
	}
}
