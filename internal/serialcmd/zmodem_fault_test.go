package serialcmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// faultEvery changes the data direction at fixed byte offsets.
func faultEvery(step int64, change func(chunk []byte, i int) []byte) func(int64, []byte) []byte {
	next := step
	return func(off int64, chunk []byte) []byte {
		for next >= off && next < off+int64(len(chunk)) {
			chunk = change(chunk, int(next-off))
			next += step
		}
		return chunk
	}
}

func TestZModemRecoversFromLineFaults(t *testing.T) {
	cases := []struct {
		name  string
		fault func(int64, []byte) []byte
	}{
		{"flip-bits", faultEvery(37_000, func(c []byte, i int) []byte { c[i] ^= 0x24; return c })},
		{"drop-bytes", faultEvery(41_000, func(c []byte, i int) []byte { return append(c[:i:i], c[i+1:]...) })},
		{"insert-garbage", faultEvery(29_000, func(c []byte, i int) []byte {
			junk := []byte("**\x18B junk \x18\x18 log line\r\n")
			return append(append(append([]byte(nil), c[:i]...), junk...), c[i:]...)
		})},
		{"truncate-subpacket", faultEvery(53_000, func(c []byte, i int) []byte { return c[:i] })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			data := zPattern(400_000, 11)
			a, b := newZWire(zWireConfig{fault: tc.fault}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			r := runZPair(t, a, b, ZModemOptions{Files: []ZSendFile{{Name: "f.bin", Data: data}}}, ZModemOptions{DestDir: dst})
			r.check(t)
			mustEqualFile(t, filepath.Join(dst, "f.bin"), data)
			f := r.send.Files[0]
			if f.Retransmits == 0 {
				t.Fatal("faults caused no ZRPOS")
			}
			if f.MinBlock >= defaultZSubpacket {
				t.Fatalf("subpacket never shrank: %d..%d", f.MinBlock, f.MaxBlock)
			}
			t.Logf("zrpos=%d subpacket=%d..%d crc-errors=%d", f.Retransmits, f.MinBlock, f.MaxBlock, r.recv.Files[0].CRCErrors)
		})
	}
}

func TestZModemReverseChannelFaultsStillComplete(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(200_000, 12)
	back := faultEvery(200, func(c []byte, i int) []byte { c[i] ^= 0x01; return c })
	a, b := newZWire(zWireConfig{}, zWireConfig{fault: back})
	t.Cleanup(func() { _ = a.Close() })
	r := runZPair(t, a, b, ZModemOptions{Files: []ZSendFile{{Name: "r.bin", Data: data}}, Window: 8192}, ZModemOptions{DestDir: dst})
	r.check(t)
	mustEqualFile(t, filepath.Join(dst, "r.bin"), data)
}

