package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

func (tc *TeamsConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		Provisioning: bridgev2.ProvisioningCapabilities{
			ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{
				CreateDM:    true,
				LookupEmail: true,
				Search:      true,
			},
		},
	}
}

// Teams takes about 28 000 characters of message HTML.
const maxTextLength = 28000

func fileCaps(maxSize int64) *event.FileFeatures {
	return &event.FileFeatures{
		MimeTypes:        map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
		Caption:          event.CapLevelFullySupported,
		MaxCaptionLength: maxTextLength,
		MaxSize:          maxSize,
	}
}

func (tc *TeamsClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	size := tc.Main.Config.MaxFileSize
	return &event.RoomFeatures{
		ID: "io.github.gooder24.teams.capabilities.2026_11_25",
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:          event.CapLevelFullySupported,
			event.FmtItalic:        event.CapLevelFullySupported,
			event.FmtUnderline:     event.CapLevelFullySupported,
			event.FmtStrikethrough: event.CapLevelFullySupported,
			event.FmtInlineCode:    event.CapLevelFullySupported,
			event.FmtCodeBlock:     event.CapLevelFullySupported,
			event.FmtBlockquote:    event.CapLevelFullySupported,
			event.FmtInlineLink:    event.CapLevelFullySupported,
			event.FmtUserLink:      event.CapLevelFullySupported,
			event.FmtUnorderedList: event.CapLevelFullySupported,
			event.FmtOrderedList:   event.CapLevelFullySupported,
			event.FmtHeaders:       event.CapLevelFullySupported,
		},
		File: event.FileFeatureMap{
			event.MsgImage: fileCaps(size),
			event.MsgVideo: fileCaps(size),
			event.MsgAudio: fileCaps(size),
			event.MsgFile:  fileCaps(size),
		},
		MaxTextLength:       maxTextLength,
		Reply:               event.CapLevelFullySupported,
		Edit:                event.CapLevelFullySupported,
		Delete:              event.CapLevelFullySupported,
		Reaction:            event.CapLevelFullySupported,
		ReadReceipts:        true,
		TypingNotifications: false,
	}
}
