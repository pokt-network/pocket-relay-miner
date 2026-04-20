//go:build test

package miner

import (
	"encoding/hex"
	"fmt"
	"sync"
)

// TestRedisMapStore_GetEmpty tests that a missing key surfaces as
// ErrSMSTNodeMissing. The smt library only calls Get after checking the
// placeholder digest, so a miss at this level is always corruption —
// returning an error prevents parseSumTrieNode from panicking on an
// empty payload.
func (s *RedisSMSTTestSuite) TestRedisMapStore_GetEmpty() {
	store := s.createTestRedisStore("test-session-get-empty")

	value, err := store.Get([]byte("non-existent-key"))
	s.Require().Error(err, "Get on a missing key must error, not return (nil, nil)")
	s.Require().ErrorIs(err, ErrSMSTNodeMissing,
		"missing-key error must satisfy errors.Is(ErrSMSTNodeMissing)")
	s.Require().Nil(value, "Get on a missing key must return no value")
}

// TestRedisMapStore_SetGet tests basic Set and Get operations.
func (s *RedisSMSTTestSuite) TestRedisMapStore_SetGet() {
	store := s.createTestRedisStore("test-session-set-get")

	key := []byte("test-key")
	value := []byte("test-value")

	// Set value
	err := store.Set(key, value)
	s.Require().NoError(err, "Set should not error")

	// Get value
	gotValue, err := store.Get(key)
	s.Require().NoError(err, "Get should not error")
	s.Require().Equal(value, gotValue, "Get should return the same value that was set")
}

// TestRedisMapStore_SetOverwrite tests overwriting an existing key.
func (s *RedisSMSTTestSuite) TestRedisMapStore_SetOverwrite() {
	store := s.createTestRedisStore("test-session-set-overwrite")

	key := []byte("test-key")
	value1 := []byte("value1")
	value2 := []byte("value2")

	// Set initial value
	err := store.Set(key, value1)
	s.Require().NoError(err)

	// Verify initial value
	gotValue, err := store.Get(key)
	s.Require().NoError(err)
	s.Require().Equal(value1, gotValue)

	// Overwrite with new value
	err = store.Set(key, value2)
	s.Require().NoError(err)

	// Verify new value
	gotValue, err = store.Get(key)
	s.Require().NoError(err)
	s.Require().Equal(value2, gotValue, "value should be overwritten")
}

// TestRedisMapStore_Delete tests deleting an existing key.
func (s *RedisSMSTTestSuite) TestRedisMapStore_Delete() {
	store := s.createTestRedisStore("test-session-delete")

	key := []byte("test-key")
	value := []byte("test-value")

	// Set value
	err := store.Set(key, value)
	s.Require().NoError(err)

	// Verify it exists
	gotValue, err := store.Get(key)
	s.Require().NoError(err)
	s.Require().Equal(value, gotValue)

	// Delete it
	err = store.Delete(key)
	s.Require().NoError(err)

	// Verify it's gone. Get now surfaces a missing key as
	// ErrSMSTNodeMissing (see Get docstring) so the smt library cannot
	// hit the parseSumTrieNode panic on a zero-length slice. The test
	// asserts the new contract explicitly.
	gotValue, err = store.Get(key)
	s.Require().ErrorIs(err, ErrSMSTNodeMissing,
		"Get on a deleted key must return ErrSMSTNodeMissing, not (nil, nil)")
	s.Require().Nil(gotValue)
}

// TestRedisMapStore_DeleteNonExistent tests deleting a non-existent key (should be a no-op).
func (s *RedisSMSTTestSuite) TestRedisMapStore_DeleteNonExistent() {
	store := s.createTestRedisStore("test-session-delete-nonexistent")

	// Delete non-existent key (should not error)
	err := store.Delete([]byte("non-existent-key"))
	s.Require().NoError(err, "Delete should not error for non-existent key")
}