// A cut link leaves the partial data in .zpart; the next transfer of the
// same file resumes from it after the prefix CRC matches.
func TestZModemResumesAfterDisconnect(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(500_000, 13)
	a, b := newZWire(zWireConfig{cutAt: 250_000}, zWireConfig{})
	r := runZPair(t, a, b, ZModemOptions{Files: []ZSendFile{{Name: "cut.bin", Data: data}}}, ZModemOptions{DestDir: dst})
	if r.sendErr == nil || r.recvErr == nil {
		t.Fatalf("cut link: send=%v recv=%v", r.sendErr, r.recvErr)
	}
	if r.recv.Outcome != OutcomeFailed {
		t.Fatalf("recv outcome = %s", r.recv.Outcome)
	}
	part := filepath.Join(dst, "cut.bin.zpart")
	info, err := os.Stat(part)
	if err != nil || info.Size() == 0 || info.Size() >= int64(len(data)) {
		t.Fatalf("zpart after cut: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "cut.bin")); err == nil {
		t.Fatal("final name exists after a cut transfer")
	}
	kept := info.Size()

	a2, b2 := zPipePair(t)
	r2 := runZPair(t, a2, b2, ZModemOptions{Files: []ZSendFile{{Name: "cut.bin", Data: data}}}, ZModemOptions{DestDir: dst})
	r2.check(t)
	mustEqualFile(t, filepath.Join(dst, "cut.bin"), data)
	f := r2.recv.Files[0]
	if f.Status != OutcomeResumed || f.ResumeFrom != kept || r2.recv.Outcome != OutcomeResumed || r2.send.Outcome != OutcomeResumed {
		t.Fatalf("resume: %+v outcome=%s/%s, want resumed from %d", f, r2.send.Outcome, r2.recv.Outcome, kept)
	}
	if f.Bytes != int64(len(data))-kept {
		t.Fatalf("resent %d bytes, want %d", f.Bytes, int64(len(data))-kept)
	}
	if _, err := os.Stat(part); err == nil {
		t.Fatal(".zpart left after the resumed transfer")
	}
}

func TestZModemResumeChecksPrefixCRC(t *testing.T) {
	data := zPattern(100_000, 14)
	corrupt := append([]byte(nil), data[:40_000]...)
	corrupt[100] ^= 0xff
	cases := []struct {
		name       string
		part       []byte
		send       ZModemOptions
		wantFrom   int64
		wantStatus string
		want       []byte
	}{
		{"matching-prefix", data[:40_000], ZModemOptions{}, 40_000, OutcomeResumed, data},
		{"different-prefix", corrupt, ZModemOptions{}, 0, OutcomeOK, data},
		{"longer-than-source", append(append([]byte(nil), data...), 1, 2, 3), ZModemOptions{}, 0, OutcomeOK, data},
		// A sender that does not answer ZCRC gets no unchecked resume.
		{"no-zcrc-answer", corrupt, ZModemOptions{ignoreZCRC: true}, 0, OutcomeOK, data},
		// Without ZCRESUM the receiver does not ask and starts over.
		{"no-zcresum", data[:40_000], ZModemOptions{NoResume: true}, 0, OutcomeOK, data},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			writeTemp(t, dst, "p.bin.zpart", tc.part)
			send := tc.send
			send.Files = []ZSendFile{{Name: "p.bin", Data: data}}
			a, b := zPipePair(t)
			// The receiver waits this long for a ZCRC answer that never comes.
			r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst, crcWait: time.Second})
			r.check(t)
			f := r.recv.Files[0]
			if f.ResumeFrom != tc.wantFrom || f.Status != tc.wantStatus {
				t.Fatalf("resume from %d status %s, want %d %s", f.ResumeFrom, f.Status, tc.wantFrom, tc.wantStatus)
			}
			mustEqualFile(t, filepath.Join(dst, "p.bin"), tc.want)
		})
	}
}

func TestZModemConflictPolicies(t *testing.T) {
	data := zPattern(20_000, 15)
	other := zPattern(20_000, 16)
	cases := []struct {
		name     string
		existing []byte
		policy   ConflictPolicy
		// The sender's management option overrides the receiver policy.
		senderPolicy ConflictPolicy
		wantStatus   string
		wantFiles    map[string][]byte
	}{
		{"default-identical-skips", data, "", ConflictSkipIdenticalElseRename, OutcomeSkipped, map[string][]byte{"c.bin": data}},
		{"default-different-renames", other, "", ConflictSkipIdenticalElseRename, OutcomeOK, map[string][]byte{"c.bin": other, "c_1.bin": data}},
		{"receiver-overwrite", other, ConflictOverwrite, ConflictSkipIdenticalElseRename, OutcomeOK, map[string][]byte{"c.bin": data}},
		{"sender-clobber", other, "", ConflictOverwrite, OutcomeOK, map[string][]byte{"c.bin": data}},
		{"sender-protect", other, "", ConflictSkip, OutcomeRefused, map[string][]byte{"c.bin": other}},
		{"sender-rename", data, "", ConflictRename, OutcomeOK, map[string][]byte{"c.bin": data, "c_1.bin": data}},
		// An explicit receiver policy wins over the sender's ZF1.
		{"explicit-overwrite-beats-protect", other, ConflictOverwrite, ConflictSkip, OutcomeOK, map[string][]byte{"c.bin": data}},
		{"explicit-skip-beats-zmcrc", other, ConflictSkip, ConflictSkipIdenticalElseRename, OutcomeRefused, map[string][]byte{"c.bin": other}},
		{"explicit-skip-beats-clobber", other, ConflictSkip, ConflictOverwrite, OutcomeRefused, map[string][]byte{"c.bin": other}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			writeTemp(t, dst, "c.bin", tc.existing)
			a, b := zPipePair(t)
			r := runZPair(t, a, b,
				ZModemOptions{Files: []ZSendFile{{Name: "c.bin", Data: data}}, Conflict: tc.senderPolicy},
				ZModemOptions{DestDir: dst, Conflict: tc.policy})
			r.checkOutcome(t, tc.wantStatus)
			if got := r.recv.Files[0].Status; got != tc.wantStatus {
				t.Fatalf("status = %s (%s), want %s", got, r.recv.Files[0].Reason, tc.wantStatus)
			}
			for name, want := range tc.wantFiles {
				mustEqualFile(t, filepath.Join(dst, name), want)
			}
			entries, _ := os.ReadDir(dst)
			if len(entries) != len(tc.wantFiles) {
				t.Fatalf("dir has %d entries, want %d", len(entries), len(tc.wantFiles))
			}
		})
	}
}

