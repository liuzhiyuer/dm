package config

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/ioutil"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/siddontang/go-mysql/mysql"
	"gopkg.in/yaml.v2"

	"github.com/pingcap/dm/pkg/binlog"
	"github.com/pingcap/dm/pkg/gtid"
	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/dm/pkg/terror"
	"github.com/pingcap/dm/pkg/utils"
)

const (
	// dbReadTimeout is readTimeout for DB connection in adjust
	dbReadTimeout = "30s"
	// dbGetTimeout is timeout for getting some information from DB
	dbGetTimeout = 30 * time.Second

	// the default base(min) server id generated by random
	defaultBaseServerID = math.MaxUint32 / 10
)

var getAllServerIDFunc = utils.GetAllServerID

// SampleConfigFile is sample config file of source
// later we can read it from dm/master/source.yaml
// and assign it to SampleConfigFile while we build dm-ctl
var SampleConfigFile string

// PurgeConfig is the configuration for Purger
type PurgeConfig struct {
	Interval    int64 `yaml:"interval" toml:"interval" json:"interval"`             // check whether need to purge at this @Interval (seconds)
	Expires     int64 `yaml:"expires" toml:"expires" json:"expires"`                // if file's modified time is older than @Expires (hours), then it can be purged
	RemainSpace int64 `yaml:"remain-space" toml:"remain-space" json:"remain-space"` // if remain space in @RelayBaseDir less than @RemainSpace (GB), then it can be purged
}

// SourceConfig is the configuration for Worker
type SourceConfig struct {
	EnableGTID  bool   `yaml:"enable-gtid" toml:"enable-gtid" json:"enable-gtid"`
	AutoFixGTID bool   `yaml:"auto-fix-gtid" toml:"auto-fix-gtid" json:"auto-fix-gtid"`
	RelayDir    string `yaml:"relay-dir" toml:"relay-dir" json:"relay-dir"`
	MetaDir     string `yaml:"meta-dir" toml:"meta-dir" json:"meta-dir"`
	Flavor      string `yaml:"flavor" toml:"flavor" json:"flavor"`
	Charset     string `yaml:"charset" toml:"charset" json:"charset"`

	EnableRelay bool `yaml:"enable-relay" toml:"enable-relay" json:"enable-relay"`
	// relay synchronous starting point (if specified)
	RelayBinLogName string `yaml:"relay-binlog-name" toml:"relay-binlog-name" json:"relay-binlog-name"`
	RelayBinlogGTID string `yaml:"relay-binlog-gtid" toml:"relay-binlog-gtid" json:"relay-binlog-gtid"`

	SourceID string   `yaml:"source-id" toml:"source-id" json:"source-id"`
	From     DBConfig `yaml:"from" toml:"from" json:"from"`

	// config items for purger
	Purge PurgeConfig `yaml:"purge" toml:"purge" json:"purge"`

	// config items for task status checker
	Checker CheckerConfig `yaml:"checker" toml:"checker" json:"checker"`

	// id of the worker on which this task run
	ServerID uint32 `yaml:"server-id" toml:"server-id" json:"server-id"`

	// deprecated tracer, to keep compatibility with older version
	Tracer map[string]interface{} `yaml:"tracer" toml:"tracer" json:"-"`
}

// NewSourceConfig creates a new base config for upstream MySQL/MariaDB source.
func NewSourceConfig() *SourceConfig {
	c := &SourceConfig{
		RelayDir: "relay-dir",
		Purge: PurgeConfig{
			Interval:    60 * 60,
			Expires:     0,
			RemainSpace: 15,
		},
		Checker: CheckerConfig{
			CheckEnable:     true,
			BackoffRollback: Duration{DefaultBackoffRollback},
			BackoffMax:      Duration{DefaultBackoffMax},
		},
	}
	c.adjust()
	return c
}

// Clone clones a config
func (c *SourceConfig) Clone() *SourceConfig {
	clone := &SourceConfig{}
	*clone = *c
	return clone
}

// Toml returns TOML format representation of config
func (c *SourceConfig) Toml() (string, error) {
	var b bytes.Buffer

	err := toml.NewEncoder(&b).Encode(c)
	if err != nil {
		log.L().Error("fail to marshal config to toml", log.ShortError(err))
	}

	return b.String(), nil
}

// Yaml returns YAML format representation of config
func (c *SourceConfig) Yaml() (string, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		log.L().Error("fail to marshal config to yaml", log.ShortError(err))
	}

	return string(b), nil
}

// Parse parses flag definitions from the argument list.
// accept toml content for legacy use (mainly used by etcd)
func (c *SourceConfig) Parse(content string) error {
	// Parse first to get config file.
	metaData, err := toml.Decode(content, c)
	return c.check(&metaData, err)
}

// ParseYaml parses flag definitions from the argument list, content should be yaml format
func (c *SourceConfig) ParseYaml(content string) error {
	if err := yaml.UnmarshalStrict([]byte(content), c); err != nil {
		return terror.ErrConfigYamlTransform.Delegate(err, "decode source config")
	}
	c.adjust()
	return nil
}

