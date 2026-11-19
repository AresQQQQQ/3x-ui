package model

// GhostLinkUsageWatermark stores the last committed cumulative Core counters
// observed for one stable GhostLink ClientID. It is accounting metadata, not a
// second user or quota database.
type GhostLinkUsageWatermark struct {
	Id               int    `gorm:"primaryKey;autoIncrement"`
	InboundId        int    `gorm:"not null;uniqueIndex:idx_ghostlink_usage_identity,priority:1"`
	ClientId         string `gorm:"not null;uniqueIndex:idx_ghostlink_usage_identity,priority:2"`
	Email            string `gorm:"not null;index:idx_ghostlink_usage_email"`
	StoreId          string `gorm:"not null"`
	UploadBytes      int64  `gorm:"not null;default:0"`
	DownloadBytes    int64  `gorm:"not null;default:0"`
	State            string `gorm:"not null;default:baseline"`
	ErrorCode        string `gorm:"not null;default:''"`
	ManualEnabled    bool   `gorm:"not null;default:false"`
	EffectiveEnabled bool   `gorm:"not null;default:false"`
	PolicyVersion    int    `gorm:"not null;default:0"`
	PolicyState      string `gorm:"not null;default:''"`
	PolicyErrorCode  string `gorm:"not null;default:''"`
	QuotaBytes       int64  `gorm:"not null;default:0"`
	ExpiryTime       int64  `gorm:"not null;default:0"`
	MaxTrafficBytes  int64  `gorm:"not null;default:0"`
	TrafficReset     string `gorm:"not null;default:never"`
	TrafficResetDay  int    `gorm:"not null;default:0"`
	NextResetAt      int64  `gorm:"not null;default:0"`
	LastResetAt      int64  `gorm:"not null;default:0"`
	LastSnapshotAt   int64  `gorm:"not null;default:0"`
	CreatedAt        int64  `gorm:"autoCreateTime:milli"`
	UpdatedAt        int64  `gorm:"autoUpdateTime:milli"`
}
