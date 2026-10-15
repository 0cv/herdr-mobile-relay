package herdr

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const scrollEvidenceFileLimit = 512 * 1024

type ScrollEvidenceRecord struct {
	Kind string `json:"kind"`
	Method string `json:"method,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SocketPath string `json:"socket_path"`
	Started time.Time `json:"started"`
	Duration time.Duration `json:"duration_ns"`
	Request []byte `json:"request_base64,omitempty"`
	Response []byte `json:"response_base64,omitempty"`
	WriteCalls int `json:"write_calls"`
	Written int `json:"written"`
	Wrote bool `json:"wrote"`
	Error string `json:"error,omitempty"`
	Status *ServerStatus `json:"selected_status,omitempty"`
	Decision *FeatureEvidence `json:"decision,omitempty"`
	Overflow bool `json:"overflow"`
}

type ScrollEvidence struct {
	mu sync.Mutex
	records []ScrollEvidenceRecord
	bytes int
	limit int
	count int
	failed atomic.Bool
}

func NewScrollEvidence(byteLimit, recordLimit int) (*ScrollEvidence, error) {
	if byteLimit < 1 || byteLimit > 8*1024*1024 || recordLimit < 1 || recordLimit > 1024 {
		return nil, errors.New("invalid scroll evidence limits")
	}
	return &ScrollEvidence{limit: byteLimit, count: recordLimit}, nil
}

type scrollEvidenceInstallation struct {
	observer *ScrollEvidence
}

func (c *socketAPIClient) scrollObserver() *ScrollEvidence {
	if c == nil {
		return nil
	}
	installation := c.evidence.Load()
	if installation == nil {
		empty := &scrollEvidenceInstallation{}
		if c.evidence.CompareAndSwap(nil, empty) {
			return nil
		}
		installation = c.evidence.Load()
	}
	return installation.observer
}

func (c *Client) SetScrollEvidence(observer *ScrollEvidence) error {
	if c == nil || c.api == nil || c.capabilities == nil || observer == nil || c.api.seq.Load() != 0 || c.capabilities.refreshSeq.Load() != 0 || !c.api.evidence.CompareAndSwap(nil, &scrollEvidenceInstallation{observer: observer}) {
		return errors.New("scroll evidence must be installed once before client use")
	}
	return nil
}

func (o *ScrollEvidence) record(value ScrollEvidenceRecord) {
	if o == nil {
		return
	}
	if !o.mu.TryLock() {
		o.failed.Store(true)
		return
	}
	defer o.mu.Unlock()
	size := len(value.Request) + len(value.Response) + len(value.Error) + 4096
	if value.Status != nil {
		size += len(value.Status.InstalledClientVersion) + len(value.Status.ServerVersion) + len(value.Status.SocketInstance)
		for name, feature := range value.Status.Features {
			size += len(name) + len(feature.State) + len(feature.Reason) + 128
		}
	}
	if value.Decision != nil {
		size += len(value.Decision.State) + len(value.Decision.Reason)
	}
	if value.Overflow || len(value.Request) > scrollEvidenceFileLimit || len(value.Response) > scrollEvidenceFileLimit || size > o.limit-o.bytes || len(o.records) >= o.count {
		o.failed.Store(true)
		return
	}
	value.Request = append([]byte(nil), value.Request...)
	value.Response = append([]byte(nil), value.Response...)
	o.records = append(o.records, value)
	o.bytes += size
}

func (o *ScrollEvidence) Snapshot() ([]ScrollEvidenceRecord, error) {
	if o == nil {
		return nil, errors.New("scroll evidence is not installed")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed.Load() {
		return nil, errors.New("scroll evidence overflow or concurrent snapshot loss")
	}
	result := make([]ScrollEvidenceRecord, len(o.records))
	for index, value := range o.records {
		value.Request = append([]byte(nil), value.Request...)
		value.Response = append([]byte(nil), value.Response...)
		if value.Status != nil {
			status := cloneServerStatus(*value.Status)
			value.Status = &status
		}
		if value.Decision != nil {
			decision := *value.Decision
			value.Decision = &decision
		}
		result[index] = value
	}
	return result, nil
}

type scrollEvidenceReader struct {
	reader io.Reader
	record *ScrollEvidenceRecord
}

func (r scrollEvidenceReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	remaining := scrollEvidenceFileLimit - len(r.record.Response)
	if n > remaining {
		r.record.Overflow = true
	}
	r.record.Response = append(r.record.Response, buffer[:min(n, remaining)]...)
	return n, err
}

func (c *Client) ReadScrollEvidencePane(ctx context.Context, pane, source, format string, lines int) (PaneRead, error) {
	if c == nil || c.api == nil || c.api.scrollObserver() == nil || pane == "" || (source != "visible" && source != "recent") || (format != "ansi" && format != "text") || lines < 1 || lines > 4096 {
		return PaneRead{}, errors.New("invalid observed pane read")
	}
	raw, _, err := c.api.requestUnary(ctx, "pane.read", map[string]any{"pane_id": pane, "source": source, "format": format, "lines": lines, "strip_ansi": format != "ansi"})
	if err != nil {
		return PaneRead{}, err
	}
	var result struct {
		Read struct {
			Text string `json:"text"`
			Truncated bool `json:"truncated"`
		} `json:"read"`
	}
	if err := decodeSocketResult(raw, "pane_read", &result); err != nil {
		return PaneRead{}, err
	}
	if result.Read.Truncated {
		return PaneRead{}, errors.New("observed pane read was truncated")
	}
	return PaneRead{Content: []byte(result.Read.Text)}, nil
}

func scrollEvidenceMethod(method string) bool {
	switch method {
	case "ping", "pane.process_info", "pane.read", "pane.send_text":
		return true
	default:
		return false
	}
}
