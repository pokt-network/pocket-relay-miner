// Package standalone holds what the standalone subcommand adds on top of the
// relayer and the miner it runs in one process: one config file for both.
package standalone

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// Sections of a standalone file. The common ones are written once, at the top
// level, and given to both sides; relayer: and miner: hold what only that side
// reads, with the same keys as the side's own config file.
const (
	sectionRelayer = "relayer"
	sectionMiner   = "miner"
	sectionRedis   = "redis"
	sectionStorage = "storage"
)

// StorageConfig is the embedded store the process keeps its state in.
type StorageConfig struct {
	// Path is the directory of the store. Required.
	Path string `yaml:"path"`
	// SyncInterval bounds what an OS crash can lose (pebblestore). Zero means
	// the store's default.
	SyncInterval time.Duration `yaml:"sync_interval,omitempty"`
}

// commonSections are given whole to both sides.
var commonSections = []string{"pocket_node", "keys", "logging", "metrics", "pprof"}

// redisConnectionKeys are the redis leaves that name a server and a keyspace;
// a side's other redis keys (the relayer's publish interval) still apply.
var redisConnectionKeys = []string{"url", "namespace"}

// Config is a parsed standalone file: each side's config, built by that side's
// own parser, so defaults and validation are the ones the relayer and miner
// subcommands apply.
type Config struct {
	Relayer *relayer.Config
	Miner   *miner.Config

	// Storage is the embedded store.
	Storage StorageConfig

	// Metrics and PProf configure the process's one observability server.
	Metrics config.MetricsConfig
	PProf   config.PprofConfig
	Logging logging.Config

	unknownKeys []string
}

// Warnings returns one line per key the file carries that the standalone
// config does not declare, with the line it is on in this file.
func (c *Config) Warnings() []string { return c.unknownKeys }

// probe is the whole file's shape, for the unknown-key pass over the original
// bytes: the sides' own passes run on documents this package assembles, whose
// line numbers are not the operator's.
type probe struct {
	PocketNode config.PocketNodeConfig `yaml:"pocket_node"`
	Keys       config.KeysConfig       `yaml:"keys"`
	Logging    logging.Config          `yaml:"logging"`
	Metrics    config.MetricsConfig    `yaml:"metrics"`
	PProf      config.PprofConfig      `yaml:"pprof"`
	Storage    StorageConfig           `yaml:"storage"`
	Relayer    relayer.Config          `yaml:"relayer"`
	Miner      miner.Config            `yaml:"miner"`
}

// LoadConfig reads and parses a standalone config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig splits a standalone file into a relayer document and a miner
// document -- each side's section plus the common sections -- and parses each
// with that side's own parser.
func ParseConfig(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file must be a mapping of sections")
	}
	top := doc.Content[0]

	var storage StorageConfig
	sides := map[string]*yaml.Node{}
	common := map[string]*yaml.Node{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, value := top.Content[i], top.Content[i+1]
		switch {
		case key.Value == sectionRelayer || key.Value == sectionMiner:
			if value.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: %s must be a mapping", key.Line, key.Value)
			}
			sides[key.Value] = value
		case key.Value == sectionRedis:
			return nil, fmt.Errorf("line %d: a standalone config has no redis section: standalone keeps its "+
				"state in storage.path and connects to no Redis; to run on Redis, run the relayer and miner "+
				"subcommands (the high-availability mode)", key.Line)
		case slices.Contains(commonSections, key.Value):
			common[key.Value] = value
		case key.Value == sectionStorage:
			if err := value.Decode(&storage); err != nil {
				return nil, fmt.Errorf("line %d: storage: %w", key.Line, err)
			}
		default:
			return nil, fmt.Errorf("line %d: %q is not a section of a standalone config: "+
				"put a key only one side reads under that side's section (relayer or miner)",
				key.Line, key.Value)
		}
	}
	for _, name := range []string{sectionRelayer, sectionMiner} {
		if sides[name] == nil {
			return nil, fmt.Errorf("the %s: section is required", name)
		}
	}
	if storage.Path == "" {
		return nil, fmt.Errorf("storage.path is required: the directory the embedded store lives in")
	}

	relayerDoc, err := sideDocument(sectionRelayer, sides[sectionRelayer], common)
	if err != nil {
		return nil, err
	}
	minerDoc, err := sideDocument(sectionMiner, sides[sectionMiner], common)
	if err != nil {
		return nil, err
	}

	relayerCfg, err := relayer.ParseConfigWithoutRedis(relayerDoc)
	if err != nil {
		return nil, fmt.Errorf("relayer: %w", err)
	}
	minerCfg, err := miner.ParseConfigWithoutRedis(minerDoc)
	if err != nil {
		return nil, fmt.Errorf("miner: %w", err)
	}

	// The process's own logger and observability server take the miner's
	// view of the common sections: what the file says, and where it says
	// nothing, the miner subcommand's defaults (metrics on at :9092, async
	// logging). Both sides received the same sections, so only the defaults
	// for keys the file omits can differ between them.
	return &Config{
		Relayer:     relayerCfg,
		Miner:       minerCfg,
		Storage:     storage,
		Metrics:     minerCfg.Metrics,
		PProf:       minerCfg.PProf,
		Logging:     minerCfg.Logging,
		unknownKeys: config.UnknownKeys(data, &probe{}),
	}, nil
}

// sideDocument is the side's section with the common sections added, as the
// bytes of a config file of that side. A common section, or a common redis key,
// written inside the side is refused: one value per setting, in one place.
func sideDocument(name string, side *yaml.Node, common map[string]*yaml.Node) ([]byte, error) {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: side.Tag}
	var sideRedis *yaml.Node
	for i := 0; i+1 < len(side.Content); i += 2 {
		key, value := side.Content[i], side.Content[i+1]
		if slices.Contains(commonSections, key.Value) {
			return nil, fmt.Errorf("line %d: %s.%s is set at the top level of a standalone config, once for both sides",
				key.Line, name, key.Value)
		}
		if key.Value == sectionRedis {
			if value.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: %s.redis must be a mapping", key.Line, name)
			}
			for j := 0; j+1 < len(value.Content); j += 2 {
				if slices.Contains(redisConnectionKeys, value.Content[j].Value) {
					return nil, fmt.Errorf("line %d: %s.redis.%s: standalone connects to no Redis",
						value.Content[j].Line, name, value.Content[j].Value)
				}
			}
			sideRedis = &yaml.Node{Kind: yaml.MappingNode, Content: append([]*yaml.Node(nil), value.Content...)}
			continue
		}
		out.Content = append(out.Content, key, value)
	}

	names := make([]string, 0, len(common))
	for k := range common {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if k == sectionRedis {
			continue
		}
		out.Content = append(out.Content, scalar(k), common[k])
	}

	if sideRedis != nil {
		out.Content = append(out.Content, scalar(sectionRedis), sideRedis)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return buf.Bytes(), nil
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}
