package connector

import (
	_ "embed"
	"time"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	GraphURL        string        `yaml:"graph_url"`
	LoginURL        string        `yaml:"login_url"`
	ClientID        string        `yaml:"client_id"`
	Tenant          string        `yaml:"tenant"`
	PollInterval    time.Duration `yaml:"poll_interval"`
	InitialMessages int           `yaml:"initial_messages"`
	InitialDays     int           `yaml:"initial_days"`
	MaxFileSize     int64         `yaml:"max_file_size"`
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "graph_url")
	helper.Copy(up.Str, "login_url")
	helper.Copy(up.Str, "client_id")
	helper.Copy(up.Str, "tenant")
	helper.Copy(up.Str|up.Int, "poll_interval")
	helper.Copy(up.Int, "initial_messages")
	helper.Copy(up.Int, "initial_days")
	helper.Copy(up.Int, "max_file_size")
}

func (tc *TeamsConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &tc.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Base:           ExampleConfig,
	}
}
