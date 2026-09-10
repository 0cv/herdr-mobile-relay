package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

const defaultJSONLBufferBytes = 64 * 1024

var (
	errJSONLSourceTruncated = errors.New("jsonl source ended before the captured range")
	errJSONLCheckpoint      = errors.New("jsonl scan checkpoint stopped the reader")
)

type JSONLRecord struct {
	Start        int64
	End          int64
	Raw          []byte
	Complete     bool
	Oversized    bool
	StartsInside bool
	Trailing     bool
}

type JSONLRecordProgress struct {
	ScannedBytes int64
	SourceBytes  int64
}

type JSONLRecordReader struct {
	ctx          context.Context
	file         *os.File
	reader       *bufio.Reader
	start        int64
	end          int64
	position     int64
	maxBytes     int64
	startsInside bool
	progress     func(JSONLRecordProgress)
	digest       io.Writer
	checkpoint   func() error
}

func NewJSONLRecordReader(
	ctx context.Context,
	file *os.File,
	start, end, maxBytes int64,
	progress func(JSONLRecordProgress),
) (*JSONLRecordReader, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if file == nil {
		return nil, errors.New("jsonl record reader requires a file")
	}
	if start < 0 || end < start || maxBytes < 1 {
		return nil, errors.New("invalid jsonl record range")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("jsonl source is not a regular file")
	}
	if end > info.Size() {
		return nil, errors.New("jsonl record range exceeds source")
	}
	startsInside := false
	if start > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], start-1); err != nil {
			return nil, err
		}
		startsInside = previous[0] != '\n'
	}
	section := io.NewSectionReader(file, start, end-start)
	return &JSONLRecordReader{
		ctx:          ctx,
		file:         file,
		reader:       bufio.NewReaderSize(section, defaultJSONLBufferBytes),
		start:        start,
		end:          end,
		position:     start,
		maxBytes:     maxBytes,
		startsInside: startsInside,
		progress:     progress,
	}, nil
}

func (r *JSONLRecordReader) SetDigestWriter(output io.Writer) {
	r.digest = output
}

func (r *JSONLRecordReader) SetCheckpoint(checkpoint func() error) {
	r.checkpoint = checkpoint
}

func (r *JSONLRecordReader) check() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.checkpoint != nil {
		if err := r.checkpoint(); err != nil {
			return err
		}
	}
	return nil
}

func (r *JSONLRecordReader) Next() (JSONLRecord, error) {
	for {
		if err := r.check(); err != nil {
			return JSONLRecord{}, err
		}
		if r.position >= r.end {
			if info, err := r.file.Stat(); err == nil && info.Size() < r.end {
				return JSONLRecord{}, errJSONLSourceTruncated
			}
			return JSONLRecord{}, io.EOF
		}

		record := JSONLRecord{Start: r.position}
		var raw []byte
		var rawBytes int64
		overSized := false
		hadNewline := false

		for {
			if err := r.check(); err != nil {
				return JSONLRecord{}, err
			}
			fragment, err := r.reader.ReadSlice('\n')
			if len(fragment) > 0 {
				if r.digest != nil {
					if _, writeErr := r.digest.Write(fragment); writeErr != nil {
						return JSONLRecord{}, writeErr
					}
				}
				r.position += int64(len(fragment))
				r.reportProgress()
				content := fragment
				if content[len(content)-1] == '\n' {
					hadNewline = true
					content = content[:len(content)-1]
					if len(content) > 0 && content[len(content)-1] == '\r' {
						content = content[:len(content)-1]
					}
				}
				if !overSized {
					if rawBytes+int64(len(content)) > r.maxBytes {
						overSized = true
						raw = nil
					} else {
						raw = append(raw, content...)
						rawBytes += int64(len(content))
					}
				}
			}

			switch {
			case err == nil:
				break
			case errors.Is(err, bufio.ErrBufferFull):
				continue
			case errors.Is(err, io.EOF):
				if r.position < r.end {
					return JSONLRecord{}, errJSONLSourceTruncated
				}
				break
			default:
				return JSONLRecord{}, err
			}
			break
		}

		record.End = r.position
		record.StartsInside = record.Start == r.start && r.startsInside
		if record.StartsInside {
			if record.End >= r.end {
				return JSONLRecord{}, io.EOF
			}
			continue
		}
		if overSized {
			record.Oversized = true
			record.Trailing = !hadNewline
			record.Complete = hadNewline
			return record, nil
		}
		if strings.TrimSpace(string(raw)) == "" {
			continue
		}
		record.Raw = raw
		record.Oversized = false
		record.Trailing = !hadNewline
		record.Complete = hadNewline || json.Valid(raw)
		if !hadNewline && !record.Complete {
			record.Trailing = true
		}
		return record, nil
	}
}

func (r *JSONLRecordReader) reportProgress() {
	if r.progress == nil {
		return
	}
	r.progress(JSONLRecordProgress{
		ScannedBytes: r.position - r.start,
		SourceBytes:  r.end - r.start,
	})
}

func collectJSONLRecords(
	ctx context.Context,
	file *os.File,
	start, end, maxBytes int64,
	progress func(JSONLRecordProgress),
) ([]JSONLRecord, int, error) {
	reader, err := NewJSONLRecordReader(ctx, file, start, end, maxBytes, progress)
	if err != nil {
		return nil, 0, err
	}
	records := make([]JSONLRecord, 0)
	overSized := 0
	for {
		record, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return records, overSized, nil
		}
		if err != nil {
			return nil, overSized, err
		}
		if record.Oversized {
			overSized++
			continue
		}
		records = append(records, record)
	}
}
