package keys

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

var _ KeyProvider = (*SupplierKeysFileProvider)(nil)

// SupplierKeysFile is the structure of the supplier.yaml file.
// It contains a simple list of hex-encoded private keys.
// The operator address is derived from each private key.
//
// Schema:
//
//	keys:
//	  - "0x..."  # hex-encoded secp256k1 private key (64 hex chars = 32 bytes)
//	  - "..."    # 0x prefix is optional
type SupplierKeysFile struct {
	// Keys is a list of hex-encoded secp256k1 private keys.
	// Can be prefixed with "0x" or not.
	// Must be non-empty and each key must be a valid 64-character hex string.
	Keys []string `yaml:"keys" json:"keys"`
}

// Validate validates the key file structure and returns detailed errors.
// This is called before attempting to parse individual keys.
func (f *SupplierKeysFile) Validate() error {
	if f.Keys == nil {
		return fmt.Errorf("invalid key file: 'keys' field is required")
	}
	if len(f.Keys) == 0 {
		return fmt.Errorf("invalid key file: 'keys' array is empty - at least one key is required")
	}

	var validationErrors []string
	for i, hexKey := range f.Keys {
		if err := validateHexKeyFormat(hexKey); err != nil {
			validationErrors = append(validationErrors, fmt.Sprintf("key[%d]: %s", i, err.Error()))
		}
	}

	if len(validationErrors) > 0 {
		return fmt.Errorf("key file validation failed:\n  - %s", strings.Join(validationErrors, "\n  - "))
	}

	return nil
}

// validateHexKeyFormat validates the format of a hex-encoded key WITHOUT parsing it.
// This provides fast, detailed error messages before attempting expensive crypto operations.
func validateHexKeyFormat(hexKey string) error {
	// Remove 0x prefix if present
	cleaned := strings.TrimPrefix(hexKey, "0x")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		return fmt.Errorf("empty key")
	}

	// Check length (64 hex chars = 32 bytes)
	if len(cleaned) != 64 {
		return fmt.Errorf("invalid length: expected 64 hex characters (32 bytes), got %d", len(cleaned))
	}

	// Check hex format
	for i, c := range cleaned {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		isUpperHex := c >= 'A' && c <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return fmt.Errorf("invalid hex character '%c' at position %d", c, i)
		}
	}

	return nil
}

// SupplierKeysFileProvider loads keys from a single supplier.yaml file
// containing a list of hex-encoded private keys.
// The operator address is derived from each key.
type SupplierKeysFileProvider struct {
	logger   logging.Logger
	filePath string
	watcher  *fsnotify.Watcher
	changeCh chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewSupplierKeysFileProvider creates a new provider that reads from supplier.yaml.
func NewSupplierKeysFileProvider(logger logging.Logger, filePath string) (*SupplierKeysFileProvider, error) {
	// Verify file exists
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("supplier keys file does not exist: %s", filePath)
		}
		return nil, fmt.Errorf("failed to stat supplier keys file: %w", err)
	}

	// Create fsnotify watcher for the file
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create file watcher: %w", err)
	}

	if err := watcher.Add(filePath); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("failed to watch supplier keys file: %w", err)
	}

	return &SupplierKeysFileProvider{
		logger:   logging.ForComponent(logger, logging.ComponentSupplierKeysFile),
		filePath: filePath,
		watcher:  watcher,
		changeCh: make(chan struct{}, 1),
	}, nil
}

// Name returns a human-readable name for this provider.
func (p *SupplierKeysFileProvider) Name() string {
	return "supplier_keys_file:" + p.filePath
}

