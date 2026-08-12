package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

const maintenanceModeSettingKey = "maintenance_mode"

type maintenanceSettings struct {
	Enabled bool `json:"enabled"`
}

func (a *App) maintenanceModeEnabled() (bool, error) {
	a.runtimeCaches.maintenance.mu.RLock()
	if a.runtimeCaches.maintenance.loaded {
		enabled := a.runtimeCaches.maintenance.enabled
		a.runtimeCaches.maintenance.mu.RUnlock()
		return enabled, nil
	}
	a.runtimeCaches.maintenance.mu.RUnlock()

	a.runtimeCaches.maintenance.mu.Lock()
	defer a.runtimeCaches.maintenance.mu.Unlock()
	if a.runtimeCaches.maintenance.loaded {
		return a.runtimeCaches.maintenance.enabled, nil
	}
	var raw string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key=?", maintenanceModeSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		a.runtimeCaches.maintenance.loaded = true
		a.runtimeCaches.maintenance.enabled = false
		return false, nil
	}
	if err != nil {
		return false, err
	}
	enabled := strings.EqualFold(strings.TrimSpace(raw), "true") || strings.TrimSpace(raw) == "1"
	a.runtimeCaches.maintenance.loaded = true
	a.runtimeCaches.maintenance.enabled = enabled
	return enabled, nil
}

func (a *App) getMaintenanceSettings(w http.ResponseWriter, _ *http.Request) {
	enabled, err := a.maintenanceModeEnabled()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load maintenance settings"})
		return
	}
	writeJSON(w, http.StatusOK, maintenanceSettings{Enabled: enabled})
}

func (a *App) putMaintenanceSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if readJSON(r, &in) != nil || in.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", maintenanceModeSettingKey, boolString(*in.Enabled)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save maintenance settings"})
		return
	}
	a.setCachedMaintenanceMode(*in.Enabled)
	writeJSON(w, http.StatusOK, maintenanceSettings{Enabled: *in.Enabled})
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