// The local policy applies when the sender gives no management option, as
// lrzsz sz does by default.
func TestZModemReceiverPolicyWithoutSenderOption(t *testing.T) {
	data := zPattern(5000, 17)
	other := zPattern(5000, 18)
	for _, tc := range []struct {
		policy ConflictPolicy
		status string
		files  map[string][]byte
	}{
		{ConflictSkip, OutcomeRefused, map[string][]byte{"n.bin": other}},
		{ConflictRename, OutcomeOK, map[string][]byte{"n.bin": other, "n_1.bin": data}},
		{ConflictSkipIdenticalElseRename, OutcomeOK, map[string][]byte{"n.bin": other, "n_1.bin": data}},
		{ConflictOverwrite, OutcomeOK, map[string][]byte{"n.bin": data}},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			dst := t.TempDir()
			writeTemp(t, dst, "n.bin", other)
			a, b := zPipePair(t)
			send := ZModemOptions{Files: []ZSendFile{{Name: "n.bin", Data: data}}, Conflict: zConflictNone}
			r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst, Conflict: tc.policy})
			r.checkOutcome(t, tc.status)
			if got := r.recv.Files[0].Status; got != tc.status {
				t.Fatalf("status = %s, want %s", got, tc.status)
			}
			for name, want := range tc.files {
				mustEqualFile(t, filepath.Join(dst, name), want)
			}
		})
	}
}

func TestZModemBatchSkipsBadNameAndContinues(t *testing.T) {
	dst := t.TempDir()
	good := zPattern(3000, 19)
	a, b := zPipePair(t)
	r := runZPair(t, a, b, ZModemOptions{Files: []ZSendFile{
		{Name: "..", Data: []byte("x")},
		{Name: "ok.bin", Data: good},
	}}, ZModemOptions{DestDir: dst})
	if r.recvErr == nil || r.recv.Outcome != OutcomePartial {
		t.Fatalf("recv err=%v outcome=%s, want a partial batch", r.recvErr, r.recv.Outcome)
	}
	mustEqualFile(t, filepath.Join(dst, "ok.bin"), good)
	if len(r.recv.Files) != 2 || r.recv.Files[0].Status != OutcomeFailed || r.recv.Files[1].Status != OutcomeOK {
		t.Fatalf("files = %+v", r.recv.Files)
	}
}

