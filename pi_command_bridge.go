package herdrmobilerelay

import (
	"embed"
	"io/fs"
)

//go:embed relay/pi-command-bridge/index.ts relay/pi-command-bridge/bridge.mjs relay/pi-command-bridge/activity.mjs
var piCommandBridge embed.FS

func PiCommandBridge() fs.FS {
	bridge, err := fs.Sub(piCommandBridge, "relay/pi-command-bridge")
	if err != nil {
		panic(err)
	}
	return bridge
}