// LoadKeys loads all keys from the supplier.yaml file.
// The operator address is derived from each private key.
//
// Returns an error if:
// - File cannot be read
// - File is not valid YAML
// - File fails schema validation (missing 'keys', empty array, invalid hex format)
// - No valid keys could be loaded
func (p *SupplierKeysFileProvider) LoadKeys(ctx context.Context) (map[string]cryptotypes.PrivKey, error) {
	keys := make(map[string]cryptotypes.PrivKey)

	data, err := os.ReadFile(p.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read supplier keys file: %w", err)
	}

	var keysFile SupplierKeysFile
	if err := yaml.Unmarshal(data, &keysFile); err != nil {
		return nil, fmt.Errorf("failed to parse supplier keys file as YAML: %w", err)
	}

	// Validate file structure before attempting to parse keys
	if err := keysFile.Validate(); err != nil {
		return nil, fmt.Errorf("supplier keys file %s: %w", p.filePath, err)
	}

	// Parse each key - at this point format validation has passed
	var parseErrors []string
	for i, hexKey := range keysFile.Keys {
		privKey, operatorAddr, err := parseHexKeyWithAddress(hexKey)
		if err != nil {
			// This shouldn't happen after format validation, but handle it
			parseErrors = append(parseErrors, fmt.Sprintf("key[%d]: %s", i, err.Error()))
			p.logger.Warn().
				Err(err).
				Int("index", i).
				Msg("failed to parse key from supplier.yaml")
			keyLoadErrors.WithLabelValues("supplier_keys_file").Inc()
			continue
		}

		keys[operatorAddr] = privKey
		p.logger.Debug().
			Int("index", i).
			Str("operator", operatorAddr).
			Msg("loaded key from supplier.yaml")
	}

	// If ALL keys failed to parse, return error (should be rare after validation)
	if len(keys) == 0 {
		return nil, fmt.Errorf("supplier keys file %s: no valid keys loaded - parsing errors:\n  - %s",
			p.filePath, strings.Join(parseErrors, "\n  - "))
	}

	p.logger.Info().
		Int("total_in_file", len(keysFile.Keys)).
		Int("loaded", len(keys)).
		Str("file", p.filePath).
		Msg("loaded keys from supplier.yaml")

	return keys, nil
}

// parseHexKeyWithAddress parses a hex-encoded private key and derives the operator address.
func parseHexKeyWithAddress(hexKey string) (cryptotypes.PrivKey, string, error) {
	// Remove 0x prefix if present
	hexKey = strings.TrimPrefix(hexKey, "0x")
	hexKey = strings.TrimSpace(hexKey)

	keyBytes, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, "", fmt.Errorf("invalid hex private key: %w", err)
	}

	if len(keyBytes) != 32 {
		return nil, "", fmt.Errorf("invalid private key length: expected 32 bytes, got %d", len(keyBytes))
	}

	privKey := &secp256k1.PrivKey{Key: keyBytes}

	// Derive the operator address from the public key using "pokt" bech32 prefix
	pubKey := privKey.PubKey()
	addr := cosmostypes.AccAddress(pubKey.Address())

	// Convert to "pokt" prefix (Pocket Network uses "pokt" instead of "cosmos")
	operatorAddr, err := cosmostypes.Bech32ifyAddressBytes("pokt", addr)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode address with pokt prefix: %w", err)
	}

	return privKey, operatorAddr, nil
}

// SupportsHotReload returns true if this provider supports hot-reload.
func (p *SupplierKeysFileProvider) SupportsHotReload() bool {
	return true
}

// WatchForChanges returns a channel that signals when keys may have changed.
func (p *SupplierKeysFileProvider) WatchForChanges(ctx context.Context) <-chan struct{} {
	// Start watching goroutine
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-p.watcher.Events:
				if !ok {
					return
				}
				// Trigger on Write or Create (file replacement)
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					// Non-blocking send
					select {
					case p.changeCh <- struct{}{}:
					default:
					}
				}
			case err, ok := <-p.watcher.Errors:
				if !ok {
					return
				}
				p.logger.Warn().Err(err).Msg("file watcher error")
			}
		}
	}()

	return p.changeCh
}

// Close gracefully shuts down the provider.
func (p *SupplierKeysFileProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true

	if p.watcher != nil {
		_ = p.watcher.Close()
	}

	close(p.changeCh)

	return nil
}
