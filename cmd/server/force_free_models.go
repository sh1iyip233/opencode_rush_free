package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type ForceFreeModel struct {
	ModelID   string    `json:"model_id"`
	CreatedAt time.Time `json:"created_at"`
}

func normalizeForceFreeModelID(raw string) (string, error) {
	modelID := strings.TrimSpace(raw)
	if modelID == "" {
		return "", errors.New("model_id is required")
	}
	if len(modelID) > 512 {
		return "", errors.New("model_id must be at most 512 bytes")
	}
	if strings.IndexFunc(modelID, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return "", errors.New("model_id cannot contain whitespace or control characters")
	}
	return modelID, nil
}

func (a *App) forceFreeModelSet(tx *sql.Tx) (map[string]struct{}, error) {
	rows, err := tx.Query("SELECT model_id FROM force_free_models")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	modelIDs := make(map[string]struct{})
	for rows.Next() {
		var modelID string
		if err := rows.Scan(&modelID); err != nil {
			return nil, err
		}
		modelIDs[modelID] = struct{}{}
	}
	return modelIDs, rows.Err()
}

func (a *App) listForceFreeModels(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query("SELECT model_id,created_at FROM force_free_models ORDER BY model_id")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not query model overrides"})
		return
	}
	defer rows.Close()

	items := []ForceFreeModel{}
	for rows.Next() {
		var item ForceFreeModel
		var createdAt string
		if err := rows.Scan(&item.ModelID, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read model overrides"})
			return
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read model overrides"})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *App) createForceFreeModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ModelID string `json:"model_id"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid model override"})
		return
	}
	modelID, err := normalizeForceFreeModelID(in.ModelID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create model override"})
		return
	}
	defer tx.Rollback()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.Exec("INSERT INTO force_free_models(model_id,created_at) VALUES(?,?)", modelID, createdAt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "model override already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create model override"})
		return
	}
	if _, err = tx.Exec("UPDATE models SET is_free=1,free_reason='manual_override' WHERE model_id=?", modelID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not apply model override"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create model override"})
		return
	}
	a.invalidateModelRuntimeCache()
	created, _ := time.Parse(time.RFC3339, createdAt)
	writeJSON(w, http.StatusCreated, ForceFreeModel{ModelID: modelID, CreatedAt: created})
}

func (a *App) deleteForceFreeModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ModelID string `json:"model_id"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid model override"})
		return
	}
	modelID, err := normalizeForceFreeModelID(in.ModelID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete model override"})
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec("DELETE FROM force_free_models WHERE model_id=?", modelID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete model override"})
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model override not found"})
		return
	}

	var raw string
	err = tx.QueryRow("SELECT COALESCE(raw_metadata,'{}') FROM models WHERE model_id=?", modelID).Scan(&raw)
	if err == nil {
		metadata := map[string]any{}
		_ = json.Unmarshal([]byte(raw), &metadata)
		isFree, reason := classifyFree(modelID, metadata)
		if _, err = tx.Exec("UPDATE models SET is_free=?,free_reason=? WHERE model_id=?", boolInt(isFree), reason, modelID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not restore automatic model classification"})
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load model metadata"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete model override"})
		return
	}
	a.invalidateModelRuntimeCache()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
