package conversation

func openCodeToolState(status string) string {
	switch status {
	case "pending", "running":
		return "running"
	case "completed":
		return "completed"
	case "error":
		return "failed"
	default:
		return ""
	}
}
