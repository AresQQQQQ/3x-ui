package service

import (
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"

	"gorm.io/gorm"
)

// GhostLinkClientPolicy is a secret-free business-state projection used by the
// authenticated client pages. It is not an authorization source.
type GhostLinkClientPolicy struct {
	PolicyState      string `json:"policyState"`
	PolicyVersion    int    `json:"policyVersion"`
	EffectiveEnabled bool   `json:"effectiveEnabled"`
	UsageState       string `json:"usageState"`
	UsageErrorCode   string `json:"usageErrorCode,omitempty"`
	PolicyErrorCode  string `json:"policyErrorCode,omitempty"`
	QuotaBytes       int64  `json:"quotaBytes"`
	MaxTrafficBytes  int64  `json:"maxTrafficBytes"`
	ExpiryTime       int64  `json:"expiryTime"`
	LastSnapshotAt   int64  `json:"lastSnapshotAt"`
}

func loadGhostLinkClientPolicies(
	db *gorm.DB,
	attachments map[int][]int,
) (map[int]*GhostLinkClientPolicy, error) {
	policies := make(map[int]*GhostLinkClientPolicy)
	if len(attachments) == 0 {
		return policies, nil
	}
	allInboundIDs := make([]int, 0, len(attachments))
	for _, ids := range attachments {
		allInboundIDs = append(allInboundIDs, ids...)
	}
	var ghostLinkInbounds []model.Inbound
	if err := db.Model(&model.Inbound{}).
		Select("id").
		Where("protocol = ? AND id IN ?", model.GhostLink, uniqueInts(allInboundIDs)).
		Find(&ghostLinkInbounds).Error; err != nil {
		return nil, err
	}
	ghostLinkInboundSet := make(map[int]struct{}, len(ghostLinkInbounds))
	for _, inbound := range ghostLinkInbounds {
		ghostLinkInboundSet[inbound.Id] = struct{}{}
	}
	if len(ghostLinkInboundSet) == 0 {
		return policies, nil
	}

	attached := make(map[int]struct{})
	for clientID, inboundIDs := range attachments {
		for _, inboundID := range inboundIDs {
			if _, ok := ghostLinkInboundSet[inboundID]; ok {
				attached[clientID] = struct{}{}
				break
			}
		}
	}
	if len(attached) == 0 {
		return policies, nil
	}
	clientIDs := make([]int, 0, len(attached))
	for clientID := range attached {
		clientIDs = append(clientIDs, clientID)
		policies[clientID] = &GhostLinkClientPolicy{
			PolicyState: "uninitialized",
			UsageState:  "uninitialized",
		}
	}

	var records []model.ClientRecord
	if err := db.Select("id", "uuid").Where("id IN ?", clientIDs).Find(&records).Error; err != nil {
		return nil, err
	}
	idByUUID := make(map[string]int, len(records))
	clientUUIDs := make([]string, 0, len(records))
	for _, record := range records {
		idByUUID[record.UUID] = record.Id
		clientUUIDs = append(clientUUIDs, record.UUID)
	}

	var rows []model.GhostLinkUsageWatermark
	if err := db.Where("client_id IN ?", clientUUIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		clientID, ok := idByUUID[row.ClientId]
		if !ok {
			continue
		}
		policyState := row.PolicyState
		usageState := row.State
		if row.PolicyVersion <= 0 || policyState == "" {
			policyState = "uninitialized"
		}
		if usageState == "" {
			usageState = "uninitialized"
		}
		policies[clientID] = &GhostLinkClientPolicy{
			PolicyState:      policyState,
			PolicyVersion:    row.PolicyVersion,
			EffectiveEnabled: row.EffectiveEnabled,
			UsageState:       usageState,
			UsageErrorCode:   row.ErrorCode,
			PolicyErrorCode:  row.PolicyErrorCode,
			QuotaBytes:       row.QuotaBytes,
			MaxTrafficBytes:  row.MaxTrafficBytes,
			ExpiryTime:       row.ExpiryTime,
			LastSnapshotAt:   row.LastSnapshotAt,
		}
	}
	return policies, nil
}
