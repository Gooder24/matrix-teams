package connector

import (
	"context"
	"net/http"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

type TeamsConnector struct {
	Bridge *bridgev2.Bridge
	Config Config
	HTTP   *http.Client
}

var _ bridgev2.NetworkConnector = (*TeamsConnector)(nil)

func (tc *TeamsConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Microsoft Teams",
		NetworkURL:           "https://teams.microsoft.com",
		NetworkID:            "teams",
		BeeperBridgeType:     "github.com/Gooder24/matrix-teams",
		DefaultPort:          29340,
		DefaultCommandPrefix: "!teams",
	}
}

func (tc *TeamsConnector) Init(bridge *bridgev2.Bridge) {
	tc.Bridge = bridge
	tc.HTTP = &http.Client{Timeout: 2 * time.Minute}
}

func (tc *TeamsConnector) Start(ctx context.Context) error {
	if tc.Config.PollInterval <= 0 {
		tc.Config.PollInterval = 20 * time.Second
	}
	if tc.Config.MaxFileSize <= 0 {
		tc.Config.MaxFileSize = 25 << 20
	}
	return nil
}

func (tc *TeamsConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Message:   func() any { return &MessageMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (tc *TeamsConnector) GetBridgeInfoVersion() (info, caps int) {
	return 1, 1
}

func (tc *TeamsConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	login.Client = &TeamsClient{
		Main:      tc,
		UserLogin: login,
		Graph:     tc.newGraph(meta, login),
		lastRead:  make(map[string]time.Time),
	}
	return nil
}
