package conversation

import (
	"reflect"
	"testing"
)

func TestBrowseFailurePages(t *testing.T) {
	tests := []struct {
		name      string
		page      BrowsePage
		code      string
		message   string
		retryable bool
	}{
		{
			name: "retryable failure", page: failPage("query_failed", "Query failed.", true),
			code: "query_failed", message: "Query failed.", retryable: true,
		},
		{
			name: "non-retryable failure", page: failPage("source_corrupt", "Source is corrupt.", false),
			code: "source_corrupt", message: "Source is corrupt.", retryable: false,
		},
		{
			name: "request cancelled", page: browseRequestCancelled(),
			code: "request_cancelled", message: "History request was cancelled.", retryable: true,
		},
		{
			name: "source changed", page: browseSourceChanged(),
			code: "source_changed", message: "The conversation source changed while history was being browsed.", retryable: false,
		},
		{
			name: "continuation failed", page: browseContinuationFailed(),
			code: "index_failed", message: "History continuation could not be created.", retryable: true,
		},
		{
			name: "invalid cursor", page: browseInvalidCursor(),
			code: "invalid_cursor", message: "This history cursor is invalid.", retryable: false,
		},
		{
			name: "invalid conversation cursor", page: browseInvalidConversationCursor(),
			code: "invalid_cursor", message: "This history cursor is invalid for the requested conversation.", retryable: false,
		},
		{
			name: "invalid snapshot cursor", page: browseInvalidSnapshotCursor(),
			code: "invalid_cursor", message: "This history cursor is invalid for the requested snapshot.", retryable: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := BrowsePage{
				Available: true, ReasonCode: tt.code, Reason: tt.message,
				Entries: []Entry{}, State: BrowseFailed, Mode: BrowseRecent,
				Diagnostics: BrowseDiagnostics{},
				Error:       &BrowseError{Code: tt.code, Message: tt.message, Retryable: tt.retryable},
			}
			if !reflect.DeepEqual(tt.page, want) {
				t.Fatalf("page = %#v, want %#v", tt.page, want)
			}
		})
	}
}
