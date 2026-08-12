package main

import (
	"net/http"
	"sort"
	"time"
)

const publicStatusWindowHours = 24

type publicStatusBucket struct {
	Start       time.Time `json:"start"`
	Requests    int64     `json:"requests"`
	Success     int64     `json:"success"`
	External    int64     `json:"external_errors"`
	SuccessRate *float64  `json:"success_rate"`
	Status      string    `json:"status"`
}

type publicModelStatus struct {
	ModelID                string               `json:"model_id"`
	DisplayName            string               `json:"display_name"`
	AdminEnabled           bool                 `json:"admin_enabled"`
	Requests24h            int64                `json:"requests_24h"`
	RecentRequests15m      int64                `json:"recent_requests_15m"`
	Success24h             int64                `json:"success_24h"`
	ExternalErrors24h      int64                `json:"external_errors_24h"`
	SuccessRate            *float64             `json:"success_rate"`
	AvgLatencyMS           int64                `json:"avg_latency_ms"`
	AvgFirstTokenLatencyMS int64                `json:"avg_first_token_latency_ms"`
	Status                 string               `json:"status"`
	Buckets                []publicStatusBucket `json:"buckets"`
}

type publicStatusSummary struct {
	Models            int      `json:"models"`
	Requests24h       int64    `json:"requests_24h"`
	RecentRequests15m int64    `json:"recent_requests_15m"`
	SuccessRate       *float64 `json:"success_rate"`
	Status            string   `json:"status"`
}

type publicStatusAggregate struct {
	Requests     int64
	Success      int64
	External     int64
	LatencySum   int64
	LatencyCount int64
	FirstSum     int64
	FirstCount   int64
}

