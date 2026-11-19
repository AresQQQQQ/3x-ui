// Package fingerprint defines the public host fingerprint wire format.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

type Bundle struct {
	MachineID string   `json:"machine_id_hash"`
	DMIUUID   string   `json:"dmi_uuid_hash"`
	MACs      []string `json:"mac_hashes"`
}

func Collect() (Bundle, error) {
	if runtime.GOOS != "linux" {
		return Bundle{}, errors.New("GhostLink host fingerprints require Linux")
	}
	return collect("/"), nil
}

func collect(root string) Bundle {
	anchor := func(domain string, paths ...string) string {
		for _, path := range paths {
			value := read(root, path)
			digits := strings.ReplaceAll(value, "-", "")
			if raw, err := hex.DecodeString(digits); err == nil && len(raw) == 16 &&
				digits != strings.Repeat("0", 32) && digits != strings.Repeat("f", 32) {
				return digest(domain, digits)
			}
		}
		return ""
	}
	bundle := Bundle{
		MachineID: anchor("machine_id", "host/etc/machine-id", "host/var/lib/dbus/machine-id", "etc/machine-id", "var/lib/dbus/machine-id"),
		DMIUUID:   anchor("dmi_uuid", "host/sys/class/dmi/id/product_uuid", "sys/class/dmi/id/product_uuid"),
		MACs:      []string{},
	}
	for _, dir := range []string{"host/sys/class/net", "sys/class/net"} {
		entries, _ := os.ReadDir(filepath.Join(root, dir))
		for _, entry := range entries {
			name := entry.Name()
			if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
				continue
			}
			addr, err := net.ParseMAC(read(root, filepath.Join(dir, name, "address")))
			if err == nil && len(addr) == 6 && addr[0]&1 == 0 && addr.String() != "00:00:00:00:00:00" {
				bundle.MACs = append(bundle.MACs, digest("mac", addr.String()))
			}
		}
		if len(bundle.MACs) > 0 {
			break
		}
	}
	slices.Sort(bundle.MACs)
	bundle.MACs = slices.Compact(bundle.MACs)
	return bundle
}

func read(root, path string) string {
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(data)))
}

func digest(domain, value string) string {
	sum := sha256.Sum256([]byte("ghostlink-fp-v1:" + domain + ":" + value))
	return hex.EncodeToString(sum[:])
}

func (b Bundle) Validate() error {
	valid := func(s string) bool {
		raw, err := hex.DecodeString(s)
		return err == nil && len(raw) == sha256.Size && s == strings.ToLower(s)
	}
	if !valid(b.MachineID) || (b.DMIUUID != "" && !valid(b.DMIUUID)) || len(b.MACs) > 256 ||
		(b.DMIUUID == "" && len(b.MACs) == 0) {
		return errors.New("incomplete or invalid host fingerprint")
	}
	for _, mac := range b.MACs {
		if !valid(mac) {
			return errors.New("invalid MAC fingerprint")
		}
	}
	return nil
}

func (b Bundle) Matches(live Bundle) bool {
	machine := b.MachineID != "" && b.MachineID == live.MachineID
	dmi := b.DMIUUID != "" && b.DMIUUID == live.DMIUUID
	if machine && dmi {
		return true
	}
	if !machine && !dmi {
		return false
	}
	for _, mac := range b.MACs {
		if slices.Contains(live.MACs, mac) {
			return true
		}
	}
	return false
}

// Preview returns shortened hashes for the panel and approval UI.
func (b Bundle) Preview() Bundle {
	short := func(s string) string {
		if len(s) <= 12 {
			return s
		}
		return s[:12] + "…"
	}
	result := Bundle{MachineID: short(b.MachineID), DMIUUID: short(b.DMIUUID), MACs: make([]string, len(b.MACs))}
	for i, mac := range b.MACs {
		result.MACs[i] = short(mac)
	}
	return result
}
