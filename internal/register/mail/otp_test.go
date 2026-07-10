package mail_test

import (
	"testing"

	"github.com/AokiAx/grok2api/internal/register/mail"
)

func TestExtractOTPCode(t *testing.T) {
	content := "Your Grok verification code is ABC-123. Do not share."
	code := mail.ExtractOTPCode(content)
	if code != "ABC123" {
		t.Fatalf("code = %q", code)
	}
	if !mail.IsValidEmailCode(code) {
		t.Fatal("expected valid code")
	}
}

func TestNormalizeEmailCode(t *testing.T) {
	if got := mail.NormalizeEmailCode("ab c-12"); got != "ABC12" {
		t.Fatalf("got %q", got)
	}
}