// TestRedisMapStore_Len tests length tracking across operations.
func (s *RedisSMSTTestSuite) TestRedisMapStore_Len() {
	store := s.createTestRedisStore("test-session-len")

	// Initial length should be 0
	length, err := store.Len()
	s.Require().NoError(err)
	s.Require().Equal(0, length, "new store should have length 0")

	// Add 3 keys
	err = store.Set([]byte("key1"), []byte("value1"))
	s.Require().NoError(err)
	err = store.Set([]byte("key2"), []byte("value2"))
	s.Require().NoError(err)
	err = store.Set([]byte("key3"), []byte("value3"))
	s.Require().NoError(err)

	// Length should be 3
	length, err = store.Len()
	s.Require().NoError(err)
	s.Require().Equal(3, length, "should have 3 keys")

	// Delete one key
	err = store.Delete([]byte("key2"))
	s.Require().NoError(err)

	// Length should be 2
	length, err = store.Len()
	s.Require().NoError(err)
	s.Require().Equal(2, length, "should have 2 keys after delete")

	// Overwrite existing key (length should stay the same)
	err = store.Set([]byte("key1"), []byte("new-value"))
	s.Require().NoError(err)

	length, err = store.Len()
	s.Require().NoError(err)
	s.Require().Equal(2, length, "overwriting key should not change length")
}

// TestRedisMapStore_ClearAll tests clearing the entire hash.
func (s *RedisSMSTTestSuite) TestRedisMapStore_ClearAll() {
	store := s.createTestRedisStore("test-session-clear-all")

	// Add some keys
	err := store.Set([]byte("key1"), []byte("value1"))
	s.Require().NoError(err)
	err = store.Set([]byte("key2"), []byte("value2"))
	s.Require().NoError(err)

	// Verify length
	length, err := store.Len()
	s.Require().NoError(err)
	s.Require().Equal(2, length)

	// Clear all
	err = store.ClearAll()
	s.Require().NoError(err)

	// Verify length is 0
	length, err = store.Len()
	s.Require().NoError(err)
	s.Require().Equal(0, length, "length should be 0 after ClearAll")

	// Verify keys are gone. Post-ClearAll Get must surface
	// ErrSMSTNodeMissing for the same reason Delete does — the smt
	// library never asks for a placeholder, so any miss is corruption.
	value, err := store.Get([]byte("key1"))
	s.Require().ErrorIs(err, ErrSMSTNodeMissing)
	s.Require().Nil(value)

	value, err = store.Get([]byte("key2"))
	s.Require().ErrorIs(err, ErrSMSTNodeMissing)
	s.Require().Nil(value)
}

// TestRedisMapStore_HexEncoding verifies hex encoding of keys.
// Redis field names must be strings, so byte keys are hex-encoded.
func (s *RedisSMSTTestSuite) TestRedisMapStore_HexEncoding() {
	store := s.createTestRedisStore("test-session-hex-encoding")

	// Use binary key (not valid UTF-8)
	binaryKey := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
	value := []byte("test-value")

	// Set value with binary key
	err := store.Set(binaryKey, value)
	s.Require().NoError(err)

	// Get value back
	gotValue, err := store.Get(binaryKey)
	s.Require().NoError(err)
	s.Require().Equal(value, gotValue)

	// Verify the key is actually hex-encoded in Redis
	hexKey := hex.EncodeToString(binaryKey)
	hashKey := s.redisClient.KB().SMSTNodesKey(testMapStoreSupplier, "test-session-hex-encoding")

	// Get directly from Redis to verify hex encoding
	redisValue, err := s.redisClient.HGet(s.ctx, hashKey, hexKey).Bytes()
	s.Require().NoError(err)
	s.Require().Equal(value, redisValue, "value should be stored under hex-encoded key")
}

// TestRedisMapStore_Pipeline_Buffer tests that BeginPipeline buffers Set operations.
func (s *RedisSMSTTestSuite) TestRedisMapStore_Pipeline_Buffer() {
	store := s.createTestRedisStore("test-session-pipeline-buffer")

	// Enable pipeline mode
	store.BeginPipeline()

	// Set multiple values (should be buffered, not executed)
	err := store.Set([]byte("key1"), []byte("value1"))
	s.Require().NoError(err, "Set should not error in pipeline mode")

	err = store.Set([]byte("key2"), []byte("value2"))
	s.Require().NoError(err)

	err = store.Set([]byte("key3"), []byte("value3"))
	s.Require().NoError(err)

	// Values should NOT be in Redis yet (buffered)
	// Check directly in Redis
	hashKey := s.redisClient.KB().SMSTNodesKey(testMapStoreSupplier, "test-session-pipeline-buffer")
	exists, err := s.redisClient.HExists(s.ctx, hashKey, hex.EncodeToString([]byte("key1"))).Result()
	s.Require().NoError(err)
	s.Require().False(exists, "values should be buffered, not written to Redis yet")

	// Flush pipeline to execute buffered operations
	err = store.FlushPipeline()
	s.Require().NoError(err)

	// Now values should exist in Redis
	gotValue, err := store.Get([]byte("key1"))
	s.Require().NoError(err)
	s.Require().Equal([]byte("value1"), gotValue, "value should exist after flush")

	gotValue, err = store.Get([]byte("key2"))
	s.Require().NoError(err)
	s.Require().Equal([]byte("value2"), gotValue)

	gotValue, err = store.Get([]byte("key3"))
	s.Require().NoError(err)
	s.Require().Equal([]byte("value3"), gotValue)
}

