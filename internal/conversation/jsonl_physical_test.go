package conversation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLPhysicalReadsIncludeBoundaryReadAheadAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	text := "clipped\n" + strings.Repeat("{}\n", 50000)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, start := range []int64{0, 3, 8} {
		t.Run(string(rune('a'+start)), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, err := NewJSONLRecordReader(ctx, file, start, int64(len(text)), 1024, nil)
			if err != nil {
				t.Fatal(err)
			}
			var reads int64
			reader.SetReadObserver(func(n int64) {
				reads += n
				if n > 1 {
					cancel()
				}
			})
			_, err = reader.Next()
			if !errors.Is(err, context.Canceled) {
				_, err = reader.Next()
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel = %v", err)
			}
			want := int64(defaultJSONLBufferBytes)
			if start > 0 {
				want++
			}
			if reads != want {
				t.Fatalf("underlying reads = %d, want buffered read plus boundary %d", reads, want)
			}
			_, _ = reader.Next()
			if reads != want {
				t.Fatal("cancelled reader did more I/O")
			}
			t.Logf("start=%d physical=%d after cancellation (consumed fragments are smaller)", start, reads)
		})
	}
	reader, err := NewJSONLRecordReader(context.Background(), file, 3, int64(len(text)), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reads int64
	reader.SetReadObserver(func(n int64) { reads += n })
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if reads != int64(len(text))-3+1 {
		t.Fatalf("complete clipped range physical=%d", reads)
	}
}
