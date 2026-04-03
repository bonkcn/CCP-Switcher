package sync

import (
	"encoding/json"
	"fmt"
)

// Setting keys for storing sync config in app_settings.
const (
	SettingType     = "sync_type"
	SettingURL      = "sync_url"
	SettingBucket   = "sync_bucket"
	SettingRegion   = "sync_region"
	SettingKeyID    = "sync_key_id"
	SettingSecret   = "sync_secret"
	SettingPath     = "sync_path"
	SettingAuto     = "sync_auto"
	SettingInterval = "sync_interval"
	SettingStatus   = "sync_status"
)

// SettingGetter reads a setting by key.
type SettingGetter interface {
	GetSetting(key string) (string, error)
}

// SettingSetter writes a setting by key.
type SettingSetter interface {
	SetSetting(key string, value string) error
}

// SettingStore combines getter and setter.
type SettingStore interface {
	SettingGetter
	SettingSetter
}

// LoadConfig reads sync configuration from the settings store.
func LoadConfig(st SettingGetter) (Config, error) {
	cfg := Config{Path: DefaultPath, Interval: 30}

	if v, err := st.GetSetting(SettingType); err != nil {
		return cfg, err
	} else {
		cfg.Type = v
	}
	if v, err := st.GetSetting(SettingURL); err != nil {
		return cfg, err
	} else {
		cfg.URL = v
	}
	if v, err := st.GetSetting(SettingBucket); err != nil {
		return cfg, err
	} else {
		cfg.Bucket = v
	}
	if v, err := st.GetSetting(SettingRegion); err != nil {
		return cfg, err
	} else {
		cfg.Region = v
	}
	if v, err := st.GetSetting(SettingKeyID); err != nil {
		return cfg, err
	} else {
		cfg.KeyID = v
	}
	if v, err := st.GetSetting(SettingSecret); err != nil {
		return cfg, err
	} else {
		cfg.Secret = v
	}
	if v, err := st.GetSetting(SettingPath); err != nil {
		return cfg, err
	} else if v != "" {
		cfg.Path = v
	}
	if v, err := st.GetSetting(SettingAuto); err != nil {
		return cfg, err
	} else {
		cfg.Auto = v == "true"
	}
	if v, err := st.GetSetting(SettingInterval); err != nil {
		return cfg, err
	} else if v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Interval = n
		}
	}
	return cfg, nil
}

// SaveConfig writes sync configuration to the settings store.
func SaveConfig(st SettingSetter, cfg Config) error {
	pairs := map[string]string{
		SettingType:     cfg.Type,
		SettingURL:      cfg.URL,
		SettingBucket:   cfg.Bucket,
		SettingRegion:   cfg.Region,
		SettingKeyID:    cfg.KeyID,
		SettingSecret:   cfg.Secret,
		SettingPath:     cfg.Path,
		SettingAuto:     fmt.Sprintf("%v", cfg.Auto),
		SettingInterval: fmt.Sprintf("%d", cfg.Interval),
	}
	for k, v := range pairs {
		if err := st.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// LoadStatus reads the last sync status from the store.
func LoadStatus(st SettingGetter) (SyncStatus, error) {
	var status SyncStatus
	v, err := st.GetSetting(SettingStatus)
	if err != nil || v == "" {
		return status, err
	}
	_ = json.Unmarshal([]byte(v), &status)
	return status, nil
}

// SaveStatus writes the sync status to the store.
func SaveStatus(st SettingSetter, status SyncStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return st.SetSetting(SettingStatus, string(data))
}
