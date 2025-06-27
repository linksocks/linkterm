package wsterm

import "runtime"

var (
	Version = "v1.0.5"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
