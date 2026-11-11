package slashcmd

import (
	"io"
	"os"
)

type catalogReader struct {
	remaining int64
	truncated bool
}

func discoverGenericSkills(dirs []string, format string) ([]Command, bool) {
	return newCatalogReader().discoverGenericSkills(dirs, format)
}

func newCatalogReader() *catalogReader {
	return &catalogReader{remaining: maxCatalogReadSize}
}

func (reader *catalogReader) readPath(path string, maxSize, limit int64) ([]byte, bool) {
	if reader.truncated {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	return reader.readFile(file, maxSize, limit)
}

func (reader *catalogReader) readFile(file *os.File, maxSize, limit int64) ([]byte, bool) {
	if reader.truncated {
		return nil, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, false
	}
	limit = min(limit, info.Size())
	if reader.remaining < limit {
		reader.truncated = true
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	reader.remaining -= int64(len(data))
	reader.truncated = reader.truncated || reader.remaining == 0
	if err != nil || int64(len(data)) > maxSize {
		return nil, false
	}
	return data, true
}