// TestRedisMapStore_Pipeline_Flush tests flushing buffered pipeline operations.
func (s *RedisSMSTTestSuite) TestRedisMapStore_Pipeline_Flush() {
	store := s.createTestRedisStore("test-session-pipeline-flush")

	// Enable pipeline
	store.BeginPipeline()

	// Buffer 20 Set operations (simulating SMST Commit with ~20 dirty nodes)
	for i := 0; i < 20; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		value := []byte(fmt.Sprintf("value_%d", i))
		err := store.Set(key, value)
		s.Require().NoError(err)
	}

	// Flush pipeline (executes single batched HSET)
	err := store.FlushPipeline()
	s.Require().NoError(err)

	// Verify all values were written
	for i := 0; i < 20; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		expectedValue := []byte(fmt.Sprintf("value_%d", i))

		gotValue, err := store.Get(key)
		s.Require().NoError(err)
		s.Require().Equal(expectedValue, gotValue, "all buffered values should be written after flush")
	}

	// Verify length is correct
	length, err := store.Len()
	s.Require().NoError(err)
	s.Require().Equal(20, length, "should have 20 keys after flush")

	// Test that subsequent Set operations execute immediately (pipeline mode disabled after flush)
	err = store.Set([]byte("key_immediate"), []byte("value_immediate"))
	s.Require().NoError(err)

	gotValue, err := store.Get([]byte("key_immediate"))
	s.Require().NoError(err)
	s.Require().Equal([]byte("value_immediate"), gotValue, "Set should execute immediately after flush")
}

// TestRedisMapStore_Pipeline_Error tests error handling in pipeline mode.
// This test verifies that pipeline errors propagate correctly.
func (s *RedisSMSTTestSuite) TestRedisMapStore_Pipeline_Error() {
	store := s.createTestRedisStore("test-session-pipeline-error")

	// Enable pipeline
	store.BeginPipeline()

	// Buffer some operations
	err := store.Set([]byte("key1"), []byte("value1"))
	s.Require().NoError(err)

	// Flush should succeed with normal operations
	err = store.FlushPipeline()
	s.Require().NoError(err)

	// Test empty flush (no buffered operations)
	store.BeginPipeline()
	err = store.FlushPipeline()
	s.Require().NoError(err, "flushing empty pipeline should not error")
}

// TestRedisMapStore_Concurrency tests concurrent Get/Set operations.
// This verifies thread safety (Rule #1: must pass -race flag).
func (s *RedisSMSTTestSuite) TestRedisMapStore_Concurrency() {
	store := s.createTestRedisStore("test-session-concurrency")

	// Number of concurrent goroutines
	numGoroutines := 10
	numOpsPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Launch concurrent writers
	for g := 0; g < numGoroutines; g++ {
		goroutineID := g
		go func() {
			defer wg.Done()

			for i := 0; i < numOpsPerGoroutine; i++ {
				key := []byte(fmt.Sprintf("key_g%d_i%d", goroutineID, i))
				value := []byte(fmt.Sprintf("value_g%d_i%d", goroutineID, i))

				// Set
				err := store.Set(key, value)
				s.Require().NoError(err)

				// Get (verify write)
				gotValue, err := store.Get(key)
				s.Require().NoError(err)
				s.Require().Equal(value, gotValue)
			}
		}()
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify total number of keys
	expectedKeys := numGoroutines * numOpsPerGoroutine
	length, err := store.Len()
	s.Require().NoError(err)
	s.Require().Equal(expectedKeys, length, "should have all keys written concurrently")
}
