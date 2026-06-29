package main

import "os"

func warpEnabled() bool {
	return os.Getenv("WARP") == "1"
}