func (a *App) publicStatus(w http.ResponseWriter, _ *http.Request) {
	maintenanceMode, err := a.maintenanceModeEnabled()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load public status"})
		return
	}
	now := time.Now().UTC()
	currentHour := chinaHourStart(now)
	windowStart := currentHour.Add(-time.Duration(publicStatusWindowHours-1) * time.Hour)

	rows, err := a.db.Query("SELECT m.model_id,COALESCE(m.display_name,''),COALESCE(p.enabled,1) FROM models m LEFT JOIN model_policies p ON p.model_id=m.model_id WHERE m.is_free=1 ORDER BY m.model_id")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not query public model status"})
		return
	}
	type modelEntry struct {
		modelID     string
		displayName string
		enabled     bool
		aggregate   publicStatusAggregate
		buckets     [publicStatusWindowHours]publicStatusAggregate
		recent15m   int64
	}
	models := []modelEntry{}
	modelIndex := map[string]int{}
	for rows.Next() {
		var modelID, displayName string
		var enabled int
		if err := rows.Scan(&modelID, &displayName, &enabled); err != nil {
			_ = rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read public model status"})
			return
		}
		if displayName == "" {
			displayName = modelID
		}
		modelIndex[modelID] = len(models)
		models = append(models, modelEntry{modelID: modelID, displayName: displayName, enabled: enabled == 1})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read public model status"})
		return
	}
	_ = rows.Close()

	query := "SELECT model,created_at,status,COALESCE(NULLIF(error_origin,''),'user'),latency_ms,first_token_latency_ms FROM usage_requests WHERE request_kind='chat' AND created_at>=? AND created_at<=?"
	usageRows, err := a.db.Query(query, windowStart.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not query public model status"})
		return
	}
	for usageRows.Next() {
		var modelID, createdAt, status, origin string
		var latency int64
		var first *int64
		if err := usageRows.Scan(&modelID, &createdAt, &status, &origin, &latency, &first); err != nil {
			_ = usageRows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read public model status"})
			return
		}
		index, ok := modelIndex[modelID]
		if !ok {
			continue
		}
		created, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			continue
		}
		bucketStart := chinaHourStart(created)
		bucketIndex := int(bucketStart.Sub(windowStart) / time.Hour)
		if bucketIndex < 0 || bucketIndex >= publicStatusWindowHours {
			continue
		}
		entry := &models[index]
		addPublicStatusSample(&entry.aggregate, status, origin, latency, first)
		addPublicStatusSample(&entry.buckets[bucketIndex], status, origin, latency, first)
		if !created.Before(now.Add(-15 * time.Minute)) {
			entry.recent15m++
		}
	}
	if err := usageRows.Err(); err != nil {
		_ = usageRows.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read public model status"})
		return
	}
	_ = usageRows.Close()

	output := make([]publicModelStatus, 0, len(models))
	var summaryAggregate publicStatusAggregate
	var recent15m int64
	for _, model := range models {
		status, successRate := publicAvailability(model.aggregate)
		buckets := make([]publicStatusBucket, publicStatusWindowHours)
		for i := range buckets {
			bucketStatus, bucketRate := publicAvailability(model.buckets[i])
			buckets[i] = publicStatusBucket{
				Start:       windowStart.Add(time.Duration(i) * time.Hour),
				Requests:    model.buckets[i].Requests,
				Success:     model.buckets[i].Success,
				External:    model.buckets[i].External,
				SuccessRate: bucketRate,
				Status:      bucketStatus,
			}
		}
		output = append(output, publicModelStatus{
			ModelID:                model.modelID,
			DisplayName:            model.displayName,
			AdminEnabled:           model.enabled,
			Requests24h:            model.aggregate.Requests,
			RecentRequests15m:      model.recent15m,
			Success24h:             model.aggregate.Success,
			ExternalErrors24h:      model.aggregate.External,
			SuccessRate:            successRate,
			AvgLatencyMS:           publicAverage(model.aggregate.LatencySum, model.aggregate.LatencyCount),
			AvgFirstTokenLatencyMS: publicAverage(model.aggregate.FirstSum, model.aggregate.FirstCount),
			Status:                 status,
			Buckets:                buckets,
		})
		summaryAggregate.Requests += model.aggregate.Requests
		summaryAggregate.Success += model.aggregate.Success
		summaryAggregate.External += model.aggregate.External
		recent15m += model.recent15m
	}
	summaryStatus, summaryRate := publicAvailability(summaryAggregate)
	if len(output) > 1 {
		sort.Slice(output, func(i, j int) bool { return output[i].ModelID < output[j].ModelID })
	}
	w.Header().Set("Cache-Control", "public, max-age=30, stale-while-revalidate=30")
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at":     now,
		"maintenance_mode": maintenanceMode,
		"timezone":         "Asia/Shanghai",
		"window_start":     windowStart,
		"window_end":       now,
		"bucket_hours":     publicStatusWindowHours,
		"summary": publicStatusSummary{
			Models:            len(output),
			Requests24h:       summaryAggregate.Requests,
			RecentRequests15m: recent15m,
			SuccessRate:       summaryRate,
			Status:            summaryStatus,
		},
		"models": output,
	})
}

func chinaHourStart(value time.Time) time.Time {
	local := value.In(chinaLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, chinaLocation).UTC()
}

func addPublicStatusSample(aggregate *publicStatusAggregate, status, origin string, latency int64, first *int64) {
	aggregate.Requests++
	if status == "success" {
		aggregate.Success++
	}
	if origin == "external" {
		aggregate.External++
	}
	if latency > 0 {
		aggregate.LatencySum += latency
		aggregate.LatencyCount++
	}
	if first != nil && *first > 0 {
		aggregate.FirstSum += *first
		aggregate.FirstCount++
	}
}

func publicAvailability(aggregate publicStatusAggregate) (string, *float64) {
	counted := aggregate.Requests - aggregate.External
	if counted <= 0 {
		return "no_request", nil
	}
	rate := float64(aggregate.Success) / float64(counted)
	status := "outage"
	if rate >= 0.95 {
		status = "available"
	} else if rate >= 0.80 {
		status = "degraded"
	}
	return status, &rate
}

func publicAverage(sum, count int64) int64 {
	if count <= 0 {
		return 0
	}
	return (sum + count/2) / count
}
