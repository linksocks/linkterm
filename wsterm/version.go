package wsterm

import "runtime"

var (
	Version = "v1.0.3"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
