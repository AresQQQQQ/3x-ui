package service

import (
	"encoding/hex"
	"errors"
	"strings"

	xrayuuid "github.com/xtls/xray-core/common/uuid"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/util/common"

	"gorm.io/gorm"
)

func validateClientAttachmentCompatibility(db *gorm.DB, inboundIDs []int, clientID string) error {
	if len(inboundIDs) == 0 {
		return nil
	}
	var protocols []model.Protocol
	if err := db.Model(&model.Inbound{}).
		Where("id IN ?", uniqueInts(inboundIDs)).
		Pluck("protocol", &protocols).Error; err != nil {
		return err
	}
	return validateProtocolSetCompatibility(protocols, clientID)
}

func validateProtocolSetCompatibility(protocols []model.Protocol, clientID string) error {
	var ghostLink, mtproto, uuidIdentity bool
	for _, protocol := range protocols {
		switch protocol {
		case model.GhostLink:
			ghostLink = true
		case model.MTProto:
			mtproto = true
		case model.VMESS, model.VLESS, model.TUIC:
			uuidIdentity = true
		}
	}
	if ghostLink && mtproto {
		return common.NewError(
			"a client cannot be attached to both GhostLink and MTProto because they use conflicting Secret formats",
		)
	}
	if ghostLink && uuidIdentity && !isStandardXrayUUID(clientID) {
		return common.NewError(
			"a client attached to both GhostLink and VMess/VLESS/TUIC requires a standard 32-hex or 36-character UUID Client ID; GhostLink-only clients may use a custom printable Client ID",
		)
	}
	return nil
}

func validateInboundAttachmentCompatibility(
	db *gorm.DB,
	inboundID int,
	inboundProtocol model.Protocol,
	clients []model.Client,
) error {
	if len(clients) == 0 {
		return nil
	}
	emails := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Email != "" {
			emails = append(emails, client.Email)
		}
	}
	if len(emails) == 0 {
		return nil
	}

	type attachment struct {
		Email     string
		InboundID int
	}
	var rows []attachment
	if err := db.Table("client_inbounds").
		Select("clients.email AS email, client_inbounds.inbound_id AS inbound_id").
		Joins("JOIN clients ON clients.id = client_inbounds.client_id").
		Where("clients.email IN ?", emails).
		Scan(&rows).Error; err != nil {
		return err
	}
	idsByEmail := make(map[string][]int, len(emails))
	allIDs := []int{inboundID}
	for _, row := range rows {
		idsByEmail[strings.ToLower(row.Email)] = append(
			idsByEmail[strings.ToLower(row.Email)],
			row.InboundID,
		)
		allIDs = append(allIDs, row.InboundID)
	}
	var inbounds []model.Inbound
	if err := db.Select("id", "protocol").
		Where("id IN ?", uniqueInts(allIDs)).
		Find(&inbounds).Error; err != nil {
		return err
	}
	protocolByID := make(map[int]model.Protocol, len(inbounds))
	for _, inbound := range inbounds {
		protocolByID[inbound.Id] = inbound.Protocol
	}
	protocolByID[inboundID] = inboundProtocol

	var records []model.ClientRecord
	if err := db.Select("email", "uuid").Where("email IN ?", emails).Find(&records).Error; err != nil {
		return err
	}
	uuidByEmail := make(map[string]string, len(records))
	for _, record := range records {
		uuidByEmail[strings.ToLower(record.Email)] = record.UUID
	}

	for _, client := range clients {
		key := strings.ToLower(client.Email)
		clientID := client.ID
		if clientID == "" {
			clientID = uuidByEmail[key]
		}
		protocols := []model.Protocol{inboundProtocol}
		for _, id := range idsByEmail[key] {
			if protocol, ok := protocolByID[id]; ok {
				protocols = append(protocols, protocol)
			}
		}
		if err := validateProtocolSetCompatibility(protocols, clientID); err != nil {
			return err
		}
	}
	return nil
}

func isStandardXrayUUID(value string) bool {
	if len(value) != 32 && len(value) != 36 {
		// Xray hashes arbitrary short IDs for compatibility. A mixed identity
		// must opt into the explicit standard UUID forms instead.
		return false
	}
	if len(value) == 36 &&
		(value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-') {
		return false
	}
	hexValue := strings.ReplaceAll(value, "-", "")
	if _, err := hex.DecodeString(hexValue); err != nil {
		return false
	}
	_, err := xrayuuid.ParseString(value)
	return err == nil
}

func effectiveAttachmentClientID(db *gorm.DB, email, provided string) (string, error) {
	if provided != "" || email == "" {
		return provided, nil
	}
	var record model.ClientRecord
	err := db.Select("uuid").Where("email = ?", email).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return record.UUID, nil
}
