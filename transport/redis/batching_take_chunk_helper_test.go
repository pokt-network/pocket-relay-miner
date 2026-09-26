//go:build test

package redis

import "math"

// takeChunk takes the next chunk whatever its size or age, which is what a test
// reading the queue back wants.
func (p *BatchingPublisher) takeChunk() []queued {
	return p.takeChunkBefore(math.MaxUint64)
}
