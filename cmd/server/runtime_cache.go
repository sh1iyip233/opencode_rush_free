package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type upstreamRuntimeCache struct {
	mu     sync.RWMutex
	loaded bool
	cfg    upstreamConfig
}

type engineRuntimeCache struct {
	mu     sync.RWMutex
	loaded bool
	cfg    proxyEngineConfig
}

type maintenanceRuntimeCache struct {
	mu      sync.RWMutex
	loaded  bool
	enabled bool
}

type cachedClientKey struct {
	key           ClientKey
	lastTouchNano atomic.Int64
}

type clientKeyRuntimeCache struct {
	mu     sync.RWMutex
	loaded bool
	byHash map[string]*cachedClientKey
}

type modelRuntimeEntry struct {
	Record         ModelRecord
	ImageKnown     bool
	SupportsImages bool
}

type modelRuntimeCache struct {
	mu      sync.RWMutex
	loaded  bool
	models  map[string]modelRuntimeEntry
	aliases map[string]string
}

// compressionRuntimeCache holds the compression gateway settings. The cached
// value already carries the decrypted API key, so it is never logged.
type compressionRuntimeCache struct {
	mu     sync.RWMutex
	loaded bool
	cfg    compressionConfig
}

type runtimeCaches struct {
	upstream    upstreamRuntimeCache
	engine      engineRuntimeCache
	maintenance maintenanceRuntimeCache
	keys        clientKeyRuntimeCache
	models      modelRuntimeCache
	compression compressionRuntimeCache

	routingMu     sync.RWMutex
	routingLimit  int
	routingLoaded bool
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneUpstreamConfig(cfg upstreamConfig) upstreamConfig {
	cfg.LastRefresh = cloneTime(cfg.LastRefresh)
	return cfg
}

func cloneProxyEngineConfig(cfg proxyEngineConfig) proxyEngineConfig {
	cfg.LastCheckedAt = cloneTime(cfg.LastCheckedAt)
	cfg.FallbackSince = cloneTime(cfg.FallbackSince)
	return cfg
}

func (a *App) invalidateUpstreamCache() {
	a.runtimeCaches.upstream.mu.Lock()
	a.runtimeCaches.upstream.loaded = false
	a.runtimeCaches.upstream.cfg = upstreamConfig{}
	a.runtimeCaches.upstream.mu.Unlock()
}

func (a *App) invalidateProxyEngineCache() {
	a.runtimeCaches.engine.mu.Lock()
	a.runtimeCaches.engine.loaded = false
	a.runtimeCaches.engine.cfg = proxyEngineConfig{}
	a.runtimeCaches.engine.mu.Unlock()
}

func (a *App) setCachedMaintenanceMode(enabled bool) {
	a.runtimeCaches.maintenance.mu.Lock()
	a.runtimeCaches.maintenance.loaded = true
	a.runtimeCaches.maintenance.enabled = enabled
	a.runtimeCaches.maintenance.mu.Unlock()
}

func (a *App) invalidateClientKeyCache() {
	a.runtimeCaches.keys.mu.Lock()
	a.runtimeCaches.keys.loaded = false
	a.runtimeCaches.keys.byHash = nil
	a.runtimeCaches.keys.mu.Unlock()
}

func (a *App) invalidateModelRuntimeCache() {
	a.runtimeCaches.models.mu.Lock()
	a.runtimeCaches.models.loaded = false
	a.runtimeCaches.models.models = nil
	a.runtimeCaches.models.aliases = nil
	a.runtimeCaches.models.mu.Unlock()
}

func (a *App) invalidateCompressionCache() {
	a.runtimeCaches.compression.mu.Lock()
	a.runtimeCaches.compression.loaded = false
	a.runtimeCaches.compression.cfg = compressionConfig{}
	a.runtimeCaches.compression.mu.Unlock()
}

func (a *App) cachedRoutingLimit() int {
	a.runtimeCaches.routingMu.RLock()
	if a.runtimeCaches.routingLoaded {
		limit := a.runtimeCaches.routingLimit
		a.runtimeCaches.routingMu.RUnlock()
		return limit
	}
	a.runtimeCaches.routingMu.RUnlock()

	a.runtimeCaches.routingMu.Lock()
	defer a.runtimeCaches.routingMu.Unlock()
	if a.runtimeCaches.routingLoaded {
		return a.runtimeCaches.routingLimit
	}
	const defaultLimit = 50
	limit := defaultLimit
	var raw string
	if a.db.QueryRow("SELECT value FROM settings WHERE key='session_proxy_request_limit'").Scan(&raw) == nil {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 && parsed <= 100000 {
			limit = parsed
		}
	}
	a.runtimeCaches.routingLimit = limit
	a.runtimeCaches.routingLoaded = true
	return limit
}

func (a *App) invalidateRoutingLimit() {
	a.runtimeCaches.routingMu.Lock()
	a.runtimeCaches.routingLoaded = false
	a.runtimeCaches.routingLimit = 0
	a.runtimeCaches.routingMu.Unlock()
}

func (a *App) loadCachedClientKeys() (map[string]*cachedClientKey, error) {
	a.runtimeCaches.keys.mu.RLock()
	if a.runtimeCaches.keys.loaded {
		cached := a.runtimeCaches.keys.byHash
		a.runtimeCaches.keys.mu.RUnlock()
		return cached, nil
	}
	a.runtimeCaches.keys.mu.RUnlock()

	a.runtimeCaches.keys.mu.Lock()
	defer a.runtimeCaches.keys.mu.Unlock()
	if a.runtimeCaches.keys.loaded {
		return a.runtimeCaches.keys.byHash, nil
	}
	rows, err := a.db.Query("SELECT id,name,key_hash,key_hint,enabled,maintenance_allowed,COALESCE(expires_at,''),rpm_limit,tpm_limit,COALESCE(last_used_at,''),created_at FROM client_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byHash := make(map[string]*cachedClientKey)
	for rows.Next() {
		var key ClientKey
		var hash string
		var enabled, maintenanceAllowed int
		var expires, lastUsed, created string
		if err := rows.Scan(&key.ID, &key.Name, &hash, &key.Hint, &enabled, &maintenanceAllowed, &expires, &key.RPMLimit, &key.TPMLimit, &lastUsed, &created); err != nil {
			return nil, err
		}
		key.Enabled = enabled == 1
		key.MaintenanceAllowed = maintenanceAllowed == 1
		key.ExpiresAt = parseStoredTime(expires)
		key.LastUsedAt = parseStoredTime(lastUsed)
		key.CreatedAt, _ = time.Parse(time.RFC3339, created)
		cached := &cachedClientKey{key: key}
		cached.key.lastTouch = &cached.lastTouchNano
		if key.LastUsedAt != nil {
			cached.lastTouchNano.Store(key.LastUsedAt.UnixNano())
		}
		byHash[hash] = cached
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	a.runtimeCaches.keys.byHash = byHash
	a.runtimeCaches.keys.loaded = true
	return byHash, nil
}

func (a *App) cachedClientKey(hash string) (*cachedClientKey, error) {
	byHash, err := a.loadCachedClientKeys()
	if err != nil {
		return nil, err
	}
	return byHash[hash], nil
}

func (a *App) loadModelRuntime() (map[string]modelRuntimeEntry, map[string]string, error) {
	a.runtimeCaches.models.mu.RLock()
	if a.runtimeCaches.models.loaded {
		models, aliases := a.runtimeCaches.models.models, a.runtimeCaches.models.aliases
		a.runtimeCaches.models.mu.RUnlock()
		return models, aliases, nil
	}
	a.runtimeCaches.models.mu.RUnlock()

	a.runtimeCaches.models.mu.Lock()
	defer a.runtimeCaches.models.mu.Unlock()
	if a.runtimeCaches.models.loaded {
		return a.runtimeCaches.models.models, a.runtimeCaches.models.aliases, nil
	}
	rows, err := a.db.Query("SELECT m.id,m.model_id,COALESCE(m.display_name,''),m.is_free,COALESCE(m.free_reason,''),COALESCE(m.pricing_metadata,''),COALESCE(m.raw_metadata,''),COALESCE(p.enabled,1),m.refreshed_at FROM models m LEFT JOIN model_policies p ON p.model_id=m.model_id WHERE m.is_free=1 ORDER BY m.model_id")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	models := make(map[string]modelRuntimeEntry)
	for rows.Next() {
		var model ModelRecord
		var free, enabled int
		var pricing, raw, refreshed string
		if err := rows.Scan(&model.ID, &model.ModelID, &model.DisplayName, &free, &model.FreeReason, &pricing, &raw, &enabled, &refreshed); err != nil {
			return nil, nil, err
		}
		model.IsFree = free == 1
		model.AdminEnabled = enabled == 1
		model.Pricing = json.RawMessage(pricing)
		model.Raw = json.RawMessage(raw)
		model.RefreshedAt, _ = time.Parse(time.RFC3339, refreshed)
		known, supported := modelImageSupport([]byte(raw))
		models[model.ModelID] = modelRuntimeEntry{Record: model, ImageKnown: known, SupportsImages: supported}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	aliasesRows, err := a.db.Query("SELECT a.alias,a.target_model_id FROM model_aliases a WHERE a.enabled=1")
	if err != nil {
		return nil, nil, err
	}
	defer aliasesRows.Close()
	aliases := make(map[string]string)
	for aliasesRows.Next() {
		var alias, target string
		if err := aliasesRows.Scan(&alias, &target); err != nil {
			return nil, nil, err
		}
		aliases[alias] = target
	}
	if err := aliasesRows.Err(); err != nil {
		return nil, nil, err
	}
	a.runtimeCaches.models.models = models
	a.runtimeCaches.models.aliases = aliases
	a.runtimeCaches.models.loaded = true
	return models, aliases, nil
}

func modelRuntimeList(models map[string]modelRuntimeEntry) []modelRuntimeEntry {
	out := make([]modelRuntimeEntry, 0, len(models))
	for _, item := range models {
		if item.Record.AdminEnabled {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.ModelID < out[j].Record.ModelID })
	return out
}
