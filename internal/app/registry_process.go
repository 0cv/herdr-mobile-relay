package app

import "context"

func registryProcessStart(ctx context.Context, pid int) (float64, error) {
	start, err := processStartTime(ctx, pid)
	if err != nil {
		return 0, err
	}
	return float64(start.Unix()) + float64(start.Nanosecond())/1e9, nil
}
