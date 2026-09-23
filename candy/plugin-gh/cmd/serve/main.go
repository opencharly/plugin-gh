package main

import (
	plugingh "github.com/opencharly/plugin-gh/candy/plugin-gh"
	"github.com/opencharly/sdk"
)

func main() {
	sdk.Main(plugingh.NewProvider(), plugingh.NewMeta(), plugingh.CliMain)
}
