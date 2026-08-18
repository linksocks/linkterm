package linkterm

import "runtime"

var (
	Version  = "v1.3.0"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
