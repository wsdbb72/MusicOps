package roon

// The extension surface this app uses: registry info, registration, zone
// subscription and transport control. Everything else in Roon's API is out of
// scope on purpose.

import (
	"encoding/json"
	"fmt"
	"time"
)

// Service names. Roon 2.x provides transport:2; transport:1 is kept as a
// fallback for older cores.
const (
	TransportV2 = "com.roonlabs.transport:2"
	TransportV1 = "com.roonlabs.transport:1"
	BrowseV1    = "com.roonlabs.browse:1"
)

// Info is what the core reports about itself.
type Info struct {
	CoreID         string `json:"core_id"`
	DisplayName    string `json:"display_name"`
	DisplayVersion string `json:"display_version"`
}

// RegisterOptions describes the extension. ProvidedServices must include
// com.roonlabs.ping:1 and com.roonlabs.pairing:1: Roon pings the extension and
// expects an answer, and it pairs the connection through the pairing service.
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

// TransportService reports the transport the core granted this extension:
// transport:2 when Roon offers it, otherwise transport:1. A core only serves
// services that are in the approved registration, so this is decided by the
// registration answer, not guessed.
func (c *Client) TransportService() string {
	if value, ok := c.transport.Load().(string); ok && value != "" {
		return value
	}
	return TransportV1
}

// Registration is the successful answer to register.
type Registration struct {
	CoreID           string   `json:"core_id"`
	DisplayName      string   `json:"display_name"`
	DisplayVersion   string   `json:"display_version"`
	Token            string   `json:"token"`
	ProvidedServices []string `json:"provided_services"`
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
	c.PairedCoreID.Store(info.CoreID)
	return info, nil
}

// Register asks the core to register this extension. With a token from an
// earlier approval the core re-enables the extension without asking; without
// one the call blocks until a person enables it in Settings → Extensions, so
// the wait is long and ErrPendingApproval is a normal state.
func (c *Client) Register(options RegisterOptions, wait time.Duration) (Registration, error) {
	body, err := json.Marshal(options)
	if err != nil {
		return Registration{}, err
	}
	message, err := c.RequestFirst("com.roonlabs.registry:1/register", string(body), wait)
	if err != nil {
		return Registration{}, err
	}
	var registration Registration
	if err := json.Unmarshal([]byte(message.Body), &registration); err != nil {
		return Registration{}, err
	}
	if registration.Token == "" {
		return Registration{}, fmt.Errorf("roon: 注册成功但没有返回 token")
	}
	service := TransportV1
	for _, provided := range registration.ProvidedServices {
		if provided == TransportV2 {
			service = TransportV2
		}
	}
	c.transport.Store(service)
	return registration, nil
}

// Volume is an output's volume control. min/max are absent for incremental
// controls, so they default to 0 and callers treat that as "no absolute value".
type Volume struct {
	Type    string `json:"type"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Value   int    `json:"value"`
	Step    int    `json:"step"`
	IsMuted bool   `json:"is_muted"`
}

// Output is one output of a zone.
type Output struct {
	OutputID    string  `json:"output_id"`
	ZoneID      string  `json:"zone_id"`
	DisplayName string  `json:"display_name"`
	Volume      *Volume `json:"volume"`
}

// NowPlaying is the track a zone is playing. seek_position is null when the
// zone is stopped, so it is a pointer.
type NowPlaying struct {
	SeekPosition *int   `json:"seek_position"`
	Length       int    `json:"length"`
	ImageKey     string `json:"image_key"`
	OneLine      struct {
		Line1 string `json:"line1"`
	} `json:"one_line"`
	TwoLine struct {
		Line1 string `json:"line1"`
		Line2 string `json:"line2"`
	} `json:"two_line"`
	ThreeLine struct {
		Line1 string `json:"line1"`
		Line2 string `json:"line2"`
		Line3 string `json:"line3"`
	} `json:"three_line"`
}

// Zone is the part of a zone snapshot this app shows and controls.
type Zone struct {
	ZoneID              string      `json:"zone_id"`
	DisplayName         string      `json:"display_name"`
	State               string      `json:"state"`
	QueueItemsRemaining int         `json:"queue_items_remaining"`
	NowPlaying          *NowPlaying `json:"now_playing"`
	Outputs             []Output    `json:"outputs"`
}

// ZonesPayload is one zone subscription message. Roon sends the first snapshot
// in zones and later pushes as zones_added / zones_changed / zones_removed.
type ZonesPayload struct {
	Zones            []Zone   `json:"zones"`
	ZonesAdded       []Zone   `json:"zones_added"`
	ZonesChanged     []Zone   `json:"zones_changed"`
	ZonesRemoved     []string `json:"zones_removed"`
	ZonesSeekChanged []struct {
		ZoneID       string `json:"zone_id"`
		SeekPosition int    `json:"seek_position"`
	} `json:"zones_seek_changed"`
}

// SubscribeZones starts the transport subscription. subscription_key is an
// integer in Roon's API — a string is answered with InvalidRequest.
func (c *Client) SubscribeZones() (ZonesPayload, string, error) {
	service := c.TransportService()
	message, err := c.RequestFirst(service+"/subscribe_zones", `{"subscription_key":0}`, 15*time.Second)
	if err != nil && service == TransportV2 {
		// A core can answer Unauthorized for :2 and still serve :1.
		message, err = c.RequestFirst(TransportV1+"/subscribe_zones", `{"subscription_key":0}`, 15*time.Second)
		if err == nil {
			c.transport.Store(TransportV1)
			service = TransportV1
		}
	}
	if err != nil {
		return ZonesPayload{}, "", err
	}
	return decodeZones(message, service)
}

func decodeZones(message Message, service string) (ZonesPayload, string, error) {
	var payload ZonesPayload
	if err := json.Unmarshal([]byte(message.Body), &payload); err != nil {
		return ZonesPayload{}, service, err
	}
	return payload, service, nil
}

// ZoneUpdate decodes a pushed subscription message, if it is one.
func ZoneUpdate(message Message) (ZonesPayload, bool) {
	if message.Kind != "COMPLETE" && message.Kind != "CONTINUE" {
		return ZonesPayload{}, false
	}
	var payload ZonesPayload
	if err := json.Unmarshal([]byte(message.Body), &payload); err != nil {
		return ZonesPayload{}, false
	}
	// A control response carries no zones; only a message that actually has
	// them is a zone update.
	if len(payload.Zones) == 0 && len(payload.ZonesAdded) == 0 && len(payload.ZonesChanged) == 0 &&
		len(payload.ZonesRemoved) == 0 && len(payload.ZonesSeekChanged) == 0 {
		return ZonesPayload{}, false
	}
	return payload, true
}

// Control sends one transport command to a zone. control is one of play,
// pause, playpause, stop, next, previous.
func (c *Client) Control(service, zoneID, control string) error {
	body, err := json.Marshal(map[string]any{
		"zone_or_output_id": zoneID,
		"control":           control,
	})
	if err != nil {
		return err
	}
	_, err = c.Request(service+"/control", string(body), 8*time.Second)
	return err
}

// ChangeVolume sets one output's volume. how is "absolute" (the value is in
// the output's own units) or "relative".
func (c *Client) ChangeVolume(service, outputID, how string, value int) error {
	body, err := json.Marshal(map[string]any{
		"output_id": outputID,
		"how":       how,
		"value":     value,
	})
	if err != nil {
		return err
	}
	_, err = c.Request(service+"/change_volume", string(body), 8*time.Second)
	return err
}
