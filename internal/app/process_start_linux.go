package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func processStartTime(ctx context.Context, pid int) (time.Time, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return time.Time{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return time.Time{}, fmt.Errorf("process unavailable")
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return time.Time{}, err
	}
	output, err := exec.CommandContext(ctx, "getconf", "CLK_TCK").Output()
	if err != nil {
		return time.Time{}, err
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || hz <= 0 {
		return time.Time{}, fmt.Errorf("clock ticks unavailable")
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return time.Time{}, err
			}
			sec, frac := math.Modf(boot + ticks/hz)
			return time.Unix(int64(sec), int64(frac*1e9)), nil
		}
	}
	return time.Time{}, fmt.Errorf("boot time unavailable")
}
