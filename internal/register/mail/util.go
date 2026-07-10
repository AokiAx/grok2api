package mail

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

func randomFromAlphabet(alphabet string, length int) string {
	var b strings.Builder
	b.Grow(length)
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// fallback unlikely path
			b.WriteByte(alphabet[i%len(alphabet)])
			continue
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

func NormalizeHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.Trim(value, "/")
	return value
}

func RequireFields(fields map[string]string) error {
	for name, value := range fields {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("missing %s", name)
		}
	}
	return nil
}
