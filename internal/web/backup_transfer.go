package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cloudsync "github.com/bonkcn/ccp-switcher/internal/sync"
)

const (
	fullBackupFileType     = "ccp-switcher/full"
	fullBackupFormatV1 int = 1
)

type fullBackupFile struct {
	Type         string                `json:"type"`
	Version      int                   `json:"version"`
	ExportedAt   string                `json:"exported_at,omitempty"`
	Providers    *providerTransferFile `json:"providers,omitempty"`
	SyncConfig   *syncConfigTransfer   `json:"sync_config,omitempty"`
	SiteSettings *siteSettingsTransfer `json:"site_settings,omitempty"`
}

type syncConfigTransfer struct {
	Type     string  `json:"type"`
	URL      string  `json:"url"`
	Bucket   string  `json:"bucket"`
	Region   string  `json:"region"`
	KeyID    string  `json:"key_id"`
	Secret   *string `json:"secret,omitempty"`
	Path     string  `json:"path"`
	Auto     bool    `json:"auto"`
	Interval int     `json:"interval"`
}

type siteSettingsTransfer struct {
	SiteName string `json:"site_name"`
}

type fullBackupImportResult struct {
	ProviderResult       *providerImportResult
	RestoredSyncConfig   bool
	RestoredSiteSettings bool
}

func buildFullBackupFile(providerPayload providerTransferFile, syncCfg cloudsync.Config, siteName string) fullBackupFile {
	return fullBackupFile{
		Type:         fullBackupFileType,
		Version:      fullBackupFormatV1,
		ExportedAt:   time.Now().UTC().Format(time.RFC3339),
		Providers:    &providerPayload,
		SyncConfig:   syncConfigTransferFromConfig(syncCfg),
		SiteSettings: &siteSettingsTransfer{SiteName: strings.TrimSpace(siteName)},
	}
}

func syncConfigTransferFromConfig(cfg cloudsync.Config) *syncConfigTransfer {
	if cfg.Path == "" {
		cfg.Path = cloudsync.DefaultPath
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30
	}
	secret := cfg.Secret
	return &syncConfigTransfer{
		Type:     strings.TrimSpace(cfg.Type),
		URL:      strings.TrimSpace(cfg.URL),
		Bucket:   strings.TrimSpace(cfg.Bucket),
		Region:   strings.TrimSpace(cfg.Region),
		KeyID:    strings.TrimSpace(cfg.KeyID),
		Secret:   &secret,
		Path:     strings.TrimSpace(cfg.Path),
		Auto:     cfg.Auto,
		Interval: cfg.Interval,
	}
}

func decodeFullBackup(data []byte) (fullBackupFile, error) {
	var payload fullBackupFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return fullBackupFile{}, fmt.Errorf("导入文件不是合法 JSON: %w", err)
	}
	if err := validateFullBackup(&payload); err != nil {
		return fullBackupFile{}, err
	}
	return payload, nil
}

func decodeAnyBackup(data []byte) (fullBackupFile, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fullBackupFile{}, fmt.Errorf("导入文件不是合法 JSON: %w", err)
	}

	switch strings.TrimSpace(probe.Type) {
	case "", providerTransferFileType:
		providerPayload, err := decodeProviderTransfer(data)
		if err != nil {
			return fullBackupFile{}, err
		}
		return fullBackupFile{
			Type:       fullBackupFileType,
			Version:    fullBackupFormatV1,
			ExportedAt: providerPayload.ExportedAt,
			Providers:  &providerPayload,
		}, nil
	case fullBackupFileType:
		return decodeFullBackup(data)
	default:
		return fullBackupFile{}, fmt.Errorf("不支持的导入文件类型: %s", strings.TrimSpace(probe.Type))
	}
}

