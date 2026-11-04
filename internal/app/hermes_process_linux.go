package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func hermesProcessStart(ctx context.Context, pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return 0, fmt.Errorf("process unavailable")
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, err
	}
	output, err := exec.CommandContext(ctx, "getconf", "CLK_TCK").Output()
	if err != nil {
		return 0, err
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || hz <= 0 {
		return 0, fmt.Errorf("clock ticks unavailable")
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return boot + ticks/hz, err
		}
	}
	return 0, fmt.Errorf("boot time unavailable")
}
