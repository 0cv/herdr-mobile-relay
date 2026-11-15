package conversation

import (
	"context"
	"os"
)

func (r *Reader) ReadKimiSource(ctx context.Context, scope BrowseScope, limit int) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !IsKimi(scope.Provider) || !scope.InferredSession || scope.SessionID == "" || scope.SelectionReason != "" || scope.SelectionSource == "" {
		return nil, ErrUnavailable
	}
	r.kimiMu.Lock()
	defer r.kimiMu.Unlock()
	id, location, code := r.SelectKimiSession(ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, "")
	if code != "" || id != scope.SessionID || KimiSourceIdentity(location) != scope.SelectionSource {
		return nil, ErrUnavailable
	}
	info, err := os.Stat(location.Path)
	if err != nil {
		return nil, ErrUnavailable
	}
	projection, _, code := r.readKimiLocation(ctx, location)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := os.Stat(location.Path)
	if err != nil || !os.SameFile(info, after) || kimiStatToken(after, false) != kimiStatToken(info, false) || !r.KimiSelectionCurrent(scope) {
		return nil, ErrUnavailable
	}
	if code != "" {
		return nil, ErrUnavailable
	}
	return latest(projection.entries, limit), nil
}
