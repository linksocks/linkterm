package wsterm

import "runtime"

var (
	Version = "v1.0.4"
	Platform = runtime.GOOS + "/" + runtime.GOARCH
)
