//go:build linux && ghostlink_crud_smoke

package ghostlink

import "time"

type SmokeControllerConfig struct {
	UnitName          string
	SystemctlPath     string
	BinaryPath        string
	ConfigPath        string
	StateDBPath       string
	ControlSocketPath string
	OperationPath     string
	PeerUID           uint32
	ServerUID         uint32
}

func NewSmokeController(cfg SmokeControllerConfig) Controller {
	return newSystemdController(controllerConfig{
		unitName:            cfg.UnitName,
		binaryPath:          cfg.BinaryPath,
		configPath:          cfg.ConfigPath,
		stateDBPath:         cfg.StateDBPath,
		startWait:           8 * time.Second,
		pollInterval:        100 * time.Millisecond,
		preflightPorts:      true,
		controlSocket:       cfg.ControlSocketPath,
		controlPeerUID:      cfg.PeerUID,
		controlServerUID:    cfg.ServerUID,
		allowTestPaths:      true,
		userOperationPath:   cfg.OperationPath,
		allowPrivateTargets: true,
	}, &systemctlManager{path: cfg.SystemctlPath, unit: cfg.UnitName})
}
