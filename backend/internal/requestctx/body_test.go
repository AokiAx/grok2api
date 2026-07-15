package requestctx

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBodyCacheLoadsOnceAndReusesSlice(t *testing.T) {
	ctx, cache := WithBodyCache(context.Background())
	ctxAgain, same := WithBodyCache(ctx)
	if ctxAgain != ctx || same != cache || BodyCacheFromContext(ctx) != cache {
		t.Fatal("context did not retain one shared body cache")
	}

	loads := 0
	first, err := cache.Load(func() ([]byte, error) {
		loads++
		return []byte("payload"), nil
	})
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := cache.Load(func() ([]byte, error) {
		loads++
		return []byte("different"), nil
	})
	if err != nil || loads != 1 || len(first) == 0 || &first[0] != &second[0] {
		t.Fatalf("loads=%d first=%q second=%q err=%v", loads, first, second, err)
	}
	peeked, ok := cache.Snapshot()
	if !ok || &peeked[0] != &first[0] {
		t.Fatalf("peeked=%q ok=%v", peeked, ok)
	}
}

func TestReadBoundedAcceptsLimitAndRejectsOverflow(t *testing.T) {
	body, err := ReadBounded(strings.NewReader("1234"), 4)
	if err != nil || string(body) != "1234" {
		t.Fatalf("exact limit body=%q err=%v", body, err)
	}
	if _, err := ReadBounded(strings.NewReader("12345"), 4); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("overflow err=%v", err)
	}
	if _, err := ReadBounded(strings.NewReader(""), 0); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("invalid limit err=%v", err)
	}
	readerError := errors.New("reader failed")
	if _, err := ReadBounded(errorReader{err: readerError}, 4); !errors.Is(err, readerError) {
		t.Fatalf("reader err=%v", err)
	}
}

func TestBodyCacheHandlesNilContextAndMemoizesErrors(t *testing.T) {
	ctx, cache := WithBodyCache(nil)
	if ctx == nil || cache == nil || BodyCacheFromContext(ctx) != cache {
		t.Fatal("nil context did not receive a cache")
	}
	if BodyCacheFromContext(nil) != nil {
		t.Fatal("nil context returned a cache")
	}
	if body, loaded := (*BodyCache)(nil).Snapshot(); body != nil || loaded {
		t.Fatalf("nil snapshot body=%q loaded=%v", body, loaded)
	}

	wantErr := errors.New("load failed")
	loads := 0
	for range 2 {
		body, err := cache.Load(func() ([]byte, error) {
			loads++
			return nil, wantErr
		})
		if body != nil || !errors.Is(err, wantErr) {
			t.Fatalf("body=%q err=%v", body, err)
		}
	}
	if loads != 1 {
		t.Fatalf("error loader calls=%d want=1", loads)
	}
	if body, loaded := cache.Snapshot(); body != nil || loaded {
		t.Fatalf("failed snapshot body=%q loaded=%v", body, loaded)
	}
}

func TestStickyKeyContext(t *testing.T) {
	if StickyKey(nil) != "" {
		t.Fatal("nil context returned sticky key")
	}
	ctx := WithStickyKey(nil, "")
	if StickyKey(ctx) != "" {
		t.Fatal("empty sticky key was attached")
	}
	ctx = WithStickyKey(ctx, "session-1")
	if StickyKey(ctx) != "session-1" {
		t.Fatalf("sticky key=%q", StickyKey(ctx))
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = errorReader{}
