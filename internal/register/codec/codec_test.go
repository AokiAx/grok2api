package codec_test

import (
	"bytes"
	"testing"

	"github.com/AokiAx/grok2api/internal/register/codec"
)

func TestEncodeStringFieldAndGRPCWebRoundTrip(t *testing.T) {
	payload := codec.EncodeStringField(1, "user@example.com")
	frame := codec.WrapGRPCWeb(payload)
	if frame[0] != 0x00 {
		t.Fatalf("flag = %x", frame[0])
	}
	parsed := codec.ParseGRPCWebResponse(frame)
	if !bytes.Equal(parsed.Payload, payload) {
		t.Fatalf("payload mismatch: %x vs %x", parsed.Payload, payload)
	}
}

func TestParseGRPCWebTrailers(t *testing.T) {
	trailerBody := []byte("grpc-status:0\r\ngrpc-message:ok")
	frame := append([]byte{0x80, 0, 0, 0, byte(len(trailerBody))}, trailerBody...)
	parsed := codec.ParseGRPCWebResponse(frame)
	if parsed.Status != "0" {
		t.Fatalf("status = %q", parsed.Status)
	}
	if parsed.Trailers["grpc-message"] != "ok" {
		t.Fatalf("trailers = %#v", parsed.Trailers)
	}
}

func TestExtractActionID(t *testing.T) {
	const id = "7f0123456789abcdef0123456789abcdef01234567"
	if got := codec.ExtractServerActionIDFromJS("x=" + id + ";"); got != id {
		t.Fatalf("js id = %q", got)
	}
	if got := codec.ExtractServerActionIDFromHTML(`{"id":"` + id + `"}`); got != id {
		t.Fatalf("html id = %q", got)
	}
}
