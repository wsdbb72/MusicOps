// Package configmigrate migrates a Python desktop profile's configuration
// into the native Pebble store. It reads the Python SQLite state read-only
// (from a copy, never the live file), writes only keys the Go side has not
// set yet, and never modifies or deletes any Python file. Credential
// values are never read: only vault reference names are listed in the
// report. Audio files and the task database are explicitly out of scope:
// the task database is sqlitemigrate's job, and audio stays where it is.
package configmigrate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/vault"

	_ "modernc.org/sqlite"
)

// Report is the migration receipt, also written to
// <profile>/state/migration-report.json.
type Report struct {
	At            time.Time `json:"at"`
	Profile       string    `json:"profile"`
	ConfigVersion int       `json:"config_version"`
	Mapped        []string  `json:"mapped"`
	VaultRefs     []string  `json:"vault_refs"`
	Skipped       []string  `json:"skipped"`
	Notes         []string  `json:"notes"`
}

// Keys written into the native store, in the exact JSON shape the Go
// settings readers expect.
var (
	downloadSettingsKey = []byte("app/settings/download")
	siteSettingsKey     = []byte("app/settings/sites")
	updatesSettingsKey  = []byte("app/settings/updates")
	qbtSettingsKey      = []byte("app/settings/qbittorrent")
	aiSettingsKey       = []byte("app/settings/ai")
	librarySettingsKey  = []byte("app/settings/library")
)

// knownSites is the display order of the four tracker sites, matching
// internal/app.
func knownSites() []string { return []string{"open.cd", "dicmusic", "orpheus", "redacted"} }

// pythonConfig is the stored config_versions payload: a version plus the
// values object from config/schema.py. Only the sections this migration
// maps are decoded; the rest is carried by reference in the notes.
type pythonConfig struct {
	Version int `json:"version"`
	Values  struct {
		Streaming struct {
			Enabled *bool `json:"enabled"`
		} `json:"streaming"`
		Providers map[string]struct {
			Enabled bool `json:"enabled"`
		} `json:"providers"`
		Updates struct {
			Enabled *bool `json:"enabled"`
		} `json:"updates"`
		Paths struct {
			Library string `json:"library"`
		} `json:"paths"`
		Offline  bool `json:"offline"`
		Workflow struct {
			SetupDone bool `json:"setup_done"`
		} `json:"workflow"`
		QBT struct {
			BaseURL   string `json:"base_url"`
			Category  string `json:"category"`
			TagPrefix string `json:"tag_prefix"`
		} `json:"qbt"`
		AI struct {
			Enabled         *bool    `json:"enabled"`
			BaseURL         string   `json:"base_url"`
			Model           string   `json:"model"`
			Timeout         *int     `json:"timeout"`
			MaxOutputTokens *int     `json:"max_output_tokens"`
			Temperature     *float64 `json:"temperature"`
		} `json:"ai"`
	} `json:"values"`
}

// MaybeMigrate migrates one Python profile into <profile>/state/native.
// Without a Python sqlite file it reports "no python profile". With a
// receipt whose config_version already matches the latest Python config
// it reports "already migrated". Any failure returns an error and writes
// no receipt, so the next run can retry.
func MaybeMigrate(profile string) (*Report, error) {
	abs, err := filepath.Abs(profile)
	if err != nil {
		return nil, err
	}
	report := &Report{
		At: time.Now().UTC(), Profile: abs,
		Mapped: []string{}, VaultRefs: []string{}, Skipped: []string{}, Notes: []string{},
	}
	sqlitePath := filepath.Join(abs, "state", "musicops.sqlite")
	if _, err := os.Stat(sqlitePath); errors.Is(err, os.ErrNotExist) {
		report.Skipped = []string{"no python profile"}
		return report, nil
	}
	cfg, err := readPythonConfig(sqlitePath)
	if err != nil {
		return nil, err
	}
	report.ConfigVersion = cfg.Version
	receiptPath := filepath.Join(abs, "state", "migration-report.json")
	if raw, err := os.ReadFile(receiptPath); err == nil {
		var old Report
		if err := json.Unmarshal(raw, &old); err == nil && old.ConfigVersion == cfg.Version && cfg.Version != 0 {
			report.Skipped = []string{"already migrated"}
			return report, nil
		}
	}
	// Vault references are names only. Values are never read: reading
	// them would leak credentials into the report and logs.
	v, err := vault.Open(abs)
	if err != nil {
		return nil, err
	}
	refs, err := v.ListRefs()
	if err != nil {
		return nil, fmt.Errorf("列出 vault 引用失败: %w", err)
	}
	for _, ref := range refs {
		report.VaultRefs = append(report.VaultRefs, ref.Ref)
	}
	engine, err := dataengine.Open(filepath.Join(abs, "state", "native"))
	if err != nil {
		return nil, err
	}
	defer engine.Close()
	nowNS := time.Now().UnixNano()
	streamingEnabled := false
	if cfg.Values.Streaming.Enabled != nil {
		streamingEnabled = *cfg.Values.Streaming.Enabled
	}
	download, err := json.Marshal(map[string]any{
		"streaming_enabled": streamingEnabled, "updated_ns": nowNS,
	})
	if err != nil {
		return nil, err
	}
	enabledSites := []string{}
	for _, site := range knownSites() {
		if p, ok := cfg.Values.Providers[site]; ok && p.Enabled {
			enabledSites = append(enabledSites, site)
		}
	}
	sites, err := json.Marshal(map[string]any{
		"enabled_sites": enabledSites, "updated_ns": nowNS,
	})
	if err != nil {
		return nil, err
	}
	updatesEnabled := true
	if cfg.Values.Updates.Enabled != nil {
		updatesEnabled = *cfg.Values.Updates.Enabled
	}
	updates, err := json.Marshal(map[string]any{
		"enabled": updatesEnabled, "last_check_ns": 0, "last_version": "",
	})
	if err != nil {
		return nil, err
	}
	// Write only absent keys: a Go setting the user already changed is
	// never overwritten by the old Python value. Secrets stay in the
	// vault; these documents are addresses and switches only.
	settings := []struct {
		key   []byte
		value []byte
	}{
		{downloadSettingsKey, download},
		{siteSettingsKey, sites},
		{updatesSettingsKey, updates},
	}
	if extra, ok, err := qbtSettings(cfg, nowNS); err != nil {
		return nil, err
	} else if ok {
		settings = append(settings, struct {
			key   []byte
			value []byte
		}{qbtSettingsKey, extra})
	}
	if extra, ok, err := aiSettings(cfg, nowNS); err != nil {
		return nil, err
	} else if ok {
		settings = append(settings, struct {
			key   []byte
			value []byte
		}{aiSettingsKey, extra})
	}
	if extra, ok, err := librarySettings(cfg, nowNS); err != nil {
		return nil, err
	} else if ok {
		settings = append(settings, struct {
			key   []byte
			value []byte
		}{librarySettingsKey, extra})
	}
	for _, kv := range settings {
		ok, err := engine.CompareAndSwap(
			[]dataengine.Condition{{Key: kv.key, Absent: true}},
			[]dataengine.Mutation{{Key: kv.key, Value: kv.value}},
		)
		if err != nil {
			return nil, err
		}
		if ok {
			report.Mapped = append(report.Mapped, string(kv.key))
		}
	}
	library := cfg.Values.Paths.Library
	if library == "" {
		library = "（未配置）"
	}
	report.Notes = append(report.Notes,
		fmt.Sprintf("Python 音乐库位于 %s；绝对路径会写入 Go 音乐库目录，本次不搬运音频文件，旧曲目索引不会自动变成新曲目", library),
		fmt.Sprintf("Python 配置 offline=%v，workflow.setup_done=%v", cfg.Values.Offline, cfg.Values.Workflow.SetupDone),
		"qBittorrent 地址、分类、标签和 AI 地址、模型已写入 Go 设置；密码和 API Key 仍只在 vault",
		"未映射的段（catalog、lyrics、音乐服务器等）的凭据引用仍在 vault 中，本次不迁移这些段",
		"本次不迁移音频文件与任务数据库；任务数据库迁移由 sqlitemigrate 负责",
	)
	if err := writeReceipt(receiptPath, report); err != nil {
		return nil, err
	}
	return report, nil
}

