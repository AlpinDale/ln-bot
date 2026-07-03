// Package all is the plugin manifest: it blank-imports every source
// plugin package so their init() registrations run. main imports this
// package once; adding a new source means adding one line here.
package all

import (
	_ "github.com/alpindale/ln-bot/internal/source/crossinfinite"
	_ "github.com/alpindale/ln-bot/internal/source/jnovelclub"
	_ "github.com/alpindale/ln-bot/internal/source/kodansha"
	_ "github.com/alpindale/ln-bot/internal/source/yenpress"
)
