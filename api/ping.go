package api

import (
	"context"
	"time"

	peer "github.com/libp2p/go-libp2p-peer"
)

// PingResult is the data that gets emitted on the Ping channel.
type PingResult struct {
	Time    time.Duration
	Text    string
	Success bool
}

// Ping is the interface that defines methods to send echo request packets over the network.
type Ping interface {
	Ping(ctx context.Context, pid peer.ID, count uint, delay time.Duration) (<-chan *PingResult, error)
}
