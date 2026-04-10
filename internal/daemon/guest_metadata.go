package daemon

import (
	"fmt"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const (
	defaultMMDSIPv4Address    = "169.254.170.2"
	defaultMMDSPayloadVersion = "v1"
)

type guestMetadataEnvelope struct {
	Latest guestMetadataRoot `json:"latest"`
}

type guestMetadataRoot struct {
	MetaData guestMetadataPayload `json:"meta-data"`
}

type guestMetadataPayload struct {
	Version           string                          `json:"version"`
	MachineID         string                          `json:"machine_id"`
	Hostname          string                          `json:"hostname"`
	AuthorizedKeys    []string                        `json:"authorized_keys,omitempty"`
	TrustedUserCAKeys []string                        `json:"trusted_user_ca_keys,omitempty"`
	LoginWebhook      *contracthost.GuestLoginWebhook `json:"login_webhook,omitempty"`
}

func cloneGuestConfig(config *contracthost.GuestConfig) *contracthost.GuestConfig {
	if config == nil {
		return nil
	}
	cloned := &contracthost.GuestConfig{
		Hostname:          config.Hostname,
		AuthorizedKeys:    append([]string(nil), config.AuthorizedKeys...),
		TrustedUserCAKeys: append([]string(nil), config.TrustedUserCAKeys...),
	}
	if config.LoginWebhook != nil {
		copy := *config.LoginWebhook
		cloned.LoginWebhook = &copy
	}
	return cloned
}

func guestHostname(machineID contracthost.MachineID, guestConfig *contracthost.GuestConfig) string {
	if guestConfig != nil {
		if hostname := strings.TrimSpace(guestConfig.Hostname); hostname != "" {
			return hostname
		}
	}
	return strings.TrimSpace(string(machineID))
}

func (d *Daemon) guestMetadataSpec(machineID contracthost.MachineID, guestConfig *contracthost.GuestConfig) (*firecracker.MMDSSpec, error) {
	name := guestHostname(machineID, guestConfig)
	if name == "" {
		return nil, fmt.Errorf("machine id is required")
	}

	payload := guestMetadataEnvelope{
		Latest: guestMetadataRoot{
			MetaData: guestMetadataPayload{
				Version:           defaultMMDSPayloadVersion,
				MachineID:         name,
				Hostname:          name,
				AuthorizedKeys:    nil,
				TrustedUserCAKeys: nil,
			},
		},
	}
	if guestConfig != nil {
		payload.Latest.MetaData.AuthorizedKeys = append([]string(nil), guestConfig.AuthorizedKeys...)
		payload.Latest.MetaData.TrustedUserCAKeys = append([]string(nil), guestConfig.TrustedUserCAKeys...)
		if guestConfig.LoginWebhook != nil {
			loginWebhook := *guestConfig.LoginWebhook
			payload.Latest.MetaData.LoginWebhook = &loginWebhook
		}
	}

	return &firecracker.MMDSSpec{
		NetworkInterfaces: []string{"net0"},
		Version:           firecracker.MMDSVersionV2,
		IPv4Address:       defaultMMDSIPv4Address,
		Data:              payload,
	}, nil
}
