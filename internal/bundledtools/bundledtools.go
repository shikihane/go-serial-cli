package bundledtools

import (
	"fmt"
)

func List() ([]string, error) {
	return nil, nil
}

func Extract(dest string) error {
	if dest == "" {
		return fmt.Errorf("destination is required")
	}
	return fmt.Errorf("no third-party tools are bundled; install com0com externally and make setupc.exe discoverable on PATH")
}
