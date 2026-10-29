package conversation

import "encoding/binary"

// pbField is one decoded protobuf field. Antigravity stores each conversation
// step as a schema-less protobuf blob, so the reader walks the wire format and
// picks fields by number.
type pbField struct {
	num    int
	varint uint64
	data   []byte
	isData bool
}

// parsePB decodes one protobuf message. It reports false for anything that is
// not a well-formed message so callers can treat a corrupt blob as such.
func parsePB(b []byte) ([]pbField, bool) {
	fields := make([]pbField, 0, 8)
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, false
		}
		b = b[n:]
		num, wire := int(key>>3), key&7
		if num <= 0 {
			return nil, false
		}
		switch wire {
		case 0:
			value, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, false
			}
			b = b[n:]
			fields = append(fields, pbField{num: num, varint: value})
		case 1:
			if len(b) < 8 {
				return nil, false
			}
			b = b[8:]
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return nil, false
			}
			fields = append(fields, pbField{num: num, data: b[n : n+int(size)], isData: true})
			b = b[n+int(size):]
		case 5:
			if len(b) < 4 {
				return nil, false
			}
			b = b[4:]
		default:
			return nil, false
		}
	}
	return fields, true
}

// pbBytes returns the first length-delimited field with the given number.
func pbBytes(fields []pbField, num int) ([]byte, bool) {
	for _, field := range fields {
		if field.num == num && field.isData {
			return field.data, true
		}
	}
	return nil, false
}

// pbAll returns every length-delimited field with the given number.
func pbAll(fields []pbField, num int) [][]byte {
	var out [][]byte
	for _, field := range fields {
		if field.num == num && field.isData {
			out = append(out, field.data)
		}
	}
	return out
}

// pbPath follows nested messages through the given field numbers and returns
// the final length-delimited value.
func pbPath(fields []pbField, path ...int) ([]byte, bool) {
	for index, num := range path {
		data, ok := pbBytes(fields, num)
		if !ok {
			return nil, false
		}
		if index == len(path)-1 {
			return data, true
		}
		fields, ok = parsePB(data)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}
