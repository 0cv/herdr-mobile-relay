package conversation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
)

func (r *Reader) ChangeToken(ctx context.Context, scope BrowseScope) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if normalizedAgent(scope.Provider) == "opencode" || isHermesAgent(scope.Provider) {
		var databases []string
		if isHermesAgent(scope.Provider) {
			databases, _ = r.hermes.databases()
		} else {
			databases, _ = r.openCode.databases()
		}
		digest := sha256.New()
		for _, database := range databases {
			for _, path := range []string{database, database + "-wal"} {
				info, err := os.Stat(path)
				if err != nil {
					fmt.Fprintln(digest, path, "unavailable")
					continue
				}
				fmt.Fprintln(digest, path, fileIdentity(info), info.Size(), info.ModTime().UnixNano(), fileChangeToken(info))
			}
			fmt.Fprintln(digest, walIndexHeader(database))
		}
		return fmt.Sprintf("%x", digest.Sum(nil)), nil
	}
	if isAgyAgent(scope.Provider) {
		digest := sha256.New()
		for _, database := range r.agy.databasePaths(scope.SessionID) {
			for _, path := range []string{database, database + "-wal"} {
				info, err := os.Stat(path)
				if err != nil {
					fmt.Fprintln(digest, path, "unavailable")
					continue
				}
				fmt.Fprintln(digest, path, fileIdentity(info), info.Size(), info.ModTime().UnixNano(), fileChangeToken(info))
			}
		}
		return fmt.Sprintf("%x", digest.Sum(nil)), nil
	}
	if IsKimi(scope.Provider) {
		return r.kimiChangeToken(ctx, scope)
	}
	location := r.LocateWithProject(scope.Provider, ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, scope.SessionID)
	if normalizedAgent(scope.Provider) == "omo" || normalizedAgent(scope.Provider) == "ohmyopencode" {
		location, _, _ = r.locateOMO(scope.CWD, scope.SessionID)
	}
	if location.Path == "" {
		return "", nil
	}
	digest := sha256.New()
	if isClaudeProvider(scope.Provider) {
		chain, err := resolveClaudeChain(ctx, location, scope.SessionID)
		if err != nil {
			return "", err
		}
		fmt.Fprintln(digest, chain.IncompleteReason)
		for _, segment := range chain.Segments {
			fmt.Fprintln(digest, segment.Location.Path, segment.FileRevision, segment.CapturedEnd, segment.SourceModTime, segment.SourceChangeToken, segment.FooterDigest)
		}
		return fmt.Sprintf("%x", digest.Sum(nil)), nil
	}
	source, err := captureFileSource(location)
	if err != nil {
		return "", err
	}
	defer source.close()
	fmt.Fprintln(digest, location.Path, source.revision, source.end, source.info.ModTime().UnixNano(), fileChangeToken(source.info))
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