func validateFullBackup(payload *fullBackupFile) error {
	payload.Type = strings.TrimSpace(payload.Type)
	if payload.Type == "" {
		payload.Type = fullBackupFileType
	}
	if payload.Type != fullBackupFileType {
		return fmt.Errorf("不支持的导入文件类型: %s", payload.Type)
	}
	switch payload.Version {
	case 0:
		payload.Version = fullBackupFormatV1
	case fullBackupFormatV1:
	default:
		return fmt.Errorf("不支持的导入文件版本: %d", payload.Version)
	}

	if payload.Providers != nil {
		if err := validateProviderTransfer(payload.Providers); err != nil {
			return err
		}
	}
	if payload.SyncConfig != nil {
		normalizeSyncTransfer(payload.SyncConfig)
	}
	if payload.SiteSettings != nil {
		payload.SiteSettings.SiteName = strings.TrimSpace(payload.SiteSettings.SiteName)
	}
	return nil
}

func normalizeSyncTransfer(cfg *syncConfigTransfer) {
	cfg.Type = strings.TrimSpace(cfg.Type)
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.KeyID = strings.TrimSpace(cfg.KeyID)
	cfg.Path = strings.TrimSpace(cfg.Path)
	if cfg.Path == "" {
		cfg.Path = cloudsync.DefaultPath
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30
	}
	if cfg.Secret != nil {
		secret := strings.TrimSpace(*cfg.Secret)
		cfg.Secret = &secret
	}
}

func (cfg syncConfigTransfer) toConfig(existing cloudsync.Config) cloudsync.Config {
	secret := existing.Secret
	if cfg.Secret != nil {
		secret = strings.TrimSpace(*cfg.Secret)
	}
	return cloudsync.Config{
		Type:     strings.TrimSpace(cfg.Type),
		URL:      strings.TrimSpace(cfg.URL),
		Bucket:   strings.TrimSpace(cfg.Bucket),
		Region:   strings.TrimSpace(cfg.Region),
		KeyID:    strings.TrimSpace(cfg.KeyID),
		Secret:   secret,
		Path:     strings.TrimSpace(cfg.Path),
		Auto:     cfg.Auto,
		Interval: cfg.Interval,
	}
}

func (s *Server) exportFullBackupJSON() ([]byte, error) {
	providers, err := s.store.ListProviders("")
	if err != nil {
		return nil, err
	}
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		return nil, err
	}
	syncCfg, err := cloudsync.LoadConfig(s.store)
	if err != nil {
		return nil, err
	}
	siteName, err := s.store.GetSetting("site_name")
	if err != nil {
		return nil, err
	}

	payload := buildFullBackupFile(buildProviderTransferFile(providers, activeIDs), syncCfg, siteName)
	return marshalJSON(payload)
}

func (s *Server) importFullBackup(payload fullBackupFile, restoreActive bool) (fullBackupImportResult, error) {
	var result fullBackupImportResult

	if payload.Providers != nil {
		providerResult, err := s.importProviderTransfer(*payload.Providers, restoreActive)
		result.ProviderResult = &providerResult
		if err != nil {
			return result, err
		}
	}

	if payload.SyncConfig != nil {
		existing, _ := cloudsync.LoadConfig(s.store)
		if err := cloudsync.SaveConfig(s.store, payload.SyncConfig.toConfig(existing)); err != nil {
			return result, wrapFullBackupImportError(result, "恢复云同步配置失败", err)
		}
		s.restartAutoSync()
		result.RestoredSyncConfig = true
	}

	if payload.SiteSettings != nil {
		if err := s.store.SetSetting("site_name", strings.TrimSpace(payload.SiteSettings.SiteName)); err != nil {
			return result, wrapFullBackupImportError(result, "恢复站点配置失败", err)
		}
		result.RestoredSiteSettings = true
	}

	return result, nil
}

func wrapFullBackupImportError(result fullBackupImportResult, message string, err error) error {
	summary := result.summary()
	if summary == "" {
		return fmt.Errorf("%s: %w", message, err)
	}
	return fmt.Errorf("%s；但%s: %w", summary, message, err)
}

func (r fullBackupImportResult) summary() string {
	var parts []string
	if r.ProviderResult != nil {
		parts = append(parts, r.ProviderResult.summary())
	}
	if r.RestoredSyncConfig {
		parts = append(parts, "云同步配置已恢复")
	}
	if r.RestoredSiteSettings {
		parts = append(parts, "站点设置已恢复")
	}
	return strings.Join(parts, "；")
}
