package wsterm

import "runtime"

var (
	Version = "v1.0.2"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
