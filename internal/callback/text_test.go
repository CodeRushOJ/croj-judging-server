package callback

import (
	"strings"
	"testing"
)

func TestTruncateUTF16PreservesNonBMPBoundary(t *testing.T) {
	value := strings.Repeat("😀", 32_769)
	truncated := TruncateUTF16(value, 65_536)
	if got := UTF16Len(truncated); got != 65_536 {
		t.Fatalf("UTF-16 length = %d", got)
	}
	if truncated != strings.Repeat("😀", 32_768) {
		t.Fatal("truncate split or retained a non-BMP code point beyond the limit")
	}
}

func TestValidateResultUsesUTF16CodeUnits(t *testing.T) {
	result := validResult()
	result.Stdout = strings.Repeat("😀", 32_768)
	if err := validateResult(result); err != nil {
		t.Fatalf("boundary result rejected: %v", err)
	}
	result.Stdout += "😀"
	if err := validateResult(result); err == nil {
		t.Fatal("result beyond the backend UTF-16 boundary was accepted")
	}
}
