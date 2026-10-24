package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

func nativeDatabaseRevision(path string, schema int, format string) string {
	identity := ""
	if info, err := os.Stat(path); err == nil {
		identity = fileIdentity(info)
	}
	wal := ""
	if file, err := openConversationSource(path + "-wal"); err == nil {
		defer file.Close()
		if info, err := file.Stat(); err == nil && info.Mode().IsRegular() {
			var header [32]byte
			if n, _ := file.ReadAt(header[:], 0); n == len(header) {
				wal = fileIdentity(info) + hex.EncodeToString(header[16:24])
			}
		}
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s", path, identity, schema, format, wal)))
	return hex.EncodeToString(digest[:])
}
