package processgroup

import "bytes"

// LimitedBuffer keeps the first Limit bytes written and drops the rest, setting Truncated.
type LimitedBuffer struct {
	Limit     int
	Truncated bool
	buf       bytes.Buffer
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.Limit - b.buf.Len()
	if remaining <= 0 {
		b.Truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.Truncated = true
	}
	_, err := b.buf.Write(p)
	return original, err
}

func (b *LimitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *LimitedBuffer) String() string { return b.buf.String() }
