//go:build test

package redis

// IngestionPauseForTest returns what SetIngestionPause set, for tests of other
// packages' wiring.
func (c *StreamsConsumer) IngestionPauseForTest() IngestionPause {
	return c.pause
}
