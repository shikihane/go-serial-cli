package bundledtools_test

import (
	"strings"
	"testing"

	"go-serial-cli/internal/bundledtools"
)

func TestListDoesNotIncludeThirdPartyBinaries(t *testing.T) {
	files, err := bundledtools.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}

	for _, file := range files {
		if strings.HasPrefix(file, "com0com/") {
			t.Fatalf("List includes third-party binary asset %q", file)
		}
	}
}

func TestExtractReportsNoBundledThirdPartyTools(t *testing.T) {
	err := bundledtools.Extract(t.TempDir())
	if err == nil {
		t.Fatal("Extract returned nil, want unavailable error")
	}

	if !strings.Contains(err.Error(), "no third-party tools are bundled") {
		t.Fatalf("Extract error = %v", err)
	}
}
