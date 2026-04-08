package daemon

import (
	"github.com/getcompanion-ai/computer-host/internal/store"
)

type Runtime interface{}

type Daemon struct {
	Store   store.Store
	Runtime Runtime
}
