package app

import (
	"context"
	"fmt"
)

func kimiProcessStart(pid int) (string, error) {
	start, err := processStartTime(context.Background(), pid)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%06d", start.Unix(), start.Nanosecond()/1000), nil
}
