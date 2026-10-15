package p1fixture

import (
	"os"
	"strconv"
	"strings"
)

func childBirth(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", ErrContract
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[19] == "" {
		return "", ErrContract
	}
	return fields[19], nil
}
