package linkterm

import "runtime"

var (
	Version  = "v1.3.1"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