func TestSanitizeTransferName(t *testing.T) {
	for raw, want := range map[string]string{
		"a.wav":               "a.wav",
		"//mnt/rec/a.wav":     "a.wav",
		`C:\Windows\x.dll`:    "x.dll",
		"../../etc/passwd":    "passwd",
		"..":                  "",
		"dir/":                "",
		"con.txt":             "_con.txt",
		"a<b>c|d?.txt":        "a_b_c_d_.txt",
		"name. ":              "name",
		"tab\there":           "tab_here",
		"COM3":                "_COM3",
		"normal name (1).bin": "normal name (1).bin",
	} {
		if got := SanitizeTransferName(raw); got != want {
			t.Errorf("SanitizeTransferName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestZModemCancelFromEitherSide(t *testing.T) {
	data := zPattern(2_000_000, 20)
	cfg := zWireConfig{rate: 1 << 20}
	for _, side := range []string{"sender", "receiver"} {
		t.Run(side, func(t *testing.T) {
			dst := t.TempDir()
			a, b := newZWire(cfg, cfg)
			t.Cleanup(func() { _ = a.Close() })
			cancel := make(chan struct{})
			send := ZModemOptions{Files: []ZSendFile{{Name: "big.bin", Data: data}}}
			recv := ZModemOptions{DestDir: dst}
			if side == "sender" {
				send.Cancel = cancel
			} else {
				recv.Cancel = cancel
			}
			time.AfterFunc(300*time.Millisecond, func() { close(cancel) })
			r := runZPair(t, a, b, send, recv)
			if r.send.Outcome != OutcomeCancelled || r.recv.Outcome != OutcomeCancelled {
				t.Fatalf("outcomes = %s/%s (send=%v recv=%v)", r.send.Outcome, r.recv.Outcome, r.sendErr, r.recvErr)
			}
			if _, err := os.Stat(filepath.Join(dst, "big.bin.zpart")); err != nil {
				t.Fatalf("cancelled transfer must keep .zpart: %v", err)
			}
		})
	}
}

func TestZModemFileInfoToleratesMissingAndExtraFields(t *testing.T) {
	for raw, want := range map[string]ZFileInfo{
		"a.bin\x00":       {Name: "a.bin", Size: -1},
		"a.bin\x0012\x00": {Name: "a.bin", Size: 12},
		"a.bin\x0012 14752305671 100644 0 3 99 7 8": {Name: "a.bin", Size: 12, ModTime: time.Unix(0o14752305671, 0), Mode: 0o100644, FilesLeft: 3, BytesLeft: 99},
		"a.bin": {Name: "a.bin", Size: -1},
	} {
		got := parseZFileInfo([]byte(raw))
		if got.Name != want.Name || got.Size != want.Size || !got.ModTime.Equal(want.ModTime) || got.Mode != want.Mode || got.FilesLeft != want.FilesLeft || got.BytesLeft != want.BytesLeft {
			t.Errorf("parse %q = %+v, want %+v", raw, got, want)
		}
	}
}

// Without a size the receiver takes whatever the sender sends.
func TestZModemReceivesFileWithoutSize(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(10_000, 21)
	a, b := zPipePair(t)
	send := ZModemOptions{Files: []ZSendFile{{Name: "nosize.bin", Data: data}}, noSize: true}
	r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst})
	r.check(t)
	mustEqualFile(t, filepath.Join(dst, "nosize.bin"), data)
}

func TestZModemRefusesRemoteCommand(t *testing.T) {
	a, b := zPipePair(t)
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: b, Receive: true, DestDir: t.TempDir(), HandshakeWait: 3 * time.Second, Result: &res})
	}()
	l := newZLink(a, nil)
	s := &zSession{l: l, opts: &ZModemOptions{DataWait: time.Second}, res: &ZModemResult{}, out: os.Stderr, crc32: true}
	if h, err := l.readHeader(2 * time.Second); err != nil || h.Type != zRINIT {
		t.Fatalf("first header = %v %v", h, err)
	}
	if err := s.sendBin(zPosHeader(zCOMMAND, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.sendData([]byte("rm -rf /\x00"), zCRCW); err != nil {
		t.Fatal(err)
	}
	for {
		h, err := l.readHeader(2 * time.Second)
		if err != nil {
			t.Fatalf("waiting for ZCOMPL: %v", err)
		}
		if h.Type == zCOMPL {
			if h.pos() == 0 {
				t.Fatal("ZCOMPL status 0: command reported as run")
			}
			break
		}
	}
	_ = l.sendHex(zPosHeader(zFIN, 0))
	if err := <-done; err != nil {
		t.Fatalf("receiver: %v", err)
	}
}