// EncodeToml encodes config.
func (c *SourceConfig) EncodeToml() (string, error) {
	buf := new(bytes.Buffer)
	if err := toml.NewEncoder(buf).Encode(c); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *SourceConfig) String() string {
	cfg, err := json.Marshal(c)
	if err != nil {
		log.L().Error("fail to marshal config to json", log.ShortError(err))
	}
	return string(cfg)
}

func (c *SourceConfig) adjust() {
	c.From.Adjust()
	c.Checker.Adjust()
}

// Verify verifies the config
func (c *SourceConfig) Verify() error {
	if len(c.SourceID) == 0 {
		return terror.ErrWorkerNeedSourceID.Generate()
	}
	if len(c.SourceID) > MaxSourceIDLength {
		return terror.ErrWorkerTooLongSourceID.Generate(c.SourceID, MaxSourceIDLength)
	}

	var err error
	if c.EnableRelay {
		if len(c.RelayBinLogName) > 0 {
			if !binlog.VerifyFilename(c.RelayBinLogName) {
				return terror.ErrWorkerRelayBinlogName.Generate(c.RelayBinLogName)
			}
		}
		if len(c.RelayBinlogGTID) > 0 {
			_, err = gtid.ParserGTID(c.Flavor, c.RelayBinlogGTID)
			if err != nil {
				return terror.WithClass(terror.Annotatef(err, "relay-binlog-gtid %s", c.RelayBinlogGTID), terror.ClassDMWorker)
			}
		}
	}

	c.DecryptPassword()

	return nil
}

// DecryptPassword returns a decrypted config replica in config
func (c *SourceConfig) DecryptPassword() *SourceConfig {
	clone := c.Clone()
	var (
		pswdFrom string
	)
	if len(clone.From.Password) > 0 {
		pswdFrom = utils.DecryptOrPlaintext(clone.From.Password)
	}
	clone.From.Password = pswdFrom
	return clone
}

// GenerateDBConfig creates DBConfig for DB
func (c *SourceConfig) GenerateDBConfig() *DBConfig {
	// decrypt password
	clone := c.DecryptPassword()
	from := &clone.From
	from.RawDBCfg = DefaultRawDBConfig().SetReadTimeout(dbReadTimeout)
	return from
}

// Adjust flavor and serverid of SourceConfig
func (c *SourceConfig) Adjust(db *sql.DB) (err error) {
	c.From.Adjust()
	c.Checker.Adjust()

	if c.Flavor == "" || c.ServerID == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), dbGetTimeout)
		defer cancel()

		err = c.AdjustFlavor(ctx, db)
		if err != nil {
			return err
		}

		err = c.AdjustServerID(ctx, db)
		if err != nil {
			return err
		}
	}

	if c.EnableGTID {
		val, err := utils.GetGTID(db)
		if err != nil {
			return err
		}
		if val != "ON" {
			return terror.ErrSourceCheckGTID.Generate(c.SourceID, val)
		}
	}

	return nil
}

// AdjustFlavor adjust Flavor from DB
func (c *SourceConfig) AdjustFlavor(ctx context.Context, db *sql.DB) (err error) {
	if c.Flavor != "" {
		switch c.Flavor {
		case mysql.MariaDBFlavor, mysql.MySQLFlavor:
			return nil
		default:
			return terror.ErrNotSupportedFlavor.Generate(c.Flavor)
		}
	}

	c.Flavor, err = utils.GetFlavor(ctx, db)
	if ctx.Err() != nil {
		err = terror.Annotatef(err, "time cost to get flavor info exceeds %s", dbGetTimeout)
	}
	return terror.WithScope(err, terror.ScopeUpstream)
}

// AdjustServerID adjust server id from DB
func (c *SourceConfig) AdjustServerID(ctx context.Context, db *sql.DB) error {
	if c.ServerID != 0 {
		return nil
	}

	serverIDs, err := getAllServerIDFunc(ctx, db)
	if ctx.Err() != nil {
		err = terror.Annotatef(err, "time cost to get server-id info exceeds %s", dbGetTimeout)
	}
	if err != nil {
		return terror.WithScope(err, terror.ScopeUpstream)
	}

	for i := 0; i < 5; i++ {
		randomValue := uint32(rand.Intn(100000))
		randomServerID := defaultBaseServerID + randomValue
		if _, ok := serverIDs[randomServerID]; ok {
			continue
		}

		c.ServerID = randomServerID
		return nil
	}

	return terror.ErrInvalidServerID.Generatef("can't find a random available server ID")
}

// LoadFromFile loads config from file.
func (c *SourceConfig) LoadFromFile(path string) error {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return terror.ErrConfigReadCfgFromFile.Delegate(err, path)
	}
	if err := yaml.UnmarshalStrict(content, c); err != nil {
		return terror.ErrConfigYamlTransform.Delegate(err, "decode source config")
	}
	c.adjust()
	return nil
}

func (c *SourceConfig) check(metaData *toml.MetaData, err error) error {
	if err != nil {
		return terror.ErrWorkerDecodeConfigFromFile.Delegate(err)
	}
	undecoded := metaData.Undecoded()
	if len(undecoded) > 0 && err == nil {
		var undecodedItems []string
		for _, item := range undecoded {
			undecodedItems = append(undecodedItems, item.String())
		}
		return terror.ErrWorkerUndecodedItemFromFile.Generate(strings.Join(undecodedItems, ","))
	}
	c.adjust()
	return nil
}
