//go:build !windows

package serialcmd

import (
	"fmt"
	"time"
)

func writeSerialPortWithTimeout(port SerialPort, data []byte, _ time.Duration) error {
	return writeSerialPort(port, data)
}

func writeSerialPort(port SerialPort, data []byte) error {
	n, err := port.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write %d/%d", n, len(data))
	}
	return nil
}
