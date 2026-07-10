package mail

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

type Provider interface {
	Name() string
	HasAccounts() bool
	CreateMailbox(ctx context.Context) (email string, token string, err error)
	WaitCode(ctx context.Context, token string, email string) (string, error)
	RecordSuccess()
	RecordFailure(reason string)
}

var (
	otpDashPattern  = regexp.MustCompile(`(?i)\b([A-Z0-9]{3})-([A-Z0-9]{3})\b`)
	otpPlainPattern = regexp.MustCompile(`(?i)\b([A-Z0-9]{6})\b`)
	hintPattern     = regexp.MustCompile(`(?i)(x\.ai|grok|verification|verify|code|验证码)`)
)

func NormalizeEmailCode(code string) string {
	code = strings.TrimSpace(code)
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return strings.ToUpper(code)
}

func IsValidEmailCode(code string) bool {
	code = NormalizeEmailCode(code)
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func ExtractOTPCode(content string) string {
	if content == "" {
		return ""
	}
	if !hintPattern.MatchString(content) {
		// Still try; some providers strip branding.
	}
	if match := otpDashPattern.FindStringSubmatch(content); len(match) == 3 {
		return NormalizeEmailCode(match[1] + match[2])
	}
	if match := otpPlainPattern.FindStringSubmatch(content); len(match) == 2 {
		code := NormalizeEmailCode(match[1])
		if IsValidEmailCode(code) {
			return code
		}
	}
	return ""
}

func GeneratePassword(length int) string {
	if length < 12 {
		length = 14
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%"
	var b strings.Builder
	b.Grow(length)
	// deterministic-enough using crypto would be better; use crypto/rand below via helper.
	return randomFromAlphabet(alphabet, length)
}
