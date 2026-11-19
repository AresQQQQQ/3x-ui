package sub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	ghostlinkconfig "github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
)

var errGhostLinkMihomoOnly = errors.New(
	"unsupported-format: GhostLink credentials are available only in the custom Mihomo/Clash YAML subscription",
)

func (s *SubService) matchingSubscriptionClients(inbound *model.Inbound, subId string) ([]model.Client, error) {
	clients := s.matchingClients(inbound, subId)
	if inbound == nil || inbound.Protocol != model.GhostLink || len(clients) == 0 {
		return clients, nil
	}
	if inbound.NodeID != nil {
		return nil, fmt.Errorf("GhostLink subscription is local-only; a node-managed GhostLink inbound is unsupported")
	}

	var embedded struct {
		Clients []model.Client `json:"clients"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &embedded); err != nil {
		return nil, fmt.Errorf("GhostLink subscription settings are invalid")
	}
	embeddedByEmail := make(map[string]model.Client, len(embedded.Clients))
	for _, client := range embedded.Clients {
		embeddedByEmail[strings.ToLower(client.Email)] = client
	}

	var rows []model.GhostLinkUsageWatermark
	if err := database.GetDB().
		Where("inbound_id = ?", inbound.Id).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load GhostLink subscription policy: %w", err)
	}
	rowsByClientID := make(map[string]model.GhostLinkUsageWatermark, len(rows))
	for _, row := range rows {
		rowsByClientID[row.ClientId] = row
	}

	now := time.Now().UnixMilli()
	eligible := make([]model.Client, 0, len(clients))
	for _, client := range clients {
		// Client.Enable is only administrator intent. A watermark is required
		// before its credentials may leave through a subscription.
		if !client.Enable {
			continue
		}
		embeddedClient, ok := embeddedByEmail[strings.ToLower(client.Email)]
		if !ok || embeddedClient.ID != client.ID || embeddedClient.Secret == "" ||
			embeddedClient.Enable != client.Enable {
			continue
		}
		row, ok := rowsByClientID[client.ID]
		if !ok || !strings.EqualFold(row.Email, client.Email) {
			continue
		}
		if row.PolicyVersion <= 0 ||
			row.PolicyState != "ok" ||
			row.ManualEnabled != client.Enable ||
			!row.EffectiveEnabled ||
			(row.State != "ok" && row.State != "baseline") ||
			row.ErrorCode != "" ||
			row.PolicyErrorCode != "" {
			continue
		}
		if row.ExpiryTime > 0 && now >= row.ExpiryTime {
			continue
		}
		client.Secret = embeddedClient.Secret
		eligible = append(eligible, client)
	}
	return eligible, nil
}

func ghostLinkUnsupportedFormat(format string) error {
	return fmt.Errorf("%w (requested %s)", errGhostLinkMihomoOnly, format)
}

// ghostLinkMihomoSettings projects the panel-owned inbound settings into the
// exact custom-Mihomo fields accepted by the native GhostLink outbound.
func ghostLinkMihomoSettings(inbound *model.Inbound) (map[string]any, error) {
	if inbound == nil || inbound.Protocol != model.GhostLink {
		return nil, errors.New("not a GhostLink inbound")
	}
	var settings struct {
		Version         int                                `json:"version"`
		TransportPreset string                             `json:"transportPreset"`
		ServerName      string                             `json:"serverName"`
		IdentityTLS     *ghostlinkconfig.IdentityTLSConfig `json:"identityTls"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return nil, fmt.Errorf("decode GhostLink subscription settings: %w", err)
	}
	transport, err := ghostlinkconfig.ResolveTransportPreset(settings.TransportPreset)
	if err != nil {
		return nil, err
	}
	preset := transport.Preset
	if preset == "" {
		preset = ghostlinkconfig.PresetDual
	}
	fields := map[string]any{
		"transport":          preset,
		"enable-udp-capsule": transport.EnableH2UDPCapsule,
		"h2-scale-threshold": ghostlinkconfig.DefaultH2Scale,
	}
	if preset == ghostlinkconfig.PresetH2 || preset == ghostlinkconfig.PresetH2Capsule {
		fields["early-data"] = true
	}
	if serverName := strings.TrimSpace(settings.ServerName); serverName != "" {
		fields["server-name"] = serverName
	}
	if identity := settings.IdentityTLS; identity != nil {
		for field, value := range map[string]string{
			"identity-client-id":  identity.ClientID,
			"identity-secret":     identity.Secret,
			"identity-public-key": identity.PublicKey,
			"identity-short-id":   identity.ShortID,
			"sni":                 identity.ServerName,
		} {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("GhostLink identityTls is incomplete: %s is empty", field)
			}
			fields[field] = value
		}
	} else if transport.Preset != "" {
		return nil, errors.New("GhostLink selected transport preset requires identityTls")
	}
	return fields, nil
}
