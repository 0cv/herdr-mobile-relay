package history

import "io"

func ANSISequenceRange(reader io.RuneReader) (int, int) {
	match := ansiRe.FindReaderIndex(reader)
	if match == nil {
		return -1, -1
	}
	return match[0], match[1]
}
