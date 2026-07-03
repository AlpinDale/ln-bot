// Package all is the plugin manifest: it blank-imports every source
// plugin package so their init() registrations run. main imports this
// package once; adding a new source means adding one line here.
package all

import (
	_ "github.com/alpindale/ln-bot/internal/source/jnovelclub"
)
