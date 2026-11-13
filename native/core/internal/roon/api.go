package roon

// The extension surface this app uses: registry info, registration, zone
// subscription and transport control. Everything else in Roon's API is out of
// scope on purpose.

import (
	"encoding/json"
	"fmt"
	"time"
)

// Info is what the core reports about itself.
type Info struct {
	CoreID           string   `json:"core_id"`
	DisplayName      string   `json:"display_name"`
	DisplayVersion   string   `json:"display_version"`
	ServiceID        string   `json:"service_id"`
	ProvidedServices []string `json:"provided_services"`
}

// RegisterOptions describes the extension.
type RegisterOptions struct {
	ExtensionID      string   `json:"extension_id"`
	DisplayName      string   `json:"display_name"`
	DisplayVersion   string   `json:"display_version"`
	Publisher        string   `json:"publisher"`
	Email            string   `json:"email"`
	RequiredServices []string `json:"required_services"`
	OptionalServices []string `json:"optional_services"`
	ProvidedServices []string `json:"provided_services"`
	Website          string   `json:"website,omitempty"`
	Token            string   `json:"token,omitempty"`
}

// Info reads com.roonlabs.registry:1/info.
func (c *Client) Info() (Info, error) {
	message, err := c.Request("com.roonlabs.registry:1/info", "", 5*time.Second)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal([]byte(message.Body), &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// Register asks the core to register this extension. The first time, Roon
// waits for a person to enable it in Settings → Extensions, so the timeout is
// generous and ErrPendingApproval is a normal state, not a failure.
func (c *Client) Register(options RegisterOptions, wait time.Duration) (string, error) {
	body, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	// Registration answers CONTINUE Registered, not COMPLETE.
	message, err := c.RequestFirst("com.roonlabs.registry:1/register", string(body), wait)
	if err != nil {
		return "", err
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(message.Body), &result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("roon: 注册成功但没有返回 token")
	}
	return result.Token, nil
}

// Zone is the part of a zone snapshot this app shows and controls.
type Zone struct {
	ZoneID      string `json:"zone_id"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
	NowPlaying  struct {
		OneLine struct {
			Line1 string `json:"line1"`
		} `json:"one_line"`
		TwoLine struct {
			Line1 string `json:"line1"`
			Line2 string `json:"line2"`
		} `json:"two_line"`
		Length        int `json:"length"`
		SeekPosition  int `json:"seek_position"`
	} `json:"now_playing"`
	Outputs []struct {
		OutputID    string `json:"output_id"`
		DisplayName string `json:"display_name"`
		Volume      *struct {
			Value int    `json:"value"`
			Max   int    `json:"max"`
			Type  string `json:"type"`
		} `json:"volume"`
	} `json:"outputs"`
}

// ZonesPayload is one zone subscription message.
type ZonesPayload struct {
	Zones         []Zone `json:"zones"`
	ZonesChanged  []Zone `json:"zones_changed"`
	ZonesRemoved  []struct {
		ZoneID string `json:"zone_id"`
	} `json:"zones_removed"`
	ZonesSeekChanged []struct {
		ZoneID       string `json:"zone_id"`
		SeekPosition int    `json:"seek_position"`
	} `json:"zones_seek_changed"`
}

// SubscribeZones starts the transport subscription. The first payload carries
// every zone; later pushes use zones_changed / zones_removed.
func (c *Client) SubscribeZones() (ZonesPayload, error) {
	// A body made Roon answer InvalidRequest; the subscription works with no
	// parameters at all, so try that first and keep the key only as a
	// fallback for cores that demand one.
	message, err := c.Request("com.roonlabs.transport:1/subscribe_zones", "", 10*time.Second)
	if err != nil {
		message, err = c.Request("com.roonlabs.transport:1/subscribe_zones", `{"subscription_key":"musicops"}`, 10*time.Second)
	}
	if err != nil {
		return ZonesPayload{}, err
	}
	var payload ZonesPayload
	if err := json.Unmarshal([]byte(message.Body), &payload); err != nil {
		return ZonesPayload{}, err
	}
	return payload, nil
}

// ZoneUpdate decodes a pushed subscription message, if it is one.
func ZoneUpdate(message Message) (ZonesPayload, bool) {
	if message.Kind != "COMPLETE" && message.Kind != "CONTINUE" {
		return ZonesPayload{}, false
	}
	if message.Headers["Content-Type"] != "application/json" {
		return ZonesPayload{}, false
	}
	var payload ZonesPayload
	if err := json.Unmarshal([]byte(message.Body), &payload); err != nil {
		return ZonesPayload{}, false
	}
	// A control response echoes what changed; it is not a snapshot. Only
	// treat a message as a zone update when it actually carries zones.
	if len(payload.Zones) == 0 && len(payload.ZonesChanged) == 0 && len(payload.ZonesRemoved) == 0 && len(payload.ZonesSeekChanged) == 0 {
		return ZonesPayload{}, false
	}
	return payload, true
}

// Control sends one transport command to a zone. control is one of play,
// pause, playpause, stop, next, previous, or volume (with value 0..100).
func (c *Client) Control(zoneID, control string, value int) error {
	body := map[string]any{"zone_id": zoneID, "control": control}
	if control == "volume" {
		body["value"] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.Request("com.roonlabs.transport:1/control", string(encoded), 8*time.Second)
	return err
}
