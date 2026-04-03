package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	cloudsync "github.com/bonkcn/ccp-switcher/internal/sync"
)

// parseSyncConfigFromForm reads sync config from form values, merges with DB for secret.
func (s *Server) parseSyncConfigFromForm(r *http.Request) cloudsync.Config {
	cfg := cloudsync.Config{
		Type:   strings.TrimSpace(r.FormValue("sync_type")),
		URL:    strings.TrimSpace(r.FormValue("sync_url")),
		Bucket: strings.TrimSpace(r.FormValue("sync_bucket")),
		Region: strings.TrimSpace(r.FormValue("sync_region")),
		KeyID:  strings.TrimSpace(r.FormValue("sync_key_id")),
		Path:   strings.TrimSpace(r.FormValue("sync_path")),
		Auto:   r.FormValue("sync_auto") == "on",
	}
	if secret := strings.TrimSpace(r.FormValue("sync_secret")); secret != "" {
		cfg.Secret = secret
	} else {
		existing, _ := cloudsync.LoadConfig(s.store)
		cfg.Secret = existing.Secret
	}
	if cfg.Path == "" {
		cfg.Path = cloudsync.DefaultPath
	}
	if v, err := strconv.Atoi(strings.TrimSpace(r.FormValue("sync_interval"))); err == nil && v > 0 {
		cfg.Interval = v
	} else {
		cfg.Interval = 30
	}
	return cfg
}

// saveSyncConfig persists config and restarts auto-sync. Returns error message or "".
func (s *Server) saveSyncConfig(cfg cloudsync.Config) string {
	if err := cloudsync.SaveConfig(s.store, cfg); err != nil {
		return "保存同步配置失败: " + err.Error()
	}
	s.restartAutoSync()
	return ""
}

func (s *Server) handleSyncSave(w http.ResponseWriter, r *http.Request) {
	cfg := s.parseSyncConfigFromForm(r)
	if errMsg := s.saveSyncConfig(cfg); errMsg != "" {
		s.redirectWithMessage(w, r, "/sync", "", errMsg)
		return
	}
	s.redirectWithMessage(w, r, "/sync", "云同步配置已保存", "")
}

func (s *Server) handleSyncTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.parseSyncConfigFromForm(r)
	if errMsg := s.saveSyncConfig(cfg); errMsg != "" {
		s.redirectWithMessage(w, r, "/sync", "", errMsg)
		return
	}

	backend, err := cloudsync.NewBackend(cfg)
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "同步未启用或配置不完整: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := backend.Test(ctx); err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "连接测试失败: "+err.Error())
		return
	}

	s.redirectWithMessage(w, r, "/sync", "配置已保存，连接测试成功", "")
}

func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	cfg := s.parseSyncConfigFromForm(r)
	if errMsg := s.saveSyncConfig(cfg); errMsg != "" {
		s.redirectWithMessage(w, r, "/sync", "", errMsg)
		return
	}

	backend, err := cloudsync.NewBackend(cfg)
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "同步未启用: "+err.Error())
		return
	}

	data, err := s.exportProvidersJSON()
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "导出数据失败: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := backend.Push(ctx, data); err != nil {
		s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
			st.LastError = err.Error()
			st.LastErrorAt = time.Now()
		})
		s.redirectWithMessage(w, r, "/sync", "", "推送失败: "+err.Error())
		return
	}

	s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
		st.LastPushAt = time.Now()
		st.LastError = ""
	})

	s.redirectWithMessage(w, r, "/sync", "配置已保存，已推送到远端", "")
}

func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	cfg := s.parseSyncConfigFromForm(r)
	if errMsg := s.saveSyncConfig(cfg); errMsg != "" {
		s.redirectWithMessage(w, r, "/sync", "", errMsg)
		return
	}

	backend, err := cloudsync.NewBackend(cfg)
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "同步未启用: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	data, err := backend.Pull(ctx)
	if err != nil {
		s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
			st.LastError = err.Error()
			st.LastErrorAt = time.Now()
		})
		s.redirectWithMessage(w, r, "/sync", "", "拉取失败: "+err.Error())
		return
	}

	payload, err := decodeProviderTransfer(data)
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "解析远端数据失败: "+err.Error())
		return
	}

	result, err := s.importProviderTransfer(payload, true)
	if err != nil {
		s.redirectWithMessage(w, r, "/sync", "", "导入失败: "+err.Error())
		return
	}

	s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
		st.LastPullAt = time.Now()
		st.LastError = ""
	})

	s.redirectWithMessage(w, r, "/sync", "拉取完成: "+result.summary(), "")
}

func (s *Server) exportProvidersJSON() ([]byte, error) {
	providers, err := s.store.ListProviders("")
	if err != nil {
		return nil, err
	}
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		return nil, err
	}
	payload := buildProviderTransferFile(providers, activeIDs)
	return marshalJSON(payload)
}

func (s *Server) updateSyncStatus(fn func(*cloudsync.SyncStatus)) {
	status, _ := cloudsync.LoadStatus(s.store)
	fn(&status)
	_ = cloudsync.SaveStatus(s.store, status)
}

// StartAutoSync starts the background auto-sync goroutine if configured.
func (s *Server) StartAutoSync() {
	s.syncStop = make(chan struct{})
	go s.autoSyncLoop()
}

func (s *Server) restartAutoSync() {
	if s.syncStop != nil {
		close(s.syncStop)
	}
	s.syncStop = make(chan struct{})
	go s.autoSyncLoop()
}

func (s *Server) autoSyncLoop() {
	stop := s.syncStop
	for {
		cfg, err := cloudsync.LoadConfig(s.store)
		if err != nil || !cfg.Auto || cfg.Type == "" || cfg.Type == "disabled" {
			return
		}

		interval := time.Duration(cfg.Interval) * time.Minute
		if interval < time.Minute {
			interval = time.Minute
		}

		select {
		case <-stop:
			return
		case <-time.After(interval):
		}

		cfg, err = cloudsync.LoadConfig(s.store)
		if err != nil || !cfg.Auto || cfg.Type == "" || cfg.Type == "disabled" {
			return
		}

		backend, err := cloudsync.NewBackend(cfg)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		data, err := s.exportProvidersJSON()
		if err != nil {
			cancel()
			s.logger.Printf("[sync] export failed: %v", err)
			continue
		}
		if err := backend.Push(ctx, data); err != nil {
			s.logger.Printf("[sync] auto push failed: %v", err)
			s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
				st.LastError = err.Error()
				st.LastErrorAt = time.Now()
			})
		} else {
			s.updateSyncStatus(func(st *cloudsync.SyncStatus) {
				st.LastPushAt = time.Now()
				st.LastError = ""
			})
		}
		cancel()
	}
}

func marshalJSON(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