// readPythonConfig copies the Python sqlite file to a temporary directory
// and opens the copy read-only, so the live profile is never written to
// and a concurrent Python writer cannot corrupt the read.
func readPythonConfig(source string) (*pythonConfig, error) {
	dir, err := os.MkdirTemp("", "musicops-config-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "source.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(source + suffix); err != nil {
			if errors.Is(err, os.ErrNotExist) && suffix != "" {
				continue
			}
			return nil, err
		}
		if err := copyFile(source+suffix, copyPath+suffix); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(copyPath)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var payload string
	err = db.QueryRow("SELECT payload FROM config_versions ORDER BY CAST(id AS INTEGER) DESC LIMIT 1").Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("Python 配置不存在")
		}
		return nil, err
	}
	var cfg pythonConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		return nil, fmt.Errorf("Python 配置解析失败: %w", err)
	}
	if cfg.Version < 1 {
		return nil, errors.New("Python 配置版本无效")
	}
	return &cfg, nil
}

func qbtSettings(cfg *pythonConfig, nowNS int64) ([]byte, bool, error) {
	if strings.TrimSpace(cfg.Values.QBT.BaseURL) == "" {
		return nil, false, nil
	}
	encoded, err := json.Marshal(map[string]any{
		"base_url": cfg.Values.QBT.BaseURL, "category": cfg.Values.QBT.Category,
		"tags": cfg.Values.QBT.TagPrefix, "updated_ns": nowNS,
	})
	return encoded, err == nil, err
}

func aiSettings(cfg *pythonConfig, nowNS int64) ([]byte, bool, error) {
	ai := cfg.Values.AI
	if strings.TrimSpace(ai.BaseURL) == "" && strings.TrimSpace(ai.Model) == "" && ai.Enabled == nil {
		return nil, false, nil
	}
	enabled := false
	if ai.Enabled != nil {
		enabled = *ai.Enabled
	}
	body := map[string]any{
		"enabled": enabled, "base_url": ai.BaseURL, "model": ai.Model, "updated_ns": nowNS,
	}
	if ai.Timeout != nil {
		body["timeout"] = *ai.Timeout
	}
	if ai.MaxOutputTokens != nil {
		body["max_output_tokens"] = *ai.MaxOutputTokens
	}
	if ai.Temperature != nil {
		body["temperature"] = *ai.Temperature
	}
	encoded, err := json.Marshal(body)
	return encoded, err == nil, err
}

func librarySettings(cfg *pythonConfig, nowNS int64) ([]byte, bool, error) {
	library := strings.TrimSpace(cfg.Values.Paths.Library)
	if library == "" || !filepath.IsAbs(library) {
		return nil, false, nil
	}
	encoded, err := json.Marshal(map[string]any{"library_dir": library, "updated_ns": nowNS})
	return encoded, err == nil, err
}

func copyFile(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// writeReceipt writes the report atomically: temp file, fsync, rename.
func writeReceipt(path string, report *Report) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "migration-report-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
